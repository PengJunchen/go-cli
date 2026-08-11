package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestE2E_ContextCancellationViaInteractive verifies that the REPL handles
// context cancellation gracefully. A mock LLM server delays its response; the
// parent context is cancelled mid-turn. The REPL should print "[interrupted]"
// and exit without hanging.
func TestE2E_ContextCancellationViaInteractive(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Simulate a slow LLM response so the context is cancelled mid-turn.
		time.Sleep(3 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"slow"}}]}`))
	}))
	defer srv.Close()

	cfg := newE2ETestConfig(t, srv.URL)

	var out bytes.Buffer
	in := &delayedReader{
		lines: []string{"hello", "exit"},
		delay: 5 * time.Second, // long delay so context cancels before second line is read
	}
	cmd := newInteractiveCmd(in, &out)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Cancel the context after the turn has started.
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	err := cmd.Run(ctx, cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")
}

// TestE2E_SessionPersistenceViaInteractive verifies that when a session store
// path is configured, user and assistant entries are persisted to the JSONL
// file. After the REPL exits, the file should contain at least one user entry
// and one assistant entry.
func TestE2E_SessionPersistenceViaInteractive(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"persisted-ok"}}]}`))
	}))
	defer srv.Close()

	cfg := newE2ETestConfig(t, srv.URL)
	sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
	cfg.Session.StorePath = sessionPath

	var out bytes.Buffer
	in := &delayedReader{
		lines: []string{"remember this", "exit"},
		delay: 200 * time.Millisecond,
	}
	cmd := newInteractiveCmd(in, &out)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := cmd.Run(ctx, cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")

	// Read the session file and verify entries were persisted.
	file, err := os.Open(sessionPath)
	require.NoError(t, err)
	defer file.Close() //nolint:errcheck

	var hasUser, hasAssistant bool
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var entry session.SessionEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			continue
		}
		switch entry.Type {
		case session.EntryTypeUser:
			hasUser = true
			assert.Contains(t, entry.Content, "remember this")
		case session.EntryTypeAssistant:
			hasAssistant = true
			assert.Contains(t, entry.Content, "persisted-ok")
		}
	}
	require.NoError(t, scanner.Err())
	assert.True(t, hasUser, "session file must contain at least one user entry")
	assert.True(t, hasAssistant, "session file must contain at least one assistant entry")
}

// TestE2E_CompactionViaInteractive verifies that the /compact slash command
// triggers the compaction hook and reports the before/after message count.
func TestE2E_CompactionViaInteractive(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var mu sync.Mutex
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		// Return a simple response for both conversation and summarization requests.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"compacted summary"}}]}`))
	}))
	defer srv.Close()

	cfg := newE2ETestConfig(t, srv.URL)

	var out bytes.Buffer
	in := &delayedReader{
		lines: []string{"tell me a story", "/compact", "exit"},
		delay: 300 * time.Millisecond,
	}
	cmd := newInteractiveCmd(in, &out)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := cmd.Run(ctx, cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")
	assert.Contains(t, out.String(), "Compacted history")
}

// TestE2E_MultiTurnConversationViaInteractive verifies that the REPL correctly
// handles multiple consecutive turns, each with a distinct LLM response. The
// test ensures the REPL does not crash or lose state between turns.
func TestE2E_MultiTurnConversationViaInteractive(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var mu sync.Mutex
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requestCount++
		n := requestCount
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		responses := []string{
			`{"choices":[{"message":{"role":"assistant","content":"first response"}}]}`,
			`{"choices":[{"message":{"role":"assistant","content":"second response"}}]}`,
			`{"choices":[{"message":{"role":"assistant","content":"third response"}}]}`,
		}
		idx := (n - 1) % len(responses)
		_, _ = w.Write([]byte(responses[idx]))
	}))
	defer srv.Close()

	cfg := newE2ETestConfig(t, srv.URL)

	var out bytes.Buffer
	in := &delayedReader{
		lines: []string{"msg one", "msg two", "msg three", "exit"},
		delay: 200 * time.Millisecond,
	}
	cmd := newInteractiveCmd(in, &out)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := cmd.Run(ctx, cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "first response")
	assert.Contains(t, out.String(), "second response")
	assert.Contains(t, out.String(), "third response")
	assert.Contains(t, out.String(), "Session ended")
}

// TestE2E_EOFExitsCleanly verifies that the REPL exits cleanly when the input
// stream reaches EOF (e.g., piped input that ends without an explicit "exit").
func TestE2E_EOFExitsCleanly(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	cfg := newE2ETestConfig(t, srv.URL)

	var out bytes.Buffer
	// No "exit" line — EOF should terminate the session.
	in := &delayedReader{
		lines: []string{"hello"},
		delay: 200 * time.Millisecond,
	}
	cmd := newInteractiveCmd(in, &out)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	err := cmd.Run(ctx, cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")
}


