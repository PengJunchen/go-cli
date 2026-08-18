//go:build e2e

package memory

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
)

// TestET_memory_full_cycle_persists verifies the complete cross-session memory
// lifecycle: fresh start, manual add, search/list, LLM extraction with
// deduplication, close, reopen, persistence, and injection into the system
// prompt via core.MemoryEntry.
func TestET_memory_full_cycle_persists(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1. Start with no memory file (fresh start).
	path := filepath.Join(t.TempDir(), "memories.jsonl")
	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err), "memory file must not exist at start")

	// 2. Create a FileMemoryStore.
	store, err := NewFileMemoryStore(path)
	require.NoError(t, err)

	// 3. Manually add memories. The primary memory will be duplicated by the
	// extractor later; the two fillers make the corpus large enough for TF-IDF
	// search to produce positive scores (N>=3 keeps idf>0 for df=1 terms).
	manualContent := "User prefers dark mode for the editor"
	require.NoError(t, store.Add(ctx, Memory{
		Content:  manualContent,
		Category: "preference",
		Source:   "manual",
	}))
	require.NoError(t, store.Add(ctx, Memory{
		Content:  "Python is used for data analysis scripts",
		Category: "fact",
		Source:   "manual",
	}))
	require.NoError(t, store.Add(ctx, Memory{
		Content:  "Rust is used for systems programming tasks",
		Category: "fact",
		Source:   "manual",
	}))

	// 4. Verify it can be searched/listed.
	list, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)

	res, err := store.Search(ctx, "dark mode", 10)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, manualContent, res[0].Content)

	// 5. Use LLMMemoryExtractor to extract memories from a mock conversation.
	// The mock returns two facts; one duplicates the manual memory.
	jsonResponse := `[{"content":"User prefers dark mode for the editor","category":"preference"},{"content":"Project uses Go 1.24 and Go modules","category":"convention"}]`
	server := mock.NewMockLLMServer(mock.NewConversationTemplate("T", "e2e-extract",
		mock.ConversationTurn{AssistantContent: jsonResponse},
	))
	extractor := NewLLMMemoryExtractor(server, store)

	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "I like dark mode for my editor."},
		{Role: llm.RoleAssistant, Content: "Got it, I'll use dark mode."},
	}

	extracted, err := extractor.Extract(ctx, msgs)
	require.NoError(t, err)

	// 6. Verify extracted memories are deduplicated against existing ones.
	require.Len(t, extracted, 1, "duplicate of manual memory must be filtered out")
	assert.Equal(t, "Project uses Go 1.24 and Go modules", extracted[0].Content)
	assert.Equal(t, "convention", extracted[0].Category)
	assert.Equal(t, "auto", extracted[0].Source)

	// Persist the extracted memory.
	require.NoError(t, store.Add(ctx, extracted[0]))

	list, err = store.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 4)

	// 7. Close the store.
	require.NoError(t, store.Close())

	// 8. Reopen the store from the same file.
	store2, err := NewFileMemoryStore(path)
	require.NoError(t, err)
	defer func() { _ = store2.Close() }()

	// 9. Verify all memories persisted.
	list, err = store2.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 4)

	contents := make(map[string]string, len(list))
	for _, m := range list {
		contents[m.Content] = m.Category
	}
	assert.Contains(t, contents, manualContent)
	assert.Equal(t, "preference", contents[manualContent])
	assert.Contains(t, contents, "Project uses Go 1.24 and Go modules")
	assert.Equal(t, "convention", contents["Project uses Go 1.24 and Go modules"])

	// Search index is rebuilt on reload.
	res, err = store2.Search(ctx, "Go modules", 10)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "Project uses Go 1.24 and Go modules", res[0].Content)

	// 10. Convert memories to core.MemoryEntry and verify they can be injected
	// into SystemPromptBuilder.
	coreEntries := make([]core.MemoryEntry, 0, len(list))
	for _, m := range list {
		coreEntries = append(coreEntries, core.MemoryEntry{
			ID:       m.ID,
			Content:  m.Content,
			Category: m.Category,
		})
	}

	builder := core.NewDefaultSystemPromptBuilder()
	prompt := builder.Build(ctx, core.SystemPromptOptions{
		Cwd:      "/test/e2e",
		Memories: coreEntries,
	})

	assert.Contains(t, prompt, "<memory>")
	assert.Contains(t, prompt, "</memory>")
	assert.Contains(t, prompt, manualContent)
	assert.Contains(t, prompt, "Project uses Go 1.24 and Go modules")
	assert.Contains(t, prompt, "## User Preferences")
	assert.Contains(t, prompt, "## Project Conventions")
}

// TestET_memory_search_tfidf verifies TF-IDF search ranking: add multiple
// memories with overlapping and unique terms, search with specific terms, and
// verify relevance ordering.
func TestET_memory_search_tfidf(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store, err := NewFileMemoryStore(filepath.Join(t.TempDir(), "memories.jsonl"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	// Add multiple memories with different term distributions.
	docs := []Memory{
		{ID: "d1", Content: "go go go programming language backend", Category: "fact"},
		{ID: "d2", Content: "go is fun but rust is also fun", Category: "fact"},
		{ID: "d3", Content: "python data analysis and visualization", Category: "fact"},
		{ID: "d4", Content: "deployment pipeline uses go modules", Category: "convention"},
		{ID: "d5", Content: "database postgresql connection pooling", Category: "fact"},
	}
	for _, d := range docs {
		require.NoError(t, store.Add(ctx, d))
	}

	// Search for "go" — appears in d1, d2, d4 (df=3, N=5, idf=log(5/4)>0).
	// d1: tf=3/6=0.50, d4: tf=1/5=0.20, d2: tf=1/8=0.125.
	res, err := store.Search(ctx, "go", 10)
	require.NoError(t, err)
	require.Len(t, res, 3)
	assert.Equal(t, "d1", res[0].ID, "d1 has highest TF for 'go'")
	assert.InDelta(t, 1.0, res[0].Relevance, 1e-9, "top result has normalized relevance 1.0")
	assert.Equal(t, "d4", res[1].ID, "d4 has second-highest TF for 'go'")
	assert.Equal(t, "d2", res[2].ID, "d2 has lowest TF for 'go'")
	assert.Greater(t, res[1].Relevance, res[2].Relevance)

	// Search for "database" — only d5 matches.
	res, err = store.Search(ctx, "database", 10)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "d5", res[0].ID)
	assert.InDelta(t, 1.0, res[0].Relevance, 1e-9)

	// Search for "data" — only d3 contains the standalone token "data"
	// ("database" in d5 is a single token, not "data"+"base").
	res, err = store.Search(ctx, "data", 10)
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.Equal(t, "d3", res[0].ID)

	// Multi-term search: "go backend" — d1 matches both terms and should
	// rank highest.
	res, err = store.Search(ctx, "go backend", 10)
	require.NoError(t, err)
	require.True(t, len(res) >= 1)
	assert.Equal(t, "d1", res[0].ID, "d1 matches both 'go' and 'backend'")
}

// TestET_memory_concurrent_access verifies that concurrent Add and Search
// operations are safe under the race detector.
func TestET_memory_concurrent_access(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store, err := NewFileMemoryStore(filepath.Join(t.TempDir(), "memories.jsonl"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	// Pre-populate with memories so Search has data to work with immediately.
	// The seed content intentionally avoids the word "race" so that searching
	// for "race" after concurrent writes produces positive TF-IDF scores.
	const seedCount = 10
	for i := 0; i < seedCount; i++ {
		require.NoError(t, store.Add(ctx, Memory{
			Content:  "preseed memory for search testing baseline",
			Category: "fact",
			Source:   "manual",
		}))
	}

	const numWriters = 20
	const numReaders = 20
	const numOps = 50

	var wg sync.WaitGroup
	wg.Add(numWriters + numReaders)

	// Concurrent writers: Add memories.
	for i := 0; i < numWriters; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < numOps; j++ {
				_ = store.Add(ctx, Memory{
					Content:  "concurrent memory content for race testing",
					Category: "fact",
					Source:   "auto",
				})
			}
		}()
	}

	// Concurrent readers: Search and List.
	for i := 0; i < numReaders; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < numOps; j++ {
				_, _ = store.Search(ctx, "race", 10)
				_, _ = store.List(ctx)
			}
		}()
	}

	wg.Wait()

	// Verify the store is in a consistent state after concurrent access.
	list, err := store.List(ctx)
	require.NoError(t, err)
	expectedLen := seedCount + numWriters*numOps
	assert.Len(t, list, expectedLen)

	// Search should still return results. "race" appears only in the writer
	// memories, giving a positive idf (df=1000, N=1010, idf=log(1010/1001)>0).
	res, err := store.Search(ctx, "race", 10)
	require.NoError(t, err)
	assert.NotEmpty(t, res)
}
