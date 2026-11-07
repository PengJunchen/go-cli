package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
)

func memoryEnabledPointer(value bool) *bool { return &value }

func TestAutomaticMemoryUsesBoundedConfiguredTokenProfile(t *testing.T) {
	intPtr := func(v int) *int { return &v }
	tests := []struct {
		name          string
		modelMax      *int
		providerMax   *int
		smallMax      *int
		smallProvider string
		useSmall      bool
		useCycler     bool
		wantMax       int
	}{
		{name: "explicit primary max is capped", modelMax: intPtr(16384), wantMax: 1024},
		{name: "omitted defaults", wantMax: 1024},
		{name: "smaller explicit primary limit", modelMax: intPtr(128), wantMax: 128},
		{name: "explicit primary zero masks provider", modelMax: intPtr(0), providerMax: intPtr(512), wantMax: 1024},
		{name: "matching provider limit is capped", providerMax: intPtr(2048), wantMax: 1024},
		{name: "matching provider limit below cap", providerMax: intPtr(512), wantMax: 512},
		{name: "small model does not inherit another provider", modelMax: intPtr(16384), providerMax: intPtr(512), useSmall: true, smallProvider: "memory-test-other", wantMax: 1024},
		{name: "small model inherits matching provider", providerMax: intPtr(512), useSmall: true, wantMax: 512},
		{name: "small model explicit max is capped", modelMax: intPtr(16384), providerMax: intPtr(512), smallMax: intPtr(8192), useSmall: true, smallProvider: "memory-test-other", wantMax: 1024},
		{name: "small model explicit zero masks provider", providerMax: intPtr(512), smallMax: intPtr(0), useSmall: true, wantMax: 1024},
		{name: "dynamic cycler retains legacy profile", modelMax: intPtr(16384), useCycler: true, wantMax: 1024},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			configureDurableCLISessionTest(t)
			workspace, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			t.Chdir(workspace)
			extractions := make(chan map[string]any, 1)
			extractionReplies := make(chan bool, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					t.Errorf("read fixture request: %v", readErr)
					return
				}
				content := "completed"
				isExtraction := bytes.Contains(body, []byte("Analyze the following conversation"))
				if isExtraction {
					var request map[string]any
					if decodeErr := json.Unmarshal(body, &request); decodeErr != nil {
						t.Errorf("decode extraction request: %v", decodeErr)
						return
					}
					extractions <- request
					content = "[]"
				}
				w.Header().Set("Content-Type", "application/json")
				if encodeErr := json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
					"message": map[string]any{"role": "assistant", "content": content},
				}}}); encodeErr != nil {
					t.Errorf("write fixture response: %v", encodeErr)
					return
				}
				if isExtraction {
					extractionReplies <- true
				}
			}))
			defer server.Close()

			provider := map[string]any{"name": "openai", "base_url": server.URL, "api_key": newTestCredentialCanary(t)}
			model := map[string]any{"name": "primary-fixture"}
			small := map[string]any{}
			if tc.modelMax != nil {
				model["max_tokens"] = *tc.modelMax
			}
			if tc.providerMax != nil {
				provider["max_tokens"] = *tc.providerMax
			}
			if tc.useSmall {
				small["model"] = "small-fixture"
				if tc.smallProvider != "" {
					small["provider"] = tc.smallProvider
					small["base_url"] = server.URL
				}
				if tc.smallMax != nil {
					small["max_tokens"] = *tc.smallMax
				}
			}
			document := map[string]any{"provider": provider, "model": model, "small_model": small}
			if tc.useCycler {
				document["model_cycler"] = map[string]any{"enabled": true, "strategy": "round_robin",
					"models": []any{map[string]any{"provider": "openai", "model": "cycle-fixture"}}}
			}
			doc, err := json.Marshal(document)
			require.NoError(t, err)
			cfg, err := config.NewLoader().WithFileBytes("fixture.json", doc).
				WithEnvPrefix("MEMORY_PROFILE_TEST_UNUSED").Load(t.Context())
			require.NoError(t, err)
			cfg.Memory.Enabled = memoryEnabledPointer(true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			assembly, err := AssembleAgent(ctx, cfg, "openai", "primary-fixture", io.Discard)
			require.NoError(t, err)
			t.Cleanup(assembly.Cleanup)
			if tc.useSmall {
				require.NotNil(t, assembly.ModelConfig.ModelSelector.SmallModel())
			}
			assembly.RefreshMemoryContext(ctx)
			result, err := assembly.TurnRunner.RunTurn(ctx, core.Submission{Content: "remember this fixture"})
			require.NoError(t, err)
			assembly.RecordCompletedMemory(ctx, result, nil, "remember this fixture")
			select {
			case request := <-extractions:
				require.Equal(t, float64(tc.wantMax), request["max_tokens"])
				require.NotContains(t, request, "reasoning_effort", "memory's bounded extractor keeps its existing no-added-thinking policy")
				wantModel := "primary-fixture"
				if tc.useSmall {
					wantModel = "small-fixture"
				} else if tc.useCycler {
					wantModel = "cycle-fixture"
				}
				require.Equal(t, wantModel, request["model"])
			case <-time.After(3 * time.Second):
				t.Fatal("automatic memory extraction did not reach the fixture")
			}
			select {
			case accepted := <-extractionReplies:
				require.True(t, accepted, "fixture must accept the extraction profile")
			case <-time.After(3 * time.Second):
				t.Fatal("automatic memory fixture did not complete the response")
			}
			cancel()
			assembly.DrainMemory(ctx)
		})
	}
}
