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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/memory"
	"github.com/pengjunchen/go-cli/internal/verify"
)

const (
	memoryExtractionResponse = `{"choices":[{"message":{"role":"assistant","content":"[{\"content\":\"automatically extracted canary\",\"category\":\"preference\"}]"}}]}`
	conversationResponse     = `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`
	storedMemoryCanary       = "private stored memory must stay out of model prompts"
)

type memoryTestServer struct {
	*httptest.Server

	extractionRequests atomic.Int32
	requestsMu         sync.Mutex
	requests           [][]byte
}

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

// newMemoryTestServer records model requests and recognizes automatic memory
// extraction requests. Its extraction response makes accidental default
// extraction visible in the persisted JSONL store.
func newMemoryTestServer(t *testing.T) *memoryTestServer {
	t.Helper()
	fixture := &memoryTestServer{}
	fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read request body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		fixture.requestsMu.Lock()
		fixture.requests = append(fixture.requests, append([]byte(nil), body...))
		fixture.requestsMu.Unlock()

		if strings.Contains(string(body), "Analyze the following conversation") {
			fixture.extractionRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, memoryExtractionResponse)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, conversationResponse)
	}))
	return fixture
}

func (s *memoryTestServer) requestBodies() [][]byte {
	s.requestsMu.Lock()
	defer s.requestsMu.Unlock()
	bodies := make([][]byte, len(s.requests))
	for i := range s.requests {
		bodies[i] = append([]byte(nil), s.requests[i]...)
	}
	return bodies
}

// newMemoryTestConfig creates a config that redirects the memory store to a
// temp HOME directory and the durable session store to a temp directory. It returns the
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
			StorePath: filepath.Join(t.TempDir(), "sessions"),
		},
	}
	return cfg, homeDir
}

func addStoredMemory(t *testing.T, path string, item memory.Memory) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	store, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	require.NoError(t, store.Add(t.Context(), item))
	require.NoError(t, store.Close())
}

func readStoredMemories(t *testing.T, path string) []memory.Memory {
	t.Helper()
	store, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	items, err := store.List(t.Context())
	require.NoError(t, err)
	return items
}

type injectedMemoryExtractor struct {
	calls atomic.Int32
}

func (e *injectedMemoryExtractor) Extract(context.Context, []llm.Message) ([]memory.Memory, error) {
	e.calls.Add(1)
	return []memory.Memory{{Content: "explicit library memory", Category: "fact", Source: "manual"}}, nil
}

func TestExplicitLibraryMemoryExtractorCanBeInjected(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	store, err := memory.NewFileMemoryStore(filepath.Join(t.TempDir(), "memories.jsonl"))
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	extractor := &injectedMemoryExtractor{}
	var wg sync.WaitGroup
	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "remember this"},
		{Role: "assistant", Content: "noted"},
	}))
	repl := &REPLSession{
		assembly: &AgentAssembly{
			CoreRuntime: CoreRuntime{Agent: agent},
			SessionManagement: SessionManagement{
				MemoryStore:     store,
				MemoryExtractor: extractor,
				MemoryWG:        &wg,
			},
		},
	}
	repl.extractMemory()
	wg.Wait()
	if repl.memoryCtxCancel != nil {
		repl.memoryCtxCancel()
	}

	require.Equal(t, int32(1), extractor.calls.Load())
	items, err := store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "explicit library memory", items[0].Content)
}

func TestInteractiveMemoryIsManualOnlyByDefault(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := newMemoryTestServer(t)
	defer srv.Close()
	cfg, homeDir := newMemoryTestConfig(t, srv.URL)
	memPath := filepath.Join(homeDir, ".go-cli", "memories.jsonl")
	addStoredMemory(t, memPath, memory.Memory{
		ID:       "mem_stored_canary",
		Content:  storedMemoryCanary,
		Category: "preference",
		Source:   "manual",
	})

	var out bytes.Buffer
	cmd := newInteractiveCmd(strings.NewReader("hello\nexit\n"), &out)
	require.NoError(t, cmd.Run(t.Context(), cfg, nil))
	assert.Contains(t, out.String(), "Session ended")

	assert.Equal(t, int32(0), srv.extractionRequests.Load(), "the default REPL must not call an LLM memory extractor")
	memories := readStoredMemories(t, memPath)
	require.Len(t, memories, 1, "normal conversation turns must not create memories")
	assert.Equal(t, "mem_stored_canary", memories[0].ID)

	bodies := srv.requestBodies()
	require.Len(t, bodies, 1, "a single conversation turn should make one model request")
	assert.NotContains(t, string(bodies[0]), storedMemoryCanary, "existing memory must not be injected into the model prompt")
}

func TestInteractiveMultipleTurnsDoNotExtractMemoryByDefault(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := newMemoryTestServer(t)
	defer srv.Close()
	cfg, homeDir := newMemoryTestConfig(t, srv.URL)
	var out bytes.Buffer
	cmd := newInteractiveCmd(strings.NewReader("first turn\nsecond turn\nexit\n"), &out)
	require.NoError(t, cmd.Run(t.Context(), cfg, nil))
	assert.Contains(t, out.String(), "Session ended")

	assert.Equal(t, int32(0), srv.extractionRequests.Load())
	bodies := srv.requestBodies()
	assert.Len(t, bodies, 2, "each user turn should produce one model request, without extraction requests")
	memPath := filepath.Join(homeDir, ".go-cli", "memories.jsonl")
	assert.Empty(t, readStoredMemories(t, memPath), "the default REPL should not persist extracted facts")
}

func TestMemoryRequestBodiesAreValidJSON(t *testing.T) {
	srv := newMemoryTestServer(t)
	defer srv.Close()
	cfg, _ := newMemoryTestConfig(t, srv.URL)
	var out bytes.Buffer
	cmd := newInteractiveCmd(strings.NewReader("hello\nexit\n"), &out)
	require.NoError(t, cmd.Run(t.Context(), cfg, nil))

	bodies := srv.requestBodies()
	require.Len(t, bodies, 1)
	var request map[string]any
	require.NoError(t, json.Unmarshal(bodies[0], &request))
	assert.Equal(t, "test-model", request["model"])
}

func TestInteractiveMemoryStoreIsManualOnlyWhenNoTurnsRun(t *testing.T) {
	srv := newMemoryTestServer(t)
	defer srv.Close()
	cfg, homeDir := newMemoryTestConfig(t, srv.URL)
	var out bytes.Buffer
	cmd := newInteractiveCmd(strings.NewReader("/memory add a hand-entered memory\n/memory list\nexit\n"), &out)
	require.NoError(t, cmd.Run(t.Context(), cfg, nil))
	assert.Contains(t, out.String(), "Added memory")
	assert.Contains(t, out.String(), "a hand-entered memory")
	assert.Zero(t, srv.extractionRequests.Load())
	assert.Empty(t, srv.requestBodies(), "slash memory CRUD should not invoke the model")

	items := readStoredMemories(t, filepath.Join(homeDir, ".go-cli", "memories.jsonl"))
	require.Len(t, items, 1)
	assert.Equal(t, "a hand-entered memory", items[0].Content)
	assert.Equal(t, "manual", items[0].Source)
}
