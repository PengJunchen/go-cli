package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/memory"
)

const manualMemoryPromptCanary = "stored preference must stay out of the prompt"

func TestMemoryManualCRUDSurvivesRealCLIRestarts(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	binary := filepath.Join(t.TempDir(), "go-cli")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cli")
	build.Dir = root
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, string(buildOutput))

	var requestMu sync.Mutex
	var requestBodies [][]byte
	var extractionRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		requestMu.Lock()
		requestBodies = append(requestBodies, append([]byte(nil), body...))
		requestMu.Unlock()

		if strings.Contains(string(body), "Analyze the following conversation") {
			extractionRequests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"[{\"content\":\"automatically extracted memory\",\"category\":\"preference\"}]"}}]}`)
			return
		}
		var request struct {
			Stream bool `json:"stream"`
		}
		if json.Unmarshal(body, &request) != nil {
			http.Error(w, "decode request", http.StatusBadRequest)
			return
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"manual-memory\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"manual-memory\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	home := filepath.Join(t.TempDir(), "home")
	configDir := filepath.Join(home, ".go-cli")
	for _, dir := range []string{home, configDir, filepath.Join(home, "xdg"), filepath.Join(home, "cache")} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	configYAML := fmt.Sprintf("provider:\n  name: openai\n  base_url: %s/v1\n  api_key: local-test-key\n  model: gpt-4o-mini\nmodel:\n  name: gpt-4o-mini\ntracing:\n  enabled: false\nmemory:\n  enabled: false\n", server.URL)
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600))

	memoryPath := filepath.Join(configDir, "memories.jsonl")
	seed, err := memory.NewFileMemoryStore(memoryPath)
	require.NoError(t, err)
	require.NoError(t, seed.Add(ctx, memory.Memory{
		ID:       "mem_prompt_canary",
		Content:  manualMemoryPromptCanary,
		Category: "preference",
		Source:   "manual",
	}))
	require.NoError(t, seed.Close())

	env := authPersistenceTestEnv(os.Environ(), map[string]string{
		"HOME":                 home,
		"XDG_CONFIG_HOME":      filepath.Join(home, "xdg"),
		"XDG_CACHE_HOME":       filepath.Join(home, "cache"),
		"NO_COLOR":             "1",
		"TERM":                 "dumb",
		"GO_CLI_NO_ONBOARDING": "1",
		"OPENAI_API_KEY":       "",
	})
	run := func(input string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, "interactive", "--no-onboarding", "--provider=openai", "--model=gpt-4o-mini")
		cmd.Dir = t.TempDir()
		cmd.Env = env
		cmd.Stdin = strings.NewReader(input)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if runErr := cmd.Run(); runErr != nil {
			t.Fatalf("real CLI failed: %v\nstdout:\n%s\nstderr:\n%s", runErr, stdout.String(), stderr.String())
		}
		return stdout.String() + stderr.String()
	}

	firstOutput := run("/memory add User prefers herbal tea\nhello\nexit\n")
	require.Contains(t, firstOutput, "Added memory")
	require.Contains(t, firstOutput, "AI: ok")
	manualIDMatch := regexp.MustCompile(`Added memory (\S+):`).FindStringSubmatch(firstOutput)
	require.Len(t, manualIDMatch, 2, "manual add should report its persisted ID")
	manualID := manualIDMatch[1]
	require.Equal(t, int32(0), extractionRequests.Load(), "manual-only mode must not send an extraction request")

	requestMu.Lock()
	firstRequests := make([][]byte, len(requestBodies))
	for i := range requestBodies {
		firstRequests[i] = append([]byte(nil), requestBodies[i]...)
	}
	requestMu.Unlock()
	require.Len(t, firstRequests, 1, "one conversation turn should make one model request")
	require.NotContains(t, string(firstRequests[0]), manualMemoryPromptCanary, "stored manual memory must not be injected into the prompt")
	items := readRealCLIMemories(t, memoryPath)
	require.Len(t, items, 2, "only the seed and user-added record should be present")
	assertMemoryHasContent(t, items, "User prefers herbal tea")

	secondOutput := run("/memory list\n/memory search herbal tea\n/memory delete " + manualID + "\nexit\n")
	require.Contains(t, secondOutput, manualID)
	require.Contains(t, secondOutput, "User prefers herbal tea")
	require.Contains(t, secondOutput, "Deleted memory "+manualID)
	require.Equal(t, int32(0), extractionRequests.Load())

	thirdOutput := run("/memory list\nexit\n")
	require.Contains(t, thirdOutput, "Memories (1):")
	require.Contains(t, thirdOutput, "mem_prompt_canary")
	require.NotContains(t, thirdOutput, "User prefers herbal tea", "the deletion must survive the next process restart")
	items = readRealCLIMemories(t, memoryPath)
	require.Len(t, items, 1)
	require.Equal(t, "mem_prompt_canary", items[0].ID)
	require.Equal(t, int32(1), requestCount(&requestMu, requestBodies), "slash CRUD in restarted processes must not call the model")
}

func readRealCLIMemories(t *testing.T, path string) []memory.Memory {
	t.Helper()
	store, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	items, err := store.List(t.Context())
	require.NoError(t, err)
	return items
}

func assertMemoryHasContent(t *testing.T, items []memory.Memory, content string) {
	t.Helper()
	for _, item := range items {
		if item.Content == content {
			return
		}
	}
	t.Fatalf("memory store does not contain %q: %#v", content, items)
}

func requestCount(mu *sync.Mutex, bodies [][]byte) int32 {
	mu.Lock()
	defer mu.Unlock()
	return int32(len(bodies))
}
