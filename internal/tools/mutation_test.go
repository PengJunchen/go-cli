package tools

import (
	"context"
	"fmt"
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
	return func(_ context.Context, m FileMutation) (*ToolResult, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		key := m.FilePath
		if h.keyFn != nil {
			key = h.keyFn(m)
		}
		h.order = append(h.order, key)
		return nil, nil
	}
}

func (h *trackingHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.order))
	copy(out, h.order)
	return out
}

// closeQueue is a test helper that type-asserts the FileMutationQueue to
// *DefaultFileMutationQueue and calls Close. It fails the test if the
// assertion fails.
func closeQueue(t *testing.T, q FileMutationQueue) {
	t.Helper()
	cq, ok := q.(*DefaultFileMutationQueue)
	require.True(t, ok, "expected *DefaultFileMutationQueue")
	require.NoError(t, cq.Close())
}

func TestMutationQueueName(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := NewDefaultFileMutationQueue()
	defer closeQueue(t, q)

	assert.Equal(t, "file-mutation-queue", q.Name())
}

func TestMutationQueuePerFileFIFO(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Use keyFn to record the Content (op-1, op-2, op-3) as the key so the
	// handler's order slice captures the actual processing order.
	handler := &trackingHandler{
		keyFn: func(m FileMutation) string {
			if s, ok := m.Content.(string); ok {
				return s
			}
			return m.FilePath
		},
	}
	q := NewDefaultFileMutationQueue(WithMutationHandler(handler.handle()))
	defer closeQueue(t, q)

	// All three mutations target the SAME file via one shared path so they are
	// routed to the same per-file worker.
	path := filepath.Join(t.TempDir(), "serialized-fifo-target.txt")

	for _, op := range []string{"op-1", "op-2", "op-3"} {
		resCh, err := q.Enqueue(context.Background(), FileMutation{
			FilePath: path, Operation: "write", Content: op, ToolName: "write",
		})
		require.NoError(t, err)
		require.True(t, (<-resCh).Success)
	}

	// The handler records the actual processing order; since all three
	// mutations target the same file they must be processed in FIFO order.
	keys := handler.snapshot()
	require.Len(t, keys, 3)
	assert.Equal(t, []string{"op-1", "op-2", "op-3"}, keys, "mutations to the same file must apply in FIFO order")
}

func TestMutationQueueCrossFileParallelism(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var active, maxActive, applied int64
	handler := MutationHandler(func(_ context.Context, _ FileMutation) (*ToolResult, error) {
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
		return nil, nil
	})

	q := NewDefaultFileMutationQueue(WithMutationHandler(handler))
	defer closeQueue(t, q)

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
	cq := q.(*DefaultFileMutationQueue) //nolint:errcheck
	defer func() { require.NoError(t, cq.Close()) }()

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
	cq.workers.Range(func(_, _ any) bool { workerCount++; return true })
	assert.Equal(t, 1, workerCount, "symlink and real-path mutations must serialize on one worker")

	// FIFO on the shared worker: the later (real-path) write wins on disk.
	data, rerr := os.ReadFile(realFile)
	require.NoError(t, rerr)
	assert.Equal(t, "via-real", string(data))
}

func TestMutationQueueResultChannel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := NewDefaultFileMutationQueue()
	defer closeQueue(t, q)

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

	q := NewDefaultFileMutationQueue(WithMutationHandler(func(_ context.Context, _ FileMutation) (*ToolResult, error) {
		return nil, assert.AnError
	}))
	defer closeQueue(t, q)

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
	defer closeQueue(t, q)

	resCh, err := q.Enqueue(ctx, FileMutation{FilePath: filepath.Join(t.TempDir(), "span.txt"), Operation: "write", Content: "span", ToolName: "write"})
	require.NoError(t, err)
	require.True(t, (<-resCh).Success)

	root.SetStatus(tracing.SpanStatusOK, "")
	root.End()

	require.Eventually(t, func() bool { return e.hasSpan("tool.call") }, 2*time.Second, 10*time.Millisecond)
}

func TestMutationQueueCloseStopsEnqueue(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := NewDefaultFileMutationQueue()
	cq := q.(*DefaultFileMutationQueue) //nolint:errcheck
	require.NoError(t, cq.Close())
	require.Error(t, cq.Close(), "closing twice should error")

	_, err := q.Enqueue(context.Background(), FileMutation{FilePath: "/tmp/closed.txt", Operation: "write", Content: "x", ToolName: "write"})
	assert.Error(t, err, "enqueue on a closed queue must fail")
}

func TestMutationQueueConcurrentEnqueueCloseNoPanic(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var started sync.WaitGroup
	started.Add(1)

	q := NewDefaultFileMutationQueue(WithMutationHandler(func(_ context.Context, _ FileMutation) (*ToolResult, error) {
		started.Wait()
		time.Sleep(5 * time.Millisecond)
		return nil, nil
	}))
	cq := q.(*DefaultFileMutationQueue) //nolint:errcheck

	dir := t.TempDir()
	const enqueuers = 8

	var (
		wg      sync.WaitGroup
		panics  int64
		success int64
		errors  int64
	)

	for i := 0; i < enqueuers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			started.Wait()
			path := filepath.Join(dir, fmt.Sprintf("file-%d.txt", i%3))
			resCh, err := q.Enqueue(context.Background(), FileMutation{
				FilePath: path, Operation: "write", Content: "x", ToolName: "write",
			})
			if err != nil {
				atomic.AddInt64(&errors, 1)
				return
			}
			<-resCh
			atomic.AddInt64(&success, 1)
		}(i)
	}

	started.Done()
	time.Sleep(2 * time.Millisecond)

	require.NoError(t, cq.Close(), "Close must not error")

	wg.Wait()

	total := atomic.LoadInt64(&success) + atomic.LoadInt64(&errors) + atomic.LoadInt64(&panics)
	assert.Equal(t, int64(enqueuers), total, "every enqueuer must finish without panic")
	assert.Zero(t, atomic.LoadInt64(&panics), "no goroutine should panic on send to closed channel")
}

func TestMutationQueueHandlerPanicDoesNotDeadlock(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var callCount int64
	q := NewDefaultFileMutationQueue(WithMutationHandler(func(_ context.Context, m FileMutation) (*ToolResult, error) {
		cur := atomic.AddInt64(&callCount, 1)
		if cur == 1 {
			panic("handler boom")
		}
		return nil, nil
	}))
	defer closeQueue(t, q)

	path := filepath.Join(t.TempDir(), "panic-test.txt")

	resCh1, err := q.Enqueue(context.Background(), FileMutation{
		FilePath: path, Operation: "write", Content: "first", ToolName: "write",
	})
	require.NoError(t, err)

	res1 := <-resCh1
	assert.False(t, res1.Success, "panicked mutation must report failure")
	assert.Contains(t, res1.Error.Error(), "handler panic", "error must mention handler panic")

	resCh2, err := q.Enqueue(context.Background(), FileMutation{
		FilePath: path, Operation: "write", Content: "second", ToolName: "write",
	})
	require.NoError(t, err, "enqueue after panic must succeed (no deadlock)")

	res2 := <-resCh2
	assert.True(t, res2.Success, "mutation after panic must succeed")
}

func TestMutationMiddlewarePassthroughAndQueued(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	next := func(_ context.Context, call ToolCall) (*ToolResult, error) {
		return &ToolResult{Output: "ran:" + call.Name, Metadata: map[string]any{"path": "seen"}}, nil
	}

	q := NewDefaultFileMutationQueue(WithMutationHandler(func(_ context.Context, _ FileMutation) (*ToolResult, error) {
		return nil, nil
	}))
	defer closeQueue(t, q)

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

func TestMutationQueueDefaultHandlerWriteAndEdit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()

	// Use the built-in default handler (no WithMutationHandler override).
	q := NewDefaultFileMutationQueue()
	defer closeQueue(t, q)

	// Write a file via the default handler.
	writePath := filepath.Join(dir, "handler-write.txt")
	resCh, err := q.Enqueue(context.Background(), FileMutation{
		FilePath: writePath, Operation: "write", Content: "hello", ToolName: "write",
	})
	require.NoError(t, err)
	require.True(t, (<-resCh).Success)

	data, rerr := os.ReadFile(writePath)
	require.NoError(t, rerr)
	assert.Equal(t, "hello", string(data))

	// Edit the file via the default handler.
	resCh, err = q.Enqueue(context.Background(), FileMutation{
		FilePath: writePath, Operation: "edit", Content: map[string]any{
			"old_string": "hello",
			"new_string": "world",
		}, ToolName: "edit",
	})
	require.NoError(t, err)
	require.True(t, (<-resCh).Success)

	data, rerr = os.ReadFile(writePath)
	require.NoError(t, rerr)
	assert.Equal(t, "world", string(data))
}

func TestMutationQueueDefaultHandlerUnsupportedOperation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := NewDefaultFileMutationQueue()
	defer closeQueue(t, q)

	resCh, err := q.Enqueue(context.Background(), FileMutation{
		FilePath: "/tmp/unsupported.txt", Operation: "delete", Content: "", ToolName: "delete",
	})
	require.NoError(t, err)
	res := <-resCh
	assert.False(t, res.Success)
	assert.Contains(t, res.Error.Error(), "unsupported operation")
}

// recordingDiffGen is a test DiffGenerator that records whether Generate was
// called and with what arguments.
type recordingDiffGen struct {
	mu     sync.Mutex
	called bool
	old    string
	new    string
	path   string
}

func (r *recordingDiffGen) Generate(_ context.Context, oldContent, newContent, path string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = true
	r.old = oldContent
	r.new = newContent
	r.path = path
	return "mock-diff", nil
}

func (r *recordingDiffGen) wasCalled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.called
}

// TestMutationQueueConfiguredHandlerPreservesFileTracker verifies that when a
// FileTracker is injected via WithMutationFileTracker, the configured handler
// creates a backup checkpoint before writing—proving fileTracker is not lost.
func TestMutationQueueConfiguredHandlerPreservesFileTracker(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ft := NewFileTracker()
	q := NewDefaultFileMutationQueue(WithMutationFileTracker(ft))
	defer closeQueue(t, q)

	writePath := filepath.Join(t.TempDir(), "tracked.txt")
	resCh, err := q.Enqueue(context.Background(), FileMutation{
		FilePath: writePath, Operation: "write", Content: "hello", ToolName: "write",
	})
	require.NoError(t, err)
	require.True(t, (<-resCh).Success)

	checkpoints := ft.ListCheckpoints()
	require.Len(t, checkpoints, 1, "fileTracker must create a backup checkpoint")
	assert.Equal(t, writePath, checkpoints[0].Path)
}

// TestMutationQueueConfiguredHandlerPreservesDiffGenerator verifies that when a
// DiffGenerator is injected via WithMutationDiffGenerator, the configured
// handler generates a diff when overwriting an existing file—proving
// diffGenerator is not lost.
func TestMutationQueueConfiguredHandlerPreservesDiffGenerator(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	writePath := filepath.Join(dir, "existing.txt")
	require.NoError(t, os.WriteFile(writePath, []byte("old"), 0o600))

	dg := &recordingDiffGen{}
	q := NewDefaultFileMutationQueue(WithMutationDiffGenerator(dg))
	defer closeQueue(t, q)

	resCh, err := q.Enqueue(context.Background(), FileMutation{
		FilePath: writePath, Operation: "write", Content: "new", ToolName: "write",
	})
	require.NoError(t, err)
	require.True(t, (<-resCh).Success)

	assert.True(t, dg.wasCalled(), "diffGenerator.Generate must be called for overwrite")
	assert.Equal(t, "old", dg.old)
	assert.Equal(t, "new", dg.new)
}

// TestNewMutationQueueWrapper_MutationToolQueued verifies that
// NewMutationQueueWrapper returns a ToolExecutorWrapper that routes write/edit
// calls through the FileMutationQueue instead of calling next directly.
func TestNewMutationQueueWrapper_MutationToolQueued(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var handlerCalled bool
	q := NewDefaultFileMutationQueue(WithMutationHandler(func(_ context.Context, m FileMutation) (*ToolResult, error) {
		handlerCalled = true
		assert.Equal(t, "write", m.Operation)
		assert.Equal(t, "/tmp/wrapper-queued.txt", m.FilePath)
		return nil, nil
	}))
	defer closeQueue(t, q)

	wrapper := NewMutationQueueWrapper(q)
	wrapped := wrapper(func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run for mutation tools")
		return nil, nil
	})

	res, err := wrapped(context.Background(), ToolCall{
		Name: "write",
		Args: map[string]any{"path": "/tmp/wrapper-queued.txt", "content": "data"},
	})
	require.NoError(t, err)
	assert.True(t, handlerCalled, "queue handler must be invoked")
	assert.Contains(t, res.Output, "queued and applied")
}

// TestNewMutationQueueWrapper_NonMutationPassthrough verifies that
// NewMutationQueueWrapper passes non-mutation tool calls straight through to
// next without touching the queue.
func TestNewMutationQueueWrapper_NonMutationPassthrough(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := NewDefaultFileMutationQueue()
	defer closeQueue(t, q)

	wrapper := NewMutationQueueWrapper(q)
	nextCalled := false
	wrapped := wrapper(func(_ context.Context, call ToolCall) (*ToolResult, error) {
		nextCalled = true
		return &ToolResult{Output: "passthrough:" + call.Name}, nil
	})

	res, err := wrapped(context.Background(), ToolCall{
		Name: "read",
		Args: map[string]any{"path": "/tmp/wrapper-passthrough.txt"},
	})
	require.NoError(t, err)
	assert.True(t, nextCalled, "next must run for non-mutation tools")
	assert.Equal(t, "passthrough:read", res.Output)
}

// =============================================================================
// ToolResult preservation tests (task 35-11)
// =============================================================================

// TestWithMutationQueue_PreservesWriteToolResult verifies that a write through
// the mutation queue returns the real WriteTool ToolResult-including path,
// bytes, and diff preview in Metadata-instead of a synthesized placeholder.
func TestWithMutationQueue_PreservesWriteToolResult(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	writePath := filepath.Join(dir, "overwrite.txt")
	require.NoError(t, os.WriteFile(writePath, []byte("old"), 0o600))

	dg := &recordingDiffGen{}
	q := NewDefaultFileMutationQueue(WithMutationDiffGenerator(dg))
	defer closeQueue(t, q)

	wrapped := WithMutationQueue(q, func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run for mutation tools")
		return nil, nil
	})

	res, err := wrapped(context.Background(), ToolCall{
		Name: "write",
		Args: map[string]any{"path": writePath, "content": "new"},
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	// The real WriteTool output format, not the synthesized "queued and applied".
	assert.Contains(t, res.Output, "wrote", "must carry real WriteTool output")
	assert.Contains(t, res.Output, writePath)

	// Metadata must carry the real tool's keys plus the "queued" marker.
	assert.Equal(t, writePath, res.Metadata["path"])
	assert.NotNil(t, res.Metadata["bytes"])
	assert.Equal(t, "mock-diff", res.Metadata["diff"], "diff preview must be preserved")
	assert.Equal(t, true, res.Metadata["queued"])
}

// TestWithMutationQueue_PreservesEditToolResult verifies that an edit through
// the mutation queue returns the real EditFileTool ToolResult with the diff
// preview in Metadata.
func TestWithMutationQueue_PreservesEditToolResult(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	editPath := filepath.Join(dir, "edit.txt")
	require.NoError(t, os.WriteFile(editPath, []byte("hello world"), 0o600))

	dg := &recordingDiffGen{}
	q := NewDefaultFileMutationQueue(WithMutationDiffGenerator(dg))
	defer closeQueue(t, q)

	wrapped := WithMutationQueue(q, func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run for mutation tools")
		return nil, nil
	})

	res, err := wrapped(context.Background(), ToolCall{
		Name: "edit",
		Args: map[string]any{"file_path": editPath, "old_string": "hello", "new_string": "goodbye"},
	})
	require.NoError(t, err)
	require.NotNil(t, res)

	// The real EditFileTool output format.
	assert.Contains(t, res.Output, "replaced", "must carry real EditFileTool output")
	assert.Contains(t, res.Output, editPath)

	assert.Equal(t, editPath, res.Metadata["path"])
	assert.NotNil(t, res.Metadata["bytes"])
	assert.Equal(t, "mock-diff", res.Metadata["diff"], "diff preview must be preserved")
	assert.Equal(t, true, res.Metadata["queued"])
}

// TestWithMutationQueue_ErrorPropagatesToolResult verifies that when an edit
// fails (non-existent old_string), the error propagates and the returned
// ToolResult is nil (no synthesized success result).
func TestWithMutationQueue_ErrorPropagatesToolResult(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	editPath := filepath.Join(dir, "err.txt")
	require.NoError(t, os.WriteFile(editPath, []byte("hello"), 0o600))

	q := NewDefaultFileMutationQueue()
	defer closeQueue(t, q)

	wrapped := WithMutationQueue(q, func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run for mutation tools")
		return nil, nil
	})

	res, err := wrapped(context.Background(), ToolCall{
		Name: "edit",
		Args: map[string]any{"file_path": editPath, "old_string": "nonexistent", "new_string": "x"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "old_string not found")
	assert.Nil(t, res, "error branch must return nil ToolResult")
}

// TestMutationHandler_ReturnsToolResult verifies that a custom MutationHandler
// returning a specific ToolResult has it carried through FileMutationResult.
func TestMutationHandler_ReturnsToolResult(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	custom := &ToolResult{Output: "custom-output", Metadata: map[string]any{"k": "v"}}
	q := NewDefaultFileMutationQueue(WithMutationHandler(func(_ context.Context, _ FileMutation) (*ToolResult, error) {
		return custom, nil
	}))
	defer closeQueue(t, q)

	resCh, err := q.Enqueue(context.Background(), FileMutation{
		FilePath: "/tmp/custom.txt", Operation: "write", Content: "x", ToolName: "write",
	})
	require.NoError(t, err)
	res := <-resCh
	assert.True(t, res.Success)
	assert.NoError(t, res.Error)
	require.NotNil(t, res.ToolResult, "FileMutationResult.ToolResult must carry handler result")
	assert.Equal(t, "custom-output", res.ToolResult.Output)
	assert.Equal(t, "v", res.ToolResult.Metadata["k"])
}

// TestWithMutationQueue_NonMutationPassThrough verifies that non-mutation
// tools (e.g. read) pass straight through to next without "queued" metadata.
func TestWithMutationQueue_NonMutationPassThrough(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := NewDefaultFileMutationQueue()
	defer closeQueue(t, q)

	wrapped := WithMutationQueue(q, func(_ context.Context, call ToolCall) (*ToolResult, error) {
		return &ToolResult{Output: "read-done", Metadata: map[string]any{"path": "x"}}, nil
	})

	res, err := wrapped(context.Background(), ToolCall{Name: "read", Args: map[string]any{"path": "/tmp/x"}})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "read-done", res.Output)
	_, hasQueued := res.Metadata["queued"]
	assert.False(t, hasQueued, "non-mutation passthrough must not add queued metadata")
}

// TestWithMutationQueue_MetadataNotMutated verifies that the shallow copy of
// Metadata isolates results across calls: modifying the first call's Metadata
// does not pollute the second call's Metadata.
func TestWithMutationQueue_MetadataNotMutated(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// The handler reuses the same underlying Metadata map to simulate a tool
	// that returns a persistent map. The shallow copy in WithMutationQueue must
	// ensure mutations to the returned Metadata don't leak back.
	sharedMeta := map[string]any{"path": "/tmp/iso.txt", "bytes": 4}
	q := NewDefaultFileMutationQueue(WithMutationHandler(func(_ context.Context, _ FileMutation) (*ToolResult, error) {
		return &ToolResult{Output: "ok", Metadata: sharedMeta}, nil
	}))
	defer closeQueue(t, q)

	wrapped := WithMutationQueue(q, func(_ context.Context, _ ToolCall) (*ToolResult, error) {
		t.Fatal("next must not run for mutation tools")
		return nil, nil
	})

	// First call: get result and pollute its Metadata.
	res1, err := wrapped(context.Background(), ToolCall{Name: "write", Args: map[string]any{"path": "/tmp/iso.txt", "content": "data"}})
	require.NoError(t, err)
	require.NotNil(t, res1)
	assert.Equal(t, true, res1.Metadata["queued"])
	res1.Metadata["polluted"] = true

	// The underlying sharedMeta must not have been polluted with "queued".
	_, sharedQueued := sharedMeta["queued"]
	assert.False(t, sharedQueued, "shallow copy must not write 'queued' into the underlying map")

	// Second call: Metadata must be clean (no "polluted" key from first call).
	res2, err := wrapped(context.Background(), ToolCall{Name: "write", Args: map[string]any{"path": "/tmp/iso.txt", "content": "data"}})
	require.NoError(t, err)
	require.NotNil(t, res2)
	assert.Equal(t, true, res2.Metadata["queued"])
	_, polluted := res2.Metadata["polluted"]
	assert.False(t, polluted, "shallow copy must isolate Metadata between calls")
}

// TestApplySafe_PanicReturnsNilToolResult verifies that when the handler
// panics, FileMutationResult.ToolResult is nil and the Error contains panic
// information.
func TestApplySafe_PanicReturnsNilToolResult(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	q := NewDefaultFileMutationQueue(WithMutationHandler(func(_ context.Context, _ FileMutation) (*ToolResult, error) {
		panic("boom")
	}))
	defer closeQueue(t, q)

	resCh, err := q.Enqueue(context.Background(), FileMutation{
		FilePath: "/tmp/panic.txt", Operation: "write", Content: "x", ToolName: "write",
	})
	require.NoError(t, err)
	res := <-resCh
	assert.False(t, res.Success)
	require.Error(t, res.Error)
	assert.Contains(t, res.Error.Error(), "panic", "error must contain panic info")
	assert.Nil(t, res.ToolResult, "ToolResult must be nil on panic")
}
