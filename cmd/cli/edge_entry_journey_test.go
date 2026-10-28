package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
)

// TestEdgeEntryLoaderReadEditRead exercises the shipped entry/loader/Harness
// path with an explicitly granted temporary project and a temporary trust store.
// It is integration evidence, not an independently timed CLI binary benchmark.
func TestEdgeEntryLoaderReadEditRead(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	project, err := filepath.EvalSymlinks(project)
	require.NoError(t, err)
	answer := filepath.Join(project, "answer.txt")
	require.NoError(t, os.WriteFile(answer, []byte("answer = 41\n"), 0600))
	skillDir := filepath.Join(project, ".go-cli", "skills", "writing")
	require.NoError(t, os.MkdirAll(skillDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: writing\ndescription: Writing instructions for the entry fixture.\n---\nPRIVATE_EDGE_SKILL_BODY\n"), 0600))
	var mcpRequests atomic.Int32
	mcpPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mcpRequests.Add(1)
		http.Error(w, "unselected MCP must remain disconnected", http.StatusBadRequest)
	}))
	t.Cleanup(mcpPeer.Close)

	type requestMessage struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	type modelRequest struct {
		Messages []requestMessage `json:"messages"`
		Tools    json.RawMessage  `json:"tools"`
		Stream   bool             `json:"stream"`
	}
	var mu sync.Mutex
	var requests []modelRequest
	var schemaBytes []int
	llmPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request modelRequest
		if decodeErr := json.NewDecoder(r.Body).Decode(&request); decodeErr != nil {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		index := len(requests)
		requests = append(requests, request)
		schemaBytes = append(schemaBytes, len(request.Tools))
		mu.Unlock()
		message := map[string]any{"role": "assistant", "content": "EDGE_JOURNEY_OK"}
		finish := "stop"
		if index < 3 {
			name := "read"
			args := map[string]any{"path": answer}
			if index == 1 {
				name = "edit"
				args = map[string]any{"file_path": answer, "old_string": "answer = 41\n", "new_string": "answer = 42\n"}
			}
			arguments, _ := json.Marshal(args)
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"id": fmt.Sprintf("edge-fixture-%d", index), "type": "function", "function": map[string]any{"name": name, "arguments": string(arguments)}}}
			finish = "tool_calls"
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			if calls, ok := message["tool_calls"].([]any); ok {
				calls[0].(map[string]any)["index"] = 0
			}
			first, _ := json.Marshal(map[string]any{"id": "edge-fixture", "choices": []any{map[string]any{"index": 0, "delta": message}}})
			last, _ := json.Marshal(map[string]any{"id": "edge-fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", first, last)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "edge-fixture", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
	}))
	t.Cleanup(llmPeer.Close)
	for key, value := range map[string]string{
		"GO_CLI_PROVIDER_NAME": "openai", "GO_CLI_PROVIDER_API_KEY": "fixture",
		"GO_CLI_PROVIDER_BASE_URL": llmPeer.URL + "/v1", "GO_CLI_MODEL_NAME": "fixture",
		"GO_CLI_TRACING_ENABLED": "false", "GO_CLI_GIT_ENABLED": "false", "GO_CLI_EXTENSIONS_ENABLED": "false",
	} {
		t.Setenv(key, value)
	}
	settings := map[string]any{
		"runtime": map[string]any{"profile": "edge", "max_skills": 4, "max_mcp_servers": 1, "max_prompt_bytes": 131072},
		"skill":   map[string]any{"dir": ".go-cli/skills"},
		"mcp":     map[string]any{"servers": []any{map[string]any{"name": "unselected", "url": mcpPeer.URL}}},
	}
	isolateEntryFixtureStorage(t, project, settings)
	configPath := filepath.Join(project, ".go-cli.json")
	writeSettings := func() {
		data, encodeErr := json.Marshal(settings)
		require.NoError(t, encodeErr)
		require.NoError(t, os.WriteFile(configPath, data, 0600))
	}
	writeSettings()
	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(filepath.Join(t.TempDir(), "trust", "trust.json")))
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	grant := func() {
		snapshot, captureErr := config.CaptureProjectSources(context.Background(), project, nil)
		require.NoError(t, captureErr)
		require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), snapshot))
	}
	grant()
	loaded, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, loaded.ProjectSourceTrustStatus)
	require.Equal(t, "edge", loaded.Runtime.Profile)

	var stdout, stderr bytes.Buffer
	code := runWithTrustManager([]string{"prompt", "--provider", "openai", "--model", "fixture", "--approve", "auto", "Read answer.txt, edit answer = 41 to answer = 42, read it again, then return EDGE_JOURNEY_OK."}, &stdout, &stderr, manager)
	require.Zero(t, code, stderr.String())
	require.Contains(t, stdout.String(), "EDGE_JOURNEY_OK")
	content, err := os.ReadFile(answer)
	require.NoError(t, err)
	require.Equal(t, "answer = 42\n", string(content))
	require.Zero(t, mcpRequests.Load(), "edge bootstrap and unrelated file tools must not dial unselected MCP")
	mu.Lock()
	observed := append([]modelRequest(nil), requests...)
	observedSchemaBytes := append([]int(nil), schemaBytes...)
	mu.Unlock()
	require.Len(t, observed, 4)
	for _, request := range observed {
		for _, message := range request.Messages {
			require.NotContains(t, string(message.Content), "PRIVATE_EDGE_SKILL_BODY")
		}
	}
	require.Contains(t, string(observed[0].Messages[0].Content), "Writing instructions for the entry fixture.")
	require.Contains(t, string(observed[1].Messages[len(observed[1].Messages)-1].Content), "answer = 41")
	require.Contains(t, string(observed[3].Messages[len(observed[3].Messages)-1].Content), "answer = 42")
	t.Logf("edge integration: profile=edge; max_skills=4; max_mcp_servers=1; max_prompt_bytes=131072; model_requests=%d; request_schema_bytes=%v; unselected_mcp_requests=%d", len(observed), observedSchemaBytes, mcpRequests.Load())

	settings["runtime"].(map[string]any)["max_prompt_bytes"] = 1024
	writeSettings()
	grant()
	stdout.Reset()
	stderr.Reset()
	code = runWithTrustManager([]string{"prompt", "--provider", "openai", "--model", "fixture", "--approve", "auto", "Read answer.txt"}, &stdout, &stderr, manager)
	require.NotZero(t, code)
	require.Contains(t, strings.ToLower(stderr.String()), "budget")
	mu.Lock()
	requestsAfterBudgetRefusal := len(requests)
	mu.Unlock()
	require.Equal(t, len(observed), requestsAfterBudgetRefusal, "budget refusal must precede a model dispatch")
	require.Zero(t, mcpRequests.Load())
}

func isolateEntryFixtureStorage(t *testing.T, project string, settings map[string]any) {
	t.Helper()
	root := filepath.Join(project, ".go-cli", "test-storage")
	require.NoError(t, os.MkdirAll(root, 0700))
	settings["memory"] = map[string]any{"enabled": false, "store_path": filepath.Join(root, "memories.jsonl")}
	settings["history"] = map[string]any{"path": filepath.Join(root, "history.jsonl")}
	settings["production"] = map[string]any{"audit": map[string]any{"path": filepath.Join(root, "audit.jsonl")}}
}
