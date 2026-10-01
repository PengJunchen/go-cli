package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
)

func TestAssembledSubAgentModelOverrideUsesPrimaryResolverScope(t *testing.T) {
	primaryCapture := &providerRequestCapture{}
	primaryServer := httptest.NewServer(http.HandlerFunc(primaryCapture.handler))
	t.Cleanup(primaryServer.Close)
	smallCapture := &providerRequestCapture{}
	smallServer := httptest.NewServer(http.HandlerFunc(smallCapture.handler))
	t.Cleanup(smallServer.Close)

	const primaryProvider = "r3-override-primary"
	const smallProvider = "r3-override-small"
	primaryKey := newTestCredentialCanary(t)
	smallKey := newTestCredentialCanary(t)
	cfg := newAssembleTestConfig(t)
	cfg.Provider = config.ProviderConfig{
		Name:    primaryProvider,
		BaseURL: primaryServer.URL + "/primary",
		APIKey:  primaryKey,
	}
	cfg.SmallModel = config.SmallModelConfig{
		Provider: smallProvider,
		BaseURL:  smallServer.URL + "/small",
		APIKey:   smallKey,
		Model:    "configured-small-model",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	assembly, err := AssembleAgent(ctx, cfg, primaryProvider, "assembled-parent-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	result, err := assembly.Dispatcher.Dispatch(ctx, core.SubagentTask{
		ID:           "r3-override-child",
		Prompt:       "answer using the sub-agent model override",
		SystemPrompt: "r3 assembled override fixture",
		Model:        "unregistered-child-model-name",
		MaxTurns:     1,
	})
	require.NoError(t, err)
	require.NoError(t, result.Error)
	assert.Equal(t, "ok", result.Content)

	count, path, authorization, body := primaryCapture.snapshot()
	assert.Equal(t, 1, count)
	assert.Equal(t, "/primary/chat/completions", path)
	assert.Equal(t, "Bearer "+primaryKey, authorization)
	assert.Contains(t, body, `"model":"unregistered-child-model-name"`)
	assert.NotContains(t, body, primaryKey)
	assert.NotContains(t, body, smallKey)

	smallCount, _, smallAuthorization, smallBody := smallCapture.snapshot()
	assert.Zero(t, smallCount, "a bare child model override stays on the Assembly primary provider")
	assert.NotContains(t, smallAuthorization, primaryKey)
	assert.NotContains(t, smallAuthorization, smallKey)
	assert.NotContains(t, smallBody, primaryKey)
	assert.NotContains(t, smallBody, smallKey)
}
