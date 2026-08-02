package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// DefaultBranchSummary.Summarize generates a summary via an injected
// SummarizeFunc, passing it the concatenated entries text.
func TestDefaultBranchSummary_Summarize(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var gotText string
	d := NewDefaultBranchSummary(func(_ context.Context, text string) (string, error) {
		gotText = text
		return "known-summary", nil
	})

	entries := []SessionEntry{
		{ID: "a", Content: "first message"},
		{ID: "b", Content: "second message"},
	}
	sum, err := d.Summarize(context.Background(), entries)
	require.NoError(t, err)
	assert.Equal(t, "known-summary", sum)
	assert.Contains(t, gotText, "first message")
	assert.Contains(t, gotText, "second message")
	assert.Equal(t, "default-branch-summary", d.Name())
}

// A nil SummarizeFunc surfaces a loud configuration error.
func TestDefaultBranchSummary_NilSummarizerErrors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	d := NewDefaultBranchSummary(nil)
	_, err := d.Summarize(context.Background(), []SessionEntry{{ID: "a", Content: "x"}})
	require.Error(t, err)
}

// context cancellation propagates to the Summarize call.
func TestDefaultBranchSummary_ContextCancellation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var innerErr error
	d := NewDefaultBranchSummary(func(c context.Context, _ string) (string, error) {
		select {
		case <-c.Done():
			innerErr = c.Err()
			return "", c.Err()
		case <-time.After(50 * time.Millisecond):
			return "late", nil
		}
	})

	_, err := d.Summarize(ctx, []SessionEntry{{ID: "a"}})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, innerErr, context.Canceled)
}

// testBranchSummary is a minimal BranchSummary used to observe the entries
// passed on a branch departure and return a fixed summary.
type testBranchSummary struct {
	summary string
	entries []SessionEntry
	calls   int
}

func (t *testBranchSummary) Summarize(_ context.Context, entries []SessionEntry) (string, error) {
	t.entries = append([]SessionEntry(nil), entries...)
	t.calls++
	return t.summary, nil
}

func (t *testBranchSummary) Name() string { return "test-branch-summary" }

// MoveTo triggers the BranchSummary on a branch switch and appends
// the summary as a SessionEntry on the departed branch.
func TestTree_MoveToAppendsBranchSummary(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	bs := &testBranchSummary{summary: "DEPARTED-SUM"}
	tree := newConcreteTree()

	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeAssistant)))
	require.NoError(t, tree.MoveTo(context.Background(), "b"))
	// Only enroll the summarizer after we are positioned at the branch that will
	// be departed, so the departure under test is the only one observed.
	tree.SetBranchSummary(bs)
	// Current leaf is "b". Extend a child branch then depart it.
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))
	require.Equal(t, "b", tree.CurrentLeaf())

	require.NoError(t, tree.MoveTo(context.Background(), "c"))
	assert.Equal(t, "c", tree.CurrentLeaf())

	// The summarizer received exactly the departed branch a->b.
	require.Equal(t, 1, bs.calls)
	require.Len(t, bs.entries, 2)
	assert.Equal(t, "a", bs.entries[0].ID)
	assert.Equal(t, "b", bs.entries[1].ID)

	// The summary is appended to the departed branch as a SessionEntry.
	summaryEntry := findSummaryEntry(t, tree)
	require.NotNil(t, summaryEntry)
	assert.Equal(t, "b", summaryEntry.ParentID)
	assert.Equal(t, EntryTypeSystem, summaryEntry.Type)
	assert.Equal(t, "DEPARTED-SUM", summaryEntry.Content)
	assert.True(t, summaryEntry.IsSummary)
}

// BuildContext reconstructing the departed branch includes the summary.
func TestTree_BuildContextIncludesBranchSummary(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()

	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(context.Background(), "b"))
	tree.SetBranchSummary(&testBranchSummary{summary: "DEPARTED-SUM"})
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(context.Background(), "c"))

	summaryEntry := findSummaryEntry(t, tree)
	require.NotNil(t, summaryEntry)

	sc, err := tree.BuildContext(context.Background(), summaryEntry.ID)
	require.NoError(t, err)
	found := false
	for _, m := range sc.Messages {
		if strings.Contains(m.Content, "DEPARTED-SUM") {
			found = true
		}
	}
	assert.True(t, found, "branch summary should appear in reconstructed context")
}

// No configured BranchSummary means MoveTo is unchanged (backward compatible):
// no summary entry is appended.
func TestTree_MoveToWithoutBranchSummaryUnchanged(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tree := newConcreteTree()
	require.NoError(t, tree.Append(context.Background(), newTestEntry("a", "", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("b", "a", EntryTypeUser)))
	require.NoError(t, tree.Append(context.Background(), newTestEntry("c", "b", EntryTypeUser)))
	require.NoError(t, tree.MoveTo(context.Background(), "c"))

	for _, e := range tree.entries {
		assert.False(t, e.IsSummary, "no summary should be appended without a BranchSummary")
	}
	assert.Equal(t, uint64(0), tree.summarySeq.Load())
}

// a compaction span is emitted with strategy_used=branch_summary,
// a consistent trace_id, and a traceable parent_span_id.
func TestDefaultBranchSummary_EmitsCompactionSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	exp := mock.NewMockTraceExporter()
	tr := tracing.NewTracer("test-trace-id", exp)
	root, ctx := tr.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	d := NewDefaultBranchSummary(func(_ context.Context, _ string) (string, error) { return "S", nil })
	_, err := d.Summarize(ctx, []SessionEntry{{ID: "a", Content: "x"}})
	require.NoError(t, err)
	rootID := root.SpanID()
	root.End()

	require.Eventually(t, func() bool {
		for _, sp := range exp.Spans() {
			if sp.Name == "compaction.branch_summary" {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)

	// The root span is exported asynchronously (localSpan.End spawns a
	// goroutine), so await it before checking chain integrity to avoid a race
	// between the compaction span landing and the root still being in flight.
	require.Eventually(t, func() bool {
		for _, sp := range exp.Spans() {
			if sp.SpanID == rootID {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)

	var target tracing.SpanData
	for _, sp := range exp.Spans() {
		if sp.Name == "compaction.branch_summary" {
			target = sp
		}
	}
	require.NotEmpty(t, target.SpanID)
	assert.Equal(t, "test-trace-id", target.TraceID)
	assert.Equal(t, root.SpanID(), target.ParentSpanID)

	strategyFound := false
	for _, a := range target.Attributes {
		switch a.Key {
		case "strategy_used":
			assert.Equal(t, "branch_summary", a.Value)
			strategyFound = true
		case "entry_count":
			assert.Equal(t, 1, a.Value)
		case "summary_length":
			assert.Equal(t, 1, a.Value)
		}
	}
	assert.True(t, strategyFound, "strategy_used=branch_summary attribute present")
	exp.AssertSpanChain(t)
}

// findSummaryEntry returns a copy of the (single) is-summary entry on the tree.
func findSummaryEntry(t *testing.T, tree *DefaultSessionTree) *SessionEntry {
	t.Helper()
	tree.mu.RLock()
	defer tree.mu.RUnlock()
	for _, e := range tree.entries {
		if e.IsSummary {
			return e.clone()
		}
	}
	return nil
}

func TestBranchSummaryRegistry(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	orig := GetBranchSummary()
	defer RegisterBranchSummary(orig)

	RegisterBranchSummary(nil)
	got := GetBranchSummary()
	require.NotNil(t, got)
	assert.Equal(t, "default-branch-summary", got.Name())

	custom := &testBranchSummary{summary: "reg"}
	RegisterBranchSummary(custom)
	assert.Same(t, custom, GetBranchSummary())
}
