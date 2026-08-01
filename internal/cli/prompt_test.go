package cli

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// writePromptResponse writes body to an httptest response and fails the test
// if the write errors. It satisfies errcheck in the test handler goroutines.
func writePromptResponse(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write response: %v", err)
	}
}

// newPromptConfig builds a *config.Config pointing at the given base URL so the
// prompt command builds a custom provider backed by the httptest endpoint.
func newPromptConfig(baseURL string) *config.Config {
	return &config.Config{
		Provider: config.ProviderConfig{
			Name:    "test",
			BaseURL: baseURL,
			APIKey:  "test",
			Model:   "test-model",
		},
	}
}

// TestPromptCmd_RunCannedCompletion wires a prompt command to an httptest
// OpenAI-compatible endpoint and verifies the agent's response is streamed to
// the output writer.
func TestPromptCmd_RunCannedCompletion(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/chat/completions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		writePromptResponse(t, w, `{
			"choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":3,"completion_tokens":1}
		}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	cmd := newPromptCmd(&out)
	cfg := newPromptConfig(srv.URL)

	err := cmd.Run(t.Context(), cfg, []string{"hello"})
	require.NoError(t, err)
	assert.Equal(t, "ok\n", out.String())
}

// TestPromptCmd_RunFlagsOverridesConfig verifies the -model and -provider flags
// override the loaded configuration while still reaching the httptest endpoint.
func TestPromptCmd_RunFlagsOverridesConfig(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, rerr := io.ReadAll(r.Body)
		require.NoError(t, rerr)
		assert.Contains(t, string(body), `"model":"flagged-model"`)
		w.Header().Set("Content-Type", "application/json")
		writePromptResponse(t, w, `{"choices":[{"message":{"content":"hello there"}}]}`)
	}))
	defer srv.Close()

	var out bytes.Buffer
	cmd := newPromptCmd(&out)
	cfg := newPromptConfig(srv.URL)

	err := cmd.Run(t.Context(), cfg, []string{"-model", "flagged-model", "-provider", "test", "-verbose", "hello"})
	require.NoError(t, err)
	assert.Equal(t, "hello there\n", out.String())
}

// TestPromptCmd_RunUnknownFlag verifies an unknown flag produces a UsageError.
func TestPromptCmd_RunUnknownFlag(t *testing.T) {
	var out bytes.Buffer
	cmd := newPromptCmd(&out)

	err := cmd.Run(t.Context(), newPromptConfig("ignored"), []string{"-bogus", "hello"})
	require.Error(t, err)
	var usageErr *UsageError
	assert.True(t, errors.As(err, &usageErr))
	assert.True(t, strings.Contains(err.Error(), "bogus"))
}

// TestPromptCmd_RunMissingMessage verifies a missing prompt argument produces a
// UsageError without attempting any network call.
func TestPromptCmd_RunMissingMessage(t *testing.T) {
	var out bytes.Buffer
	cmd := newPromptCmd(&out)

	err := cmd.Run(t.Context(), newPromptConfig("ignored"), nil)
	require.Error(t, err)
	var usageErr *UsageError
	assert.True(t, errors.As(err, &usageErr))
	assert.Contains(t, err.Error(), "missing message argument")
}

// TestPromptCmd_Metadata verifies the prompt command's registration metadata.
func TestPromptCmd_Metadata(t *testing.T) {
	cmd := newPromptCmd(&bytes.Buffer{})
	assert.Equal(t, "prompt", cmd.Name())
	assert.NotEmpty(t, cmd.Synopsis())
}
