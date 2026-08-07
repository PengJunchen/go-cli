package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// delayedReader is an io.Reader that yields the given lines one at a time,
// sleeping for delay before each line after the first. This gives async
// memory-extraction goroutines time to finish before the next input line is
// consumed, avoiding races with assembly.Cleanup() closing the memory store.
type delayedReader struct {
	lines []string
	idx   int
	buf   []byte
	delay time.Duration
}

func (r *delayedReader) Read(p []byte) (int, error) {
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
	if r.idx >= len(r.lines) {
		return 0, io.EOF
	}
	if r.idx > 0 {
		time.Sleep(r.delay)
	}
	r.buf = []byte(r.lines[r.idx] + "\n")
	r.idx++
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

const memoryExtractionResponse = `{"choices":[{"message":{"role":"assistant","content":"[{\"content\":\"user prefers Go\",\"category\":\"preference\"}]"}}]}`

const emptyExtractionResponse = `{"choices":[{"message":{"role":"assistant","content":"[]"}}]}`

const conversationResponse = `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`

// newMemoryTestServer creates an httptest.Server that distinguishes between
// normal conversation requests and memory extraction requests by checking
// whether any message content contains "Analyze the following conversation".
// Extraction requests receive extractResponse (after an optional delay), while
// conversation requests always receive a simple assistant reply.
func newMemoryTestServer(t *testing.T, extractResponse string, extractDelay time.Duration, extractStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)

		isExtraction := false
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "Analyze the following conversation") {
				isExtraction = true
				break
			}
		}

		if isExtraction {
			if extractDelay > 0 {
				time.Sleep(extractDelay)
			}
			if extractStatus != 0 {
				w.WriteHeader(extractStatus)
				_, _ = w.Write([]byte(`{"error":"extraction failed"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(extractResponse))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(conversationResponse))
	}))
}

// newMemoryTestConfig creates a config that redirects the memory store to a
// temp HOME directory and the session store to a temp file. It returns the
// config and the HOME path (where memories.jsonl will be written).
func newMemoryTestConfig(t *testing.T, srvURL string) (*config.Config, string) {
	t.Helper()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	cfg := &config.Config{
		Provider: config.ProviderConfig{
			Name:    "test",
			BaseURL: srvURL,
			APIKey:  "test",
			Model:   "test-model",
		},
		Session: config.SessionConfig{
			StorePath: filepath.Join(t.TempDir(), "session.jsonl"),
		},
	}
	return cfg, homeDir
}

// TestMemoryExtraction verifies that after a turn, the memory extractor is
// called and extracted memories are persisted to the memory store file.
func TestMemoryExtraction(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := newMemoryTestServer(t, memoryExtractionResponse, 0, 0)
	defer srv.Close()

	cfg, homeDir := newMemoryTestConfig(t, srv.URL)

	var out bytes.Buffer
	in := &delayedReader{
		lines: []string{"hello", "exit"},
		delay: 500 * time.Millisecond,
	}
	cmd := newInteractiveCmd(in, &out)

	err := cmd.Run(t.Context(), cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")

	memPath := filepath.Join(homeDir, ".go-cli", "memories.jsonl")
	data, err := os.ReadFile(memPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "user prefers Go")
}

// TestMemoryExtractionAsync verifies that the Extract call does not block the
// main interaction flow. The mock server delays extraction responses by 2
// seconds; the session should still complete in under 1 second. An empty
// extraction response ([]) is used so the async goroutine never calls
// memStore.Add() after the store is closed by cleanup.
func TestMemoryExtractionAsync(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := newMemoryTestServer(t, emptyExtractionResponse, 2*time.Second, 0)
	defer srv.Close()

	cfg, _ := newMemoryTestConfig(t, srv.URL)

	var out bytes.Buffer
	in := strings.NewReader("hello\nexit\n")
	cmd := newInteractiveCmd(in, &out)

	start := time.Now()
	err := cmd.Run(t.Context(), cfg, nil)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")
	assert.Less(t, elapsed, time.Second, "session should complete in under 1 second despite 2s extraction delay")

	// Wait for the async extraction goroutine to finish before the goroutine
	// leak check runs.
	time.Sleep(2500 * time.Millisecond)
}

// TestMemoryExtractionError verifies that when Extract fails (server returns
// 500), the main flow is not affected and the session ends normally.
func TestMemoryExtractionError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := newMemoryTestServer(t, "", 0, http.StatusInternalServerError)
	defer srv.Close()

	cfg, _ := newMemoryTestConfig(t, srv.URL)

	var out bytes.Buffer
	in := &delayedReader{
		lines: []string{"hello", "exit"},
		delay: 500 * time.Millisecond,
	}
	cmd := newInteractiveCmd(in, &out)

	err := cmd.Run(t.Context(), cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")
}

// TestE2EMemoryExtraction performs a full multi-turn conversation and verifies
// that memories are extracted after each turn.
func TestE2EMemoryExtraction(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := newMemoryTestServer(t, memoryExtractionResponse, 0, 0)
	defer srv.Close()

	cfg, homeDir := newMemoryTestConfig(t, srv.URL)

	var out bytes.Buffer
	in := &delayedReader{
		lines: []string{"hello", "world", "exit"},
		delay: 500 * time.Millisecond,
	}
	cmd := newInteractiveCmd(in, &out)

	err := cmd.Run(t.Context(), cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")

	memPath := filepath.Join(homeDir, ".go-cli", "memories.jsonl")
	data, err := os.ReadFile(memPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "user prefers Go")
}
