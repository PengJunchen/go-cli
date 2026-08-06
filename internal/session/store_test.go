package session

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// newTestEntry builds a well-formed entry with a stable id/content/timestamp.
func newTestEntry(id, parentID string, entryType EntryType) *SessionEntry {
	return &SessionEntry{
		ID:        id,
		ParentID:  parentID,
		Type:      entryType,
		Content:   "content-" + id,
		Timestamp: time.Date(2024, 5, 1, 12, 0, int(id[0]), 0, time.UTC),
	}
}

// tracedCtx returns a context carrying a real tracer backed by a mock exporter,
// and the exporter for span assertions. The root span is ended at cleanup.
func tracedCtx(t *testing.T) (context.Context, *mock.MockTraceExporter) {
	t.Helper()
	exp := mock.NewMockTraceExporter()
	tr := tracing.NewTracer("test-trace-id", exp)
	root, ctx := tr.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)
	t.Cleanup(root.End)
	return ctx, exp
}

func TestStore_AppendGetRoundTrip(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	for _, s := range []SessionStore{
		NewDefaultSessionStore(),
		NewMemoryStore(),
	} {
		entry := newTestEntry("e1", "", EntryTypeUser)
		require.NoError(t, s.Append(context.Background(), entry))

		got, err := s.Get(context.Background(), "e1")
		require.NoError(t, err)
		assert.Equal(t, entry.ID, got.ID)
		assert.Equal(t, entry.Type, got.Type)
		assert.Equal(t, entry.Content, got.Content)
	}
}

func TestStore_AppendNilOrInvalid(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSessionStore()
	require.Error(t, s.Append(context.Background(), nil))
	require.Error(t, s.Append(context.Background(), &SessionEntry{Type: EntryTypeUser}))
	require.Error(t, s.Append(context.Background(), &SessionEntry{ID: "x"}))
}

func TestStore_AppendNoOverwrite(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSessionStore()
	first := newTestEntry("e1", "", EntryTypeUser)
	require.NoError(t, s.Append(context.Background(), first))

	// A second append with the same id must fail and must not overwrite.
	dup := newTestEntry("e1", "", EntryTypeAssistant)
	err := s.Append(context.Background(), dup)
	require.Error(t, err)

	got, err := s.Get(context.Background(), "e1")
	require.NoError(t, err)
	assert.Equal(t, EntryTypeUser, got.Type)
}

func TestStore_GetNotFound(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSessionStore()
	got, err := s.Get(context.Background(), "missing")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Nil(t, got)
}

func TestStore_GetReturnsCopy(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSessionStore()
	require.NoError(t, s.Append(context.Background(), newTestEntry("e1", "", EntryTypeUser)))

	got, err := s.Get(context.Background(), "e1")
	require.NoError(t, err)
	got.Content = "mutated"

	again, err := s.Get(context.Background(), "e1")
	require.NoError(t, err)
	assert.Equal(t, "content-e1", again.Content)
}

func TestStore_SaveNoOp(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSessionStore()
	require.NoError(t, s.Append(context.Background(), newTestEntry("e1", "", EntryTypeUser)))
	require.NoError(t, s.Save(context.Background()))
	_, err := s.Get(context.Background(), "e1")
	require.NoError(t, err)
}

func TestStore_ConcurrentAppendGet(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	s := NewDefaultSessionStore()
	const n = 100

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("conc-%d", i)
			require.NoError(t, s.Append(context.Background(), newTestEntry(id, "", EntryTypeUser)))
			got, err := s.Get(context.Background(), id)
			require.NoError(t, err)
			assert.Equal(t, id, got.ID)
		}(i)
	}
	wg.Wait()
}

func TestStore_TraceSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := tracedCtx(t)
	s := NewDefaultSessionStore()
	require.NoError(t, s.Append(ctx, newTestEntry("e1", "", EntryTypeUser)))
	_, err := s.Get(ctx, "e1")
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return exp.SpanCount() >= 2
	}, 2*time.Second, 5*time.Millisecond)

	validateSpan(t, exp, "session.save")
	validateSpan(t, exp, "session.load")
}

// validateSpan waits for a span with the given name to be exported.
func validateSpan(t *testing.T, exp *mock.MockTraceExporter, name string) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, sp := range exp.Spans() {
			if sp.Name == name {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)
}
