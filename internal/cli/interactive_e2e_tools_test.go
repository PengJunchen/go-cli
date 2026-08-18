package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
// It sets HOME to a temp directory so history files and other side-effects
// do not touch the real home directory.
func newE2ETestConfig(t *testing.T, srvURL string) *config.Config {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	return &config.Config{
		Provider: config.ProviderConfig{
			Name:    "test",
			BaseURL: srvURL,
			APIKey:  "test",
			Model:   "test-model",
		},
	}
}

// TestE2E_ToolExecutionViaInteractive drives a full tool-call cycle through
// the interactive REPL. The mock LLM first returns a bash tool call; after the
// loop executes (or denies) the tool and feeds the result back, the LLM
// returns a final text response. The test verifies that the session completes
// normally and the expected content reaches stdout.
func TestE2E_ToolExecutionViaInteractive(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var mu sync.Mutex
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requestCount++
		n := requestCount
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// First request: return a bash tool call.
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"echo hello\"}"}}]}}]}`))
			return
		}
		// Subsequent requests (tool result / extraction): return a final text response.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Done: hello"}}]}`))
	}))
	defer srv.Close()

	cfg := newE2ETestConfig(t, srv.URL)

	var out bytes.Buffer
	in := &delayedReader{
		lines: []string{"run echo hello", "exit"},
		delay: 200 * time.Millisecond,
	}
	cmd := newInteractiveCmd(in, &out)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := cmd.Run(ctx, cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "hello")
	assert.Contains(t, out.String(), "Session ended")
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
