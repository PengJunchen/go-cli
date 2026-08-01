package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// trackingHandler records the order of applied mutations so tests can assert
// per-file FIFO ordering and cross-file parallelism.
type trackingHandler struct {
	mu    sync.Mutex
	order []string
	keyFn func(FileMutation) string
}

func (h *trackingHandler) handle() MutationHandler {
	return func(_ context.Context, m FileMutation) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		key := m.FilePath
		if h.keyFn != nil {
			key = h.keyFn(m)
		}
		h.order = append(h.order, key)
		return nil
	}
}

func (h *trackingHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.order))
	copy(out, h.order)
	return out
}

func TestMutationQueueName(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := NewDefaultFileMutationQueue()
	defer func() { require.NoError(t, q.Close()) }()

	assert.Equal(t, "file-mutation-queue", q.Name())
}

func TestMutationQueuePerFileFIFO(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	handler := &trackingHandler{}
	q := NewDefaultFileMutationQueue(WithMutationHandler(handler.handle()))
	defer func() { require.NoError(t, q.Close()) }()

	// All three mutations target the SAME file via one shared path so they are
	// routed to the same per-file worker.
	path := filepath.Join(t.TempDir(), "serialized-fifo-target.txt")

	var mu sync.Mutex
	var completed []string
	for _, op := range []string{"op-1", "op-2", "op-3"} {
		resCh, err := q.Enqueue(context.Background(), FileMutation{
			FilePath: path, Operation: "write", Content: op, ToolName: "write",
		})
		require.NoError(t, err)
		go func(op string, resCh <-chan FileMutationResult) {
			require.True(t, (<-resCh).Success)
			mu.Lock()
			defer mu.Unlock()
			completed = append(completed, op)
		}(op, resCh)
	}

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(completed) == 3
	}, 2*time.Second, 10*time.Millisecond)

	mu.Lock()
	got := append([]string(nil), completed...)
	mu.Unlock()
	assert.Equal(t, []string{"op-1", "op-2", "op-3"}, got, "mutations to the same file must apply in FIFO order")

	// Same worker: every handler observation carries one shared path.
	keys := handler.snapshot()
	require.Len(t, keys, 3)
	assert.Equal(t, keys[0], keys[1], "op-1 and op-2 landed on the same file worker")
	assert.Equal(t, keys[1], keys[2], "op-2 and op-3 landed on the same file worker")
}

func TestMutationQueueCrossFileParallelism(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var active, maxActive, applied int64
	handler := MutationHandler(func(_ context.Context, _ FileMutation) error {
		cur := atomic.AddInt64(&active, 1)
		for {
			old := atomic.LoadInt64(&maxActive)
			if cur <= old || atomic.CompareAndSwapInt64(&maxActive, old, cur) {
				break
			}
		}
		defer atomic.AddInt64(&active, -1)
		atomic.AddInt64(&applied, 1)
		time.Sleep(30 * time.Millisecond)
		return nil
	})

	q := NewDefaultFileMutationQueue(WithMutationHandler(handler))
	defer func() { require.NoError(t, q.Close()) }()

	const (
		pathA = "parallel-a.txt"
		pathB = "parallel-b.txt"
	)
	dir := t.TempDir()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resCh, err := q.Enqueue(context.Background(), FileMutation{FilePath: filepath.Join(dir, pathA), Operation: "write", Content: "a", ToolName: "write"})
			require.NoError(t, err)
			require.True(t, (<-resCh).Success)
		}()
		go func() {
			defer wg.Done()
			resCh, err := q.Enqueue(context.Background(), FileMutation{FilePath: filepath.Join(dir, pathB), Operation: "write", Content: "b", ToolName: "write"})
			require.NoError(t, err)
			require.True(t, (<-resCh).Success)
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(8), atomic.LoadInt64(&applied), "all mutations applied")
	assert.GreaterOrEqual(t, atomic.LoadInt64(&maxActive), int64(2), "distinct file workers ran concurrently")
}

func TestMutationQueueRealpathSymlink(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}

	dir := t.TempDir()
	realFile := filepath.Join(dir, "real-target.txt")
	require.NoError(t, os.WriteFile(realFile, []byte("original"), 0o600))

	link := filepath.Join(dir, "link-target.txt")
	require.NoError(t, os.Symlink(realFile, link))

	// Unit-level: realpath resolution maps the symlink and the real path to the
	// same canonical location.
	require.Equal(t, resolveRealPath(link), resolveRealPath(realFile),
		"symlink path must resolve to the same real path as the target")

	// Use the built-in handler so mutations perform real writes against the
	// underlying file, proving FIFO serialization on a shared worker.
	q := NewDefaultFileMutationQueue()
	defer func() { require.NoError(t, q.Close()) }()

	// Enqueue via the symlink path and then via the real path.
	resCh1, err := q.Enqueue(context.Background(), FileMutation{FilePath: link, Operation: "write", Content: "via-link", ToolName: "write"})
	require.NoError(t, err)
	resCh2, err := q.Enqueue(context.Background(), FileMutation{FilePath: realFile, Operation: "write", Content: "via-real", ToolName: "write"})
	require.NoError(t, err)

	require.True(t, (<-resCh1).Success)
	require.True(t, (<-resCh2).Success)

	// Both mutations routed to the SAME per-file worker keyed by the resolved
	// real path, so the queue keeps exactly one worker despite two distinct
	// original paths.
	workerCount := 0
	q.workers.Range(func(_, _ any) bool { workerCount++; return true })
	assert.Equal(t, 1, workerCount, "symlink and real-path mutations must serialize on one worker")

	// FIFO on the shared worker: the later (real-path) write wins on disk.
	data, rerr := os.ReadFile(realFile)
	require.NoError(t, rerr)
	assert.Equal(t, "via-real", string(data))
}

func TestMutationQueueResultChannel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := NewDefaultFileMutationQueue()
	defer func() { require.NoError(t, q.Close()) }()

	path := filepath.Join(t.TempDir(), "ok.txt")
	resCh, err := q.Enqueue(context.Background(), FileMutation{FilePath: path, Operation: "write", Content: "hi", ToolName: "write"})
	require.NoError(t, err)

	res, open := <-resCh
	assert.True(t, open, "result channel yields exactly one value")
	assert.True(t, res.Success)
	assert.NoError(t, res.Error)

	_, open = <-resCh
	assert.False(t, open, "result channel closes after the single result")
}

func TestMutationQueueErrorResult(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := NewDefaultFileMutationQueue(WithMutationHandler(func(_ context.Context, _ FileMutation) error {
		return assert.AnError
	}))
	defer func() { require.NoError(t, q.Close()) }()

	resCh, err := q.Enqueue(context.Background(), FileMutation{FilePath: filepath.Join(t.TempDir(), "err.txt"), Operation: "write", Content: "x", ToolName: "write"})
	require.NoError(t, err)
	res := <-resCh
	assert.False(t, res.Success)
	assert.ErrorIs(t, res.Error, assert.AnError)
}

func TestMutationQueueEmitsSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	e := &captureExporter{}
	tracer := tracing.NewTracer("trace-mutation-q", e)
	root, ctx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	q := NewDefaultFileMutationQueue()
	defer func() { require.NoError(t, q.Close()) }()

	resCh, err := q.Enqueue(ctx, FileMutation{FilePath: filepath.Join(t.TempDir(), "span.txt"), Operation: "write", Content: "span", ToolName: "write"})
	require.NoError(t, err)
	require.True(t, (<-resCh).Success)

	root.SetStatus(tracing.SpanStatusOK, "")
	root.End()

	require.Eventually(t, func() bool { return e.hasSpan("tools.mutation") }, 2*time.Second, 10*time.Millisecond)
}

func TestMutationQueueCloseStopsEnqueue(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := NewDefaultFileMutationQueue()
	require.NoError(t, q.Close())
	require.Error(t, q.Close(), "closing twice should error")

	_, err := q.Enqueue(context.Background(), FileMutation{FilePath: "/tmp/closed.txt", Operation: "write", Content: "x", ToolName: "write"})
	assert.Error(t, err, "enqueue on a closed queue must fail")
}

func TestMutationMiddlewarePassthroughAndQueued(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	next := func(_ context.Context, call ToolCall) (*ToolResult, error) {
		return &ToolResult{Output: "ran:" + call.Name, Metadata: map[string]any{"path": "seen"}}, nil
	}

	q := NewDefaultFileMutationQueue(WithMutationHandler(func(_ context.Context, _ FileMutation) error {
		return nil
	}))
	defer func() { require.NoError(t, q.Close()) }()

	wrapped := WithMutationQueue(q, next)

	// Non-mutation tool passes straight through.
	res, err := wrapped(context.Background(), ToolCall{Name: "grep", Args: map[string]any{"pattern": "x"}})
	require.NoError(t, err)
	assert.Equal(t, "ran:grep", res.Output)

	// Write mutation is queued and applied via the queue handler.
	res, err = wrapped(context.Background(), ToolCall{Name: "write", Args: map[string]any{"path": "mw.txt", "content": "data"}})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "queued and applied", "mutation route reported queued application")
	queued, ok := res.Metadata["queued"].(bool)
	assert.True(t, ok && queued)
}
