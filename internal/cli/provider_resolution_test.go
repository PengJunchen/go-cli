package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const providerCredentialCanary = "provider-canary-do-not-forward"

type providerRequestCapture struct {
	mu               sync.Mutex
	count            int
	path             string
	authorization    string
	body             string
	responseStatuses []int
	requests         []capturedProviderRequest
}

type capturedProviderRequest struct {
	path          string
	authorization string
	body          string
}

func (c *providerRequestCapture) handler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request", http.StatusBadRequest)
		return
	}

	c.mu.Lock()
	c.count++
	requestNumber := c.count
	c.path = r.URL.Path
	c.authorization = r.Header.Get("Authorization")
	c.body = string(body)
	c.requests = append(c.requests, capturedProviderRequest{
		path:          r.URL.Path,
		authorization: r.Header.Get("Authorization"),
		body:          string(body),
	})
	responseStatus := http.StatusOK
	if requestNumber <= len(c.responseStatuses) {
		responseStatus = c.responseStatuses[requestNumber-1]
	}
	c.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if responseStatus != http.StatusOK {
		w.WriteHeader(responseStatus)
		if _, err := io.WriteString(w, `{"error":{"message":"fixture transient error","type":"server_error"}}`); err != nil {
			return
		}
		return
	}
	if _, err := io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`); err != nil {
		return
	}
}

func (c *providerRequestCapture) snapshot() (count int, path, authorization, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count, c.path, c.authorization, c.body
}

func (c *providerRequestCapture) snapshotRequests() []capturedProviderRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedProviderRequest(nil), c.requests...)
}

func TestProviderCredentialNeverSentToWrongOrigin(t *testing.T) {
	for _, providerName := range []string{"claude", "anthropic", "gemini", "Claude"} {
		t.Run(providerName+" explicit endpoint", func(t *testing.T) {
			capture := &providerRequestCapture{}
			srv := httptest.NewServer(http.HandlerFunc(capture.handler))
			t.Cleanup(srv.Close)

			rc := &config.Config{Provider: config.ProviderConfig{
				BaseURL: srv.URL,
				APIKey:  providerCredentialCanary,
			}}
			model, _, cleanup, err := buildModel(context.Background(), rc, providerName, providerName+"-test-model")
			if cleanup != nil {
				defer cleanup()
			}
			if err == nil {
				_, requestErr := model.Generate(withCLITestOperation(context.Background()), []llm.Message{{
					Role:    llm.RoleUser,
					Content: "do not forward this credential",
				}})
				assert.NoError(t, requestErr)
			}

			assert.Error(t, err, "the CLI must reject native provider identities until their protocol adapters are enabled")
			assert.ErrorContains(t, err, "custom provider name")
			count, _, authorization, body := capture.snapshot()
			assert.Zero(t, count, "rejected provider configuration must not contact its configured origin")
			assert.NotContains(t, authorization, providerCredentialCanary)
			assert.NotContains(t, body, providerCredentialCanary)
		})

		t.Run(providerName+" API key without endpoint", func(t *testing.T) {
			rc := &config.Config{Provider: config.ProviderConfig{APIKey: providerCredentialCanary}}
			model, _, cleanup, err := buildModel(context.Background(), rc, providerName, providerName+"-test-model")
			if cleanup != nil {
				defer cleanup()
			}
			assert.Error(t, err, "a native provider credential must not create an OpenAI-compatible model with the default OpenAI origin")
			assert.Nil(t, model)
		})
	}

	t.Run("unknown provider API key without endpoint is rejected", func(t *testing.T) {
		rc := &config.Config{Provider: config.ProviderConfig{APIKey: providerCredentialCanary}}
		model, _, cleanup, err := buildModel(context.Background(), rc, "unknown-provider", "test-model")
		if cleanup != nil {
			defer cleanup()
		}
		assert.Error(t, err)
		assert.Nil(t, model)
	})

	t.Run("unknown provider without configuration is rejected", func(t *testing.T) {
		model, _, cleanup, err := buildModel(context.Background(), nil, "unknown-provider", "test-model")
		if cleanup != nil {
			defer cleanup()
		}
		assert.Error(t, err)
		assert.Nil(t, model)
	})
}

func TestProviderCustomOpenAICompatibleAliasUsesConfiguredOrigin(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	rc := &config.Config{Provider: config.ProviderConfig{
		Name:    "custom-gateway",
		BaseURL: srv.URL,
		APIKey:  providerCredentialCanary,
	}}
	model, _, cleanup, err := buildModel(context.Background(), rc, "custom-gateway", "custom-test-model")
	require.NoError(t, err)
	require.NotNil(t, model)
	if cleanup != nil {
		defer cleanup()
	}

	response, err := model.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "hello"}})
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, "ok", response.Content)

	count, path, authorization, body := capture.snapshot()
	assert.Equal(t, 1, count)
	assert.Equal(t, "/chat/completions", path)
	assert.Equal(t, "Bearer "+providerCredentialCanary, authorization)
	assert.True(t, strings.Contains(body, `"model":"custom-test-model"`))
}

func TestProviderOpenAIIdentityAllowsAPIKeyOnlyConfiguration(t *testing.T) {
	rc := &config.Config{Provider: config.ProviderConfig{Name: "openai", APIKey: providerCredentialCanary}}
	model, _, cleanup, err := buildModel(context.Background(), rc, "openai", "openai-test-model")
	require.NoError(t, err)
	require.NotNil(t, model)
	if cleanup != nil {
		defer cleanup()
	}
}

func TestCLIProviderListingFailsClosedForNativeProvider(t *testing.T) {
	rc := &config.Config{Provider: config.ProviderConfig{
		BaseURL: "http://127.0.0.1:1",
		APIKey:  providerCredentialCanary,
	}}
	assert.Empty(t, listAvailableModels(rc, "gemini"))
}
