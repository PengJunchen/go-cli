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

const memoryExtractionResponse = `{"choices":[{"message":{"role":"assistant","content":"[{\"content\":\"user prefers Go\",\"category\":\"preference\"}]"}}]}`

const conversationResponse = `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`

// delayedReader is an io.Reader that yields the given lines one at a time,
// sleeping for delay before each line after the first. Used by E2E tests
// that need to control input pacing.
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
	in := strings.NewReader("hello\nexit\n")
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
// seconds; the session should still accept and process input quickly.
// Cleanup waits for the extraction goroutine via WaitGroup, ensuring the
// extracted memory is persisted before Run returns.
func TestMemoryExtractionAsync(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := newMemoryTestServer(t, memoryExtractionResponse, 2*time.Second, 0)
	defer srv.Close()

	cfg, homeDir := newMemoryTestConfig(t, srv.URL)

	var out bytes.Buffer
	in := strings.NewReader("hello\nexit\n")
	cmd := newInteractiveCmd(in, &out)

	err := cmd.Run(t.Context(), cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")

	// With the WaitGroup, Cleanup waits for the extraction goroutine to
	// finish before closing the memory store. The extracted memory should
	// be persisted.
	memPath := filepath.Join(homeDir, ".go-cli", "memories.jsonl")
	data, err := os.ReadFile(memPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "user prefers Go")
}

// TestMemoryExtractionError verifies that when Extract fails (server returns
// 500), the main flow is not affected and the session ends normally.
func TestMemoryExtractionError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := newMemoryTestServer(t, "", 0, http.StatusInternalServerError)
	defer srv.Close()

	cfg, _ := newMemoryTestConfig(t, srv.URL)

	var out bytes.Buffer
	in := strings.NewReader("hello\nexit\n")
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
	in := strings.NewReader("hello\nworld\nexit\n")
	cmd := newInteractiveCmd(in, &out)

	err := cmd.Run(t.Context(), cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")

	memPath := filepath.Join(homeDir, ".go-cli", "memories.jsonl")
	data, err := os.ReadFile(memPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "user prefers Go")
}

// TestMemoryExtractionCleanupRace verifies that calling Cleanup immediately
// after a turn (while the extraction goroutine is still running) does not
// cause a panic, data race, or goroutine leak. The extractor has a 2s delay
// to ensure the goroutine is in-flight when Cleanup runs.
func TestMemoryExtractionCleanupRace(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := newMemoryTestServer(t, memoryExtractionResponse, 2*time.Second, 0)
	defer srv.Close()

	cfg, _ := newMemoryTestConfig(t, srv.URL)

	var out bytes.Buffer
	in := strings.NewReader("hello\nexit\n")
	cmd := newInteractiveCmd(in, &out)

	err := cmd.Run(t.Context(), cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")
}

// TestMemoryExtractionCleanupPreservesData verifies that when the extraction
// goroutine is writing to the memory file during shutdown, the resulting
// memories.jsonl is complete and readable (no JSON truncation).
func TestMemoryExtractionCleanupPreservesData(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := newMemoryTestServer(t, memoryExtractionResponse, 500*time.Millisecond, 0)
	defer srv.Close()

	cfg, homeDir := newMemoryTestConfig(t, srv.URL)

	var out bytes.Buffer
	in := strings.NewReader("hello\nexit\n")
	cmd := newInteractiveCmd(in, &out)

	err := cmd.Run(t.Context(), cfg, nil)
	require.NoError(t, err)

	// Verify memories.jsonl is complete and valid JSON lines.
	memPath := filepath.Join(homeDir, ".go-cli", "memories.jsonl")
	data, err := os.ReadFile(memPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "user prefers Go")

	// Verify each line is valid JSON.
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var v map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &v), "memories.jsonl contains invalid JSON line: %s", line)
	}
}

// TestMultipleTurnsMemoryGoroutineDrainage verifies that after 3 consecutive
// turns with immediate exit, all memory extraction goroutines are drained
// by Cleanup before the memory store is closed.
func TestMultipleTurnsMemoryGoroutineDrainage(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := newMemoryTestServer(t, memoryExtractionResponse, 100*time.Millisecond, 0)
	defer srv.Close()

	cfg, homeDir := newMemoryTestConfig(t, srv.URL)

	var out bytes.Buffer
	in := strings.NewReader("turn1\nturn2\nturn3\nexit\n")
	cmd := newInteractiveCmd(in, &out)

	err := cmd.Run(t.Context(), cfg, nil)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Session ended")

	// All 3 extraction goroutines should have completed and written memories.
	memPath := filepath.Join(homeDir, ".go-cli", "memories.jsonl")
	data, err := os.ReadFile(memPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "user prefers Go")
}
