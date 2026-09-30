package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
)

func TestCLIPromptUsesPersistentProjectTrustAndRevokeStopsMCPContact(t *testing.T) {
	var initializeCalls atomic.Int32
	var listCalls atomic.Int32
	mcpPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "decode MCP request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "initialize":
			initializeCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{
					"protocolVersion": "2025-06-18",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]any{"name": "trust-e2e", "version": "1"},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			listCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{"tools": []any{
					map[string]any{
						"name":        "ping",
						"description": "trust test tool",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					},
				}},
			})
		default:
			http.Error(w, "unexpected MCP method", http.StatusBadRequest)
		}
	}))
	defer mcpPeer.Close()

	llmPeer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"prompt-ok"}}]}`)
	}))
	defer llmPeer.Close()

	rootDir, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	binaryDir := t.TempDir()
	binary := filepath.Join(binaryDir, "go-cli")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cli")
	build.Dir = rootDir
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, string(buildOutput))

	home := filepath.Join(t.TempDir(), "home")
	workDir := filepath.Join(t.TempDir(), "project")
	require.NoError(t, os.MkdirAll(workDir, 0o700))
	workDir, err = filepath.EvalSymlinks(workDir)
	require.NoError(t, err)
	for _, dir := range []string{home, filepath.Join(home, ".go-cli"), filepath.Join(workDir, ".go-cli")} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	globalConfig := fmt.Sprintf("provider:\n  name: openai\n  api_key: test-key\n  base_url: %s/v1\n  model: trust-e2e\nmodel:\n  name: trust-e2e\ntracing:\n  enabled: false\n", llmPeer.URL)
	require.NoError(t, os.WriteFile(filepath.Join(home, ".go-cli", "config.yaml"), []byte(globalConfig), 0o600))
	projectMCP := fmt.Sprintf(`{"servers":[{"name":"trusted-peer","url":%q}]}`, mcpPeer.URL)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, ".go-cli", "mcp.json"), []byte(projectMCP), 0o600))
	t.Chdir(workDir)

	trustPath := filepath.Join(home, ".config", "go-cli", "trust.json")
	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(trustPath))
	snapshot, err := config.CaptureProjectSources(context.Background(), workDir, nil)
	require.NoError(t, err)
	canonicalRoot := snapshot.CanonicalRoot()
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), snapshot))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := trustAssemblyTestEnv(os.Environ(), map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, "xdg"),
		"XDG_CACHE_HOME":  filepath.Join(home, "cache"),
		"NO_COLOR":        "1",
		"TERM":            "dumb",
	})
	runPrompt := func() (string, error) {
		command := exec.CommandContext(ctx, binary, "prompt", "-provider=openai", "-output=json", "check trusted tools")
		command.Dir = workDir
		command.Env = env
		output, runErr := command.CombinedOutput()
		return string(output), runErr
	}

	output, err := runPrompt()
	require.NoError(t, err, output)
	require.Contains(t, output, "prompt-ok")
	require.Positive(t, initializeCalls.Load(), "trusted project MCP server should initialize during assembly")
	require.Positive(t, listCalls.Load(), "trusted project MCP server tools should be listed during assembly")

	revoke := exec.CommandContext(ctx, binary, "trust", "revoke", canonicalRoot)
	revoke.Dir = workDir
	revoke.Env = env
	revokeOutput, err := revoke.CombinedOutput()
	require.NoError(t, err, string(revokeOutput))
	initializesAfterRevoke := initializeCalls.Load()
	listsAfterRevoke := listCalls.Load()

	output, err = runPrompt()
	require.NoError(t, err, output)
	require.Contains(t, output, "prompt-ok")
	require.Equal(t, initializesAfterRevoke, initializeCalls.Load(), "revoked project MCP server must not be contacted")
	require.Equal(t, listsAfterRevoke, listCalls.Load(), "revoked project MCP tools must not be listed")
}

func trustAssemblyTestEnv(base []string, replacements map[string]string) []string {
	env := make([]string, 0, len(base)+len(replacements))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			env = append(env, entry)
			continue
		}
		if _, replaced := replacements[key]; !replaced {
			env = append(env, entry)
		}
	}
	for key, value := range replacements {
		env = append(env, key+"="+value)
	}
	return env
}
