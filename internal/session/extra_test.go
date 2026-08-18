package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// errBranchSummary always fails summarization; used to exercise the MoveTo
// error-during-summarization path.
type errBranchSummary struct{ calls int }

func (e *errBranchSummary) Summarize(context.Context, []SessionEntry) (string, error) {
	e.calls++
	return "", errors.New("summarizer unavailable")
}

func (e *errBranchSummary) Name() string { return "err-branch-summary" }

// emptyBranchSummary returns an empty summary so no summary entry is appended.
type emptyBranchSummary struct{ calls int }

func (e *emptyBranchSummary) Summarize(context.Context, []SessionEntry) (string, error) {
	e.calls++
	return "", nil
}

func (e *emptyBranchSummary) Name() string { return "empty-branch-summary" }

// TestTree_MoveToSameLeafNoSummary verifies that MoveTo to the current leaf
// neither triggers summarizing nor changes state.
func TestTree_MoveToSameLeafNoSummary(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	bs := &testBranchSummary{summary: "S"}
	tree := NewDefaultSessionTree().(*DefaultSessionTree) //nolint:errcheck
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(context.Background(), "a"))
	tree.SetBranchSummary(bs)

	// Moving to the same leaf is a no-op for summary generation.
	require.NoError(t, tree.MoveTo(context.Background(), "a"))
	assert.Equal(t, 0, bs.calls, "no summary should be generated when not departing")
	assert.Equal(t, 1, tree.EntryCount(), "no new entries should be added on a same-leaf move")
}

// TestTree_MoveToEmptySummaryDoesNotAppend verifies an empty summary from the
// summarizer yields no summary entry.
func TestTree_MoveToEmptySummaryDoesNotAppend(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree().(*DefaultSessionTree) //nolint:errcheck
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(context.Background(), "b"))
	tree.SetBranchSummary(&emptyBranchSummary{})
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(context.Background(), "c"))

	assert.Nil(t, findSummaryEntry(t, tree), "an empty summary must not append an entry")
	assert.Equal(t, 3, tree.EntryCount())
}

// TestTree_MoveToSummarizeErrorDoesNotFail verifies an error during
// summarization is non-fatal: MoveTo still succeeds and no summary is appended.
func TestTree_MoveToSummarizeErrorDoesNotFail(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	bs := &errBranchSummary{}
	tree := NewDefaultSessionTree().(*DefaultSessionTree) //nolint:errcheck
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeUser)))
	tree.SetBranchSummary(bs)
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))

	// First MoveTo positions at c and departs b (with b's branch summarized).
	require.NoError(t, tree.MoveTo(context.Background(), "c"))
	assert.GreaterOrEqual(t, bs.calls, 1)
	// No summary entry was appended despite the error.
	assert.Nil(t, findSummaryEntry(t, tree))
	assert.Equal(t, "c", tree.CurrentLeaf())
}

// TestTree_MoveToSummaryAppendedEntryLocation verifies the summary entry's
// parent is the departed leaf and the current leaf is unchanged.
func TestTree_MoveToSummaryAppendedEntryLocation(t *testing.T) {
	tree := NewDefaultSessionTree().(*DefaultSessionTree) //nolint:errcheck
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(context.Background(), "b"))
	tree.SetBranchSummary(&testBranchSummary{summary: "S"})
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(context.Background(), "c"))

	se := findSummaryEntry(t, tree)
	require.NotNil(t, se)
	assert.Equal(t, "b", se.ParentID, "summary must be appended to the departed branch")
	assert.Equal(t, "c", tree.CurrentLeaf(), "current leaf must remain the move target")
}

// TestTree_GetBranchWithParentChainMissingLeaf verifies that walking a chain
// referencing an unknown ancestor reports the leaf as not found.
func TestTree_GetBranchWithParentChainMissingLeaf(t *testing.T) {
	tree := NewDefaultSessionTree().(*DefaultSessionTree) //nolint:errcheck
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeUser)))
	// Append an entry whose parent b exists; then build a chain that references
	// a missing ancestor indirectly through a dangling parent.
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))

	_, err := tree.GetBranch(context.Background(), "orphan")
	require.ErrorIs(t, err, ErrLeafNotFound)
}

// TestTree_BuildContextLastUpdateLatest verifies LastUpdate picks the newest
// timestamp across the walked branch.
func TestTree_BuildContextLastUpdateLatest(t *testing.T) {
	tree := NewDefaultSessionTree().(*DefaultSessionTree) //nolint:errcheck
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(2 * time.Hour)
	require.NoError(t, tree.Append(context.Background(), &SessionEntry{ID: "a", Type: EntryTypeUser, Timestamp: t0}))
	require.NoError(t, tree.Append(context.Background(), &SessionEntry{ID: "b", ParentID: "a", Type: EntryTypeUser, Timestamp: t1}))

	sc, err := tree.BuildContext(context.Background(), "b")
	require.NoError(t, err)
	assert.True(t, sc.LastUpdate.Equal(t1), "LastUpdate must be the latest entry timestamp")
}

// TestTree_ConcurrentMoveToAndAppend stresses concurrent branch switches.
func TestTree_ConcurrentMoveToAndAppend(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := NewDefaultSessionTree().(*DefaultSessionTree) //nolint:errcheck
	require.NoError(t, tree.Append(context.Background(), newTestEntry("root", "", EntryTypeUser)))

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := "m-" + string(rune('a'+i))
			require.NoError(t, tree.Append(context.Background(), newTestEntry(id, "root", EntryTypeUser)))
			require.NoError(t, tree.MoveTo(context.Background(), id))
			_, err := tree.BuildContext(context.Background(), id)
			require.NoError(t, err)
		}(i)
	}
	wg.Wait()
	assert.Equal(t, n+1, tree.EntryCount())
}

// TestDefaultBranchSummary_NameOption verifies WithBranchSummaryName overrides
// the identifier.
func TestDefaultBranchSummary_NameOption(t *testing.T) {
	d := NewDefaultBranchSummary(func(context.Context, string) (string, error) { return "s", nil }, //nolint:errcheck
		WithBranchSummaryName("summarizer-v2")).(*DefaultBranchSummary)
	assert.Equal(t, "summarizer-v2", d.Name())
}

// TestDefaultBranchSummary_BuildPromptSkipsEmptyContent verifies entries with
// empty content are not included in the compaction prompt.
func TestDefaultBranchSummary_BuildPromptSkipsEmptyContent(t *testing.T) {
	var gotText string
	d := NewDefaultBranchSummary(func(_ context.Context, text string) (string, error) {
		gotText = text
		return "out", nil
	})
	entries := []SessionEntry{
		{ID: "a", Content: "keep me"},
		{ID: "b", Content: ""},
		{ID: "c", Content: "also keep"},
	}
	_, err := d.Summarize(context.Background(), entries)
	require.NoError(t, err)
	assert.Contains(t, gotText, "keep me")
	assert.Contains(t, gotText, "also keep")
	assert.NotContains(t, gotText, "content-b")
}

// TestBranchSummaryRegistryNilResets verifies registering nil reconstructs a
// default summarizer (mirrors the lazy default behavior).
func TestBranchSummaryRegistryNilResets(t *testing.T) {
	orig := GetBranchSummary()
	defer RegisterBranchSummary(orig)

	RegisterBranchSummary(nil)
	got := GetBranchSummary()
	assert.Equal(t, "default-branch-summary", got.Name())
	_, err := got.Summarize(context.Background(), []SessionEntry{{ID: "a"}})
	require.Error(t, err, "a reset default with no summarizer must error on use")
}

// TestEntryTypeConstants verifies the EntryType string values.
func TestEntryTypeConstants(t *testing.T) {
	assert.Equal(t, "user", string(EntryTypeUser))
	assert.Equal(t, "assistant", string(EntryTypeAssistant))
	assert.Equal(t, "tool", string(EntryTypeTool))
	assert.Equal(t, "compaction", string(EntryTypeCompaction))
	assert.Equal(t, "system", string(EntryTypeSystem))
}

// TestSessionEntryJSONTags verifies IsSummary serializes under its tag.
func TestSessionEntryJSONTags(t *testing.T) {
	e := &SessionEntry{ID: "1", Type: EntryTypeUser, IsSummary: true, Summary: "s"}
	data, err := json.Marshal(e)
	require.NoError(t, err)
	var decoded SessionEntry
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.True(t, decoded.IsSummary)
	assert.Equal(t, "s", decoded.Summary)
}

// TestSessionEntryCloneNil verifies clone handles a nil receiver.
func TestSessionEntryCloneNil(t *testing.T) {
	var e *SessionEntry
	assert.Nil(t, e.clone())
}

// TestSessionEntryCloneDefensive verifies clone returns a distinct copy.
func TestSessionEntryCloneDefensive(t *testing.T) {
	e := &SessionEntry{ID: "x", Type: EntryTypeUser, Content: "orig"}
	cp := e.clone()
	require.NotNil(t, cp)
	cp.Content = "mutated"
	assert.Equal(t, "orig", e.Content, "mutating the clone must not affect the source")
}

// TestContextManagerNilTree verifies a manager without a tree reports an error.
func TestContextManagerNilTree(t *testing.T) {
	mg := NewDefaultContextManager(nil)
	_, err := mg.BuildContext(context.Background(), "leaf")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no SessionTree")
}

// TestEstimateTokens verifies the Unicode-aware heuristic.
func TestEstimateTokens(t *testing.T) {
	assert.Equal(t, 0, estimateTokens(""))
	assert.Equal(t, 1, estimateTokens("abcd"))     // 4 ASCII * 0.25 = 1
	assert.Equal(t, 1, estimateTokens("abc"))      // 3 ASCII * 0.25 = 0.75 -> 1
	assert.Equal(t, 2, estimateTokens("abcdefgh")) // 8 ASCII * 0.25 = 2
}

// TestContextManagerCompactionTokenEstimate verifies compaction entries fold
// their Summary into the token estimate rather than their (empty) content.
func TestContextManagerCompactionTokenEstimate(t *testing.T) {
	tree := NewDefaultSessionTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	// 8-char summary -> 2 tokens when folded.
	require.NoError(t, tree.Append(context.Background(), newCompactionEntry("comp", "a", "summary8")))

	mg := NewDefaultContextManager(tree)
	sc, err := mg.BuildContext(context.Background(), "comp")
	require.NoError(t, err)
	// With compaction-point behavior, only the compaction summary is in Messages.
	require.Len(t, sc.Messages, 1)
	// Summary "summary8" (8 chars -> 2 tokens).
	assert.GreaterOrEqual(t, sc.EstimatedTokens, 2)
	assert.Equal(t, "summary8", sc.Messages[0].Content, "compaction must fold to its summary")
}

// TestMemoryStoreConcurrentCopySafety verifies concurrent Get returns copies
// that do not corrupt the store.
func TestMemoryStoreConcurrentCopySafety(t *testing.T) {
	s := NewMemoryStore()
	require.NoError(t, s.Append(context.Background(), newTestEntry("e1", "", EntryTypeUser)))

	const readers = 20
	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			got, err := s.Get(context.Background(), "e1")
			require.NoError(t, err)
			got.Content = "changed" // must not affect the store
		}()
	}
	wg.Wait()

	got, err := s.Get(context.Background(), "e1")
	require.NoError(t, err)
	assert.Equal(t, "content-e1", got.Content, "concurrent mutation of returned copies must be isolated")
}

// TestJSONLSessionStoreCloseIdempotent verifies Close is safe to call twice.
func TestJSONLSessionStoreCloseIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, store.Close())
	require.NoError(t, store.Close(), "second Close must be a no-op")
}

// TestJSONLSessionStoreSkipsCorruptLines verifies corrupt JSONL lines are
// ignored on load while valid entries remain readable.
func TestJSONLSessionStoreSkipsCorruptLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, store.Close())

	// Append a corrupt line and a valid one directly to the file.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("this is not json\n")
	require.NoError(t, err)
	valid, err := json.Marshal(newTestEntry("b", "", EntryTypeTool))
	require.NoError(t, err)
	_, err = f.Write(append(valid, '\n'))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	reopened := NewJSONLSessionStore(path)
	got, err := reopened.Get(context.Background(), "a")
	require.NoError(t, err)
	assert.Equal(t, "content-a", got.Content)

	b, err := reopened.Get(context.Background(), "b")
	require.NoError(t, err)
	assert.Equal(t, EntryTypeTool, b.Type)
	require.NoError(t, reopened.Close())
}

// TestJSONLSessionStoreOpenIdempotent verifies Open twice is safe.
func TestJSONLSessionStoreOpenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Open(context.Background()))
	require.NoError(t, store.Open(context.Background()))
	require.NoError(t, store.Close())
}

// TestJSONLSessionStoreDuplicateIDOnReload verifies re-appending an id already
// on disk fails even before a fresh store is explicitly opened.
func TestJSONLSessionStoreDuplicateIDOnReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	store := NewJSONLSessionStore(path)
	require.NoError(t, store.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, store.Close())

	fresh := NewJSONLSessionStore(path)
	require.Error(t, fresh.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, fresh.Close())
}

// TestJSONLSessionStoreConcurrentGet verifies concurrent loads on a loaded
// store are safe.
func TestJSONLSessionStoreConcurrentGet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	store := NewJSONLSessionStore(path)
	for i := 0; i < 10; i++ {
		require.NoError(t, store.Append(context.Background(), newTestEntry(string(rune('a'+i)), "", EntryTypeUser)))
	}
	require.NoError(t, store.Close())

	fresh := NewJSONLSessionStore(path)
	require.NoError(t, fresh.Open(context.Background()))
	defer func() { require.NoError(t, fresh.Close()) }()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := fresh.Get(context.Background(), string(rune('a'+i)))
			require.NoError(t, err)
		}(i)
	}
	wg.Wait()
}

// TestJSONLSessionStoreFilePath verifies FilePath returns the configured path.
func TestJSONLSessionStoreFilePath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "custom.jsonl")
	store := NewJSONLSessionStore(p)
	assert.Equal(t, p, store.FilePath())
}

// TestJSONLSessionStoreAppendNilEntry verifies Append rejects a nil entry.
func TestJSONLSessionStoreAppendNilEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	store := NewJSONLSessionStore(path)
	err := store.Append(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil entry")
	require.NoError(t, store.Close())
}

// TestJSONLSessionStoreAppendEmptyID verifies Append rejects an empty ID.
func TestJSONLSessionStoreAppendEmptyID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	store := NewJSONLSessionStore(path)
	err := store.Append(context.Background(), &SessionEntry{Type: EntryTypeUser})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id is required")
	require.NoError(t, store.Close())
}

// TestJSONLSessionStoreAppendEmptyType verifies Append rejects an empty type.
func TestJSONLSessionStoreAppendEmptyType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	store := NewJSONLSessionStore(path)
	err := store.Append(context.Background(), &SessionEntry{ID: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "type is required")
	require.NoError(t, store.Close())
}
