//go:build e2e

package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

// TestET_model_cycler_rotation exercises resolver-guarded cycler candidates
// through Core so provider requests receive the current operation authority.
func TestET_model_cycler_rotation(t *testing.T) {
	t.Run("round_robin_rotation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		server, requests := newCyclerE2EServer(t, nil)
		resolver := newCyclerE2EResolver(t, server, "cycler-gpt", "cycler-claude", "cycler-gemini", "cycler-primary")
		entries := []llm.ModelEntry{
			{Provider: "cycler-gpt", Model: "gpt-4o"},
			{Provider: "cycler-claude", Model: "claude-3"},
			{Provider: "cycler-gemini", Model: "gemini-pro"},
		}
		resolved := resolveCyclerE2EEntries(t, resolver, server.URL, entries)
		primary := resolveCyclerE2EModel(t, resolver, server.URL, "cycler-primary", "primary")
		loop := newCyclerE2ELoop(t, entries, resolved, primary, false)

		expected := []string{"gpt-4o", "claude-3", "gemini-pro", "gpt-4o"}
		for i, want := range expected {
			_, err := loop.Run(ctx, core.Submission{Content: "cycle"})
			require.NoError(t, err)
			assert.Equal(t, expected[:i+1], requests(), "request sequence after call %d", i)
			assert.Equal(t, want, requests()[i], "call %d should select model %q", i, want)
		}
	})

	t.Run("session_affinity", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		server, requests := newCyclerE2EServer(t, nil)
		resolver := newCyclerE2EResolver(t, server, "cycler-gpt", "cycler-claude", "cycler-gemini", "cycler-primary")
		entries := []llm.ModelEntry{
			{Provider: "cycler-gpt", Model: "gpt-4o"},
			{Provider: "cycler-claude", Model: "claude-3"},
			{Provider: "cycler-gemini", Model: "gemini-pro"},
		}
		resolved := resolveCyclerE2EEntries(t, resolver, server.URL, entries)
		primary := resolveCyclerE2EModel(t, resolver, server.URL, "cycler-primary", "primary")
		loop := newCyclerE2ELoop(t, entries, resolved, primary, true)

		calls := []struct {
			ctx  context.Context
			want string
		}{
			{llm.WithSessionID(ctx, "session-A"), "gpt-4o"},
			{llm.WithSessionID(ctx, "session-A"), "gpt-4o"},
			{llm.WithSessionID(ctx, "session-B"), "claude-3"},
			{llm.WithSessionID(ctx, "session-B"), "claude-3"},
		}
		for i, call := range calls {
			_, err := loop.Run(call.ctx, core.Submission{Content: "cycle"})
			require.NoError(t, err)
			assert.Equal(t, call.want, requests()[i], "call %d should preserve session routing", i)
		}
	})

	t.Run("fallback_on_error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		server, requests := newCyclerE2EServer(t, map[string]bool{"selected-model": true})
		resolver := newCyclerE2EResolver(t, server, "cycler-selected", "cycler-primary")
		entries := []llm.ModelEntry{{Provider: "cycler-selected", Model: "selected-model"}}
		resolved := resolveCyclerE2EEntries(t, resolver, server.URL, entries)
		primary := resolveCyclerE2EModel(t, resolver, server.URL, "cycler-primary", "primary-model")
		loop := newCyclerE2ELoop(t, entries, resolved, primary, false)

		_, err := loop.Run(ctx, core.Submission{Content: "cycle"})
		require.NoError(t, err)
		assert.Equal(t, []string{"selected-model", "primary-model"}, requests(),
			"Core should own and authorize the primary fallback after selected model failure")
	})
}

func newCyclerE2EServer(t *testing.T, failures map[string]bool) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var models []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		models = append(models, request.Model)
		mu.Unlock()
		if failures[request.Model] {
			http.Error(w, "temporary provider failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", request.Model)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), models...)
	}
}

func newCyclerE2EResolver(t *testing.T, server *httptest.Server, providers ...string) *llm.ModelResolver {
	t.Helper()
	registry := llm.NewProviderRegistry()
	for _, providerID := range providers {
		require.NoError(t, registry.Register(llm.NewEinoProvider(
			llm.WithProviderName(providerID),
			llm.WithHTTPClient(server.Client()),
		)))
	}
	return registry.SnapshotResolver()
}

func resolveCyclerE2EEntries(t *testing.T, resolver *llm.ModelResolver, endpoint string, entries []llm.ModelEntry) []llm.ResolvedModel {
	t.Helper()
	resolved := make([]llm.ResolvedModel, 0, len(entries))
	for _, entry := range entries {
		resolved = append(resolved, resolveCyclerE2EModel(t, resolver, endpoint, entry.Provider, entry.Model))
	}
	return resolved
}

func resolveCyclerE2EModel(t *testing.T, resolver *llm.ModelResolver, endpoint, providerID, modelID string) llm.ResolvedModel {
	t.Helper()
	resolved, err := resolver.ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: providerID,
		ModelID:    modelID,
		Endpoint:   endpoint,
	})
	require.NoError(t, err)
	return resolved
}

func newCyclerE2ELoop(t *testing.T, entries []llm.ModelEntry, resolved []llm.ResolvedModel, primary llm.ResolvedModel, sessionAffinity bool) *core.LoopAgent {
	t.Helper()
	cycler := llm.NewModelCycler(llm.ModelCyclerConfig{
		Models:          entries,
		Strategy:        llm.StrategyRoundRobin,
		SessionAffinity: sessionAffinity,
	}).WithResolvedModels(resolved)
	loop := core.NewLoopAgent(
		core.WithLLM(cycler.WrapModel(buildCyclerE2EModel(t, primary))),
		core.WithMaxIterations(1),
	)
	loop.SetMidTurnCompaction(compaction.NewMidTurnCompact(), nil, compaction.NewHeuristicTokenEstimator(), 8000)
	return loop
}

func buildCyclerE2EModel(t *testing.T, resolved llm.ResolvedModel) llm.BaseChatModel {
	t.Helper()
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return model
}
