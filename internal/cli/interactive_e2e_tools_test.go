package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// newE2ETestConfig creates a config pointing to the given mock server URL.
// Persistent stores use explicit temporary paths, without changing HOME or
// the caller's workspace/sandbox configuration.
func newE2ETestConfig(t *testing.T, srvURL string) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Provider: config.ProviderConfig{
			Name:    "test",
			BaseURL: srvURL,
			APIKey:  "test",
			Model:   "test-model",
		},
	}
	isolateCLITestStorage(cfg, t.TempDir())
	return cfg
}

// TestE2E_NonTTYApprovalDenied drives a tool-call cycle through the
// non-TTY interactive REPL. The mock LLM requests a workspace read, but the
// interactive approval callback denies requests when stdin is not a terminal.
// The test verifies that the denial result is returned to the model and the
// file contents are not disclosed.
func TestE2E_NonTTYApprovalDenied(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	workspaceDir := t.TempDir()
	fixtureContent := "interactive read fixture"
	fixturePath := filepath.Join(workspaceDir, "fixture.txt")
	require.NoError(t, os.WriteFile(fixturePath, []byte(fixtureContent), 0o600))
	relativePath, err := filepath.Rel(workspaceDir, fixturePath)
	require.NoError(t, err)
	toolArgs, err := json.Marshal(map[string]string{"path": filepath.ToSlash(relativePath)})
	require.NoError(t, err)

	var mu sync.Mutex
	requestCount := 0
	toolDenialObserved := false
	fixtureContentObserved := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "failed to read request", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()

		mu.Lock()
		requestCount++
		n := requestCount
		toolDenialObserved = toolDenialObserved || strings.Contains(string(body), "tool call denied by approval policy")
		fixtureContentObserved = fixtureContentObserved || strings.Contains(string(body), fixtureContent)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// First request: return a safe read tool call.
			response, marshalErr := json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{
						"role":    "assistant",
						"content": nil,
						"tool_calls": []any{map[string]any{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      "read",
								"arguments": string(toolArgs),
							},
						}},
					},
				}},
			})
			if marshalErr != nil {
				http.Error(w, "failed to marshal response", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(response)
			return
		}
		// Subsequent requests (tool result / extraction): return a final response.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Done: interactive read fixture"}}]}`))
	}))
	defer srv.Close()

	cfg := newE2ETestConfig(t, srv.URL)
	cfg.Git.WorkDir = workspaceDir

	var out bytes.Buffer
	in := &delayedReader{
		lines: []string{"read the interactive fixture", "exit"},
		delay: 200 * time.Millisecond,
	}
	cmd := newInteractiveCmd(in, &out)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err = cmd.Run(ctx, cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Done: interactive read fixture")
	assert.Contains(t, out.String(), "Session ended")
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 2, requestCount, "the model should receive one denial followup before returning its final response")
	assert.True(t, toolDenialObserved, "the non-TTY approval denial must be sent back to the model")
	assert.False(t, fixtureContentObserved, "unapproved file contents must not be sent to the model")
}

// TestE2E_SlashCommandsViaInteractive verifies that slash commands are
// dispatched correctly inside the interactive REPL. The /help command should
// list available commands without calling the LLM.
func TestE2E_SlashCommandsViaInteractive(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	cfg := newE2ETestConfig(t, srv.URL)

	var out bytes.Buffer
	in := strings.NewReader("/help\nexit\n")
	cmd := newInteractiveCmd(in, &out)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	err := cmd.Run(ctx, cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Available commands")
	assert.Contains(t, out.String(), "Session ended")
}

// TestE2E_ErrorRecoveryViaInteractive verifies that the REPL recovers from a
// transient LLM error (HTTP 500). The mock server returns 500 on the first
// request, then 200 on all subsequent requests. The session should not crash
// and should eventually print "Session ended".
func TestE2E_ErrorRecoveryViaInteractive(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var mu sync.Mutex
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requestCount++
		n := requestCount
		mu.Unlock()

		if n == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal server error"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	cfg := newE2ETestConfig(t, srv.URL)

	var out bytes.Buffer
	in := &delayedReader{
		lines: []string{"hello", "hi", "exit"},
		delay: 200 * time.Millisecond,
	}
	cmd := newInteractiveCmd(in, &out)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := cmd.Run(ctx, cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")
}
