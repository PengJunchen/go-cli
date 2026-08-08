package tools

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

const (
	// mutationQueueName is the canonical name reported by DefaultFileMutationQueue.
	mutationQueueName = "file-mutation-queue"
	// mutationSpanName identifies the tracing span emitted per enqueued mutation.
	mutationSpanName = "tool.call"
)

// FileMutation is a single ordered write/edit mutation targeting a file. It is
// the unit of work enqueued onto a FileMutationQueue. By resolving FilePath to
// a real path, separate Enqueue calls referring to the same underlying file
// (via symlink or hard link) serialize onto the same per-file worker.
type FileMutation struct {
	// FilePath is the path of the file the mutation applies to. It is resolved
	// to its real path before being routed to a per-file worker.
	FilePath string `json:"file_path"`
	// Operation is the kind of mutation, e.g. "write" or "edit".
	Operation string `json:"operation"`
	// Content holds the mutation payload. It is a string for "write" and a
	// map[string]any{old_string,new_string} for "edit".
	Content any `json:"content,omitempty"`
	// ToolName is the name of the tool that produced the mutation.
	ToolName string `json:"tool_name,omitempty"`
}

// FileMutationResult is the outcome of applying a single queued mutation.
type FileMutationResult struct {
	// Success reports whether the mutation applied cleanly.
	Success bool `json:"success"`
	// Error holds the error that prevented the mutation from applying, if any.
	Error error `json:"-"`
	// ToolResult carries the underlying tool's ToolResult (e.g. write/edit)
	// so callers like WithMutationQueue can surface the real output and
	// metadata (path, bytes, diff) instead of a synthesized placeholder.
	// It is nil when the handler did not produce one (e.g. on error/panic).
	ToolResult *ToolResult `json:"-"`
}

// FileMutationQueue serializes write/edit mutations per file so that mutations
// targeting the same file apply in FIFO order, while different files may apply
// in parallel. Enqueue returns a receive-only channel that yields exactly one
// FileMutationResult once the mutation has been applied (or failed).
type FileMutationQueue interface {
	// Enqueue resolves the mutation's file path and routes it to that file's
	// serialized worker, returning a result channel. It blocks only until the
	// mutation has been accepted by the worker, not until it is applied.
	Enqueue(ctx context.Context, mutation FileMutation) (<-chan FileMutationResult, error)
	// Name returns a human-readable name for the queue.
	Name() string
}

// MutationHandler applies a single mutation. Workers call it to perform the
// actual write/edit against the filesystem (or via an underlying tool). It
// returns the tool's ToolResult so callers can surface the real output and
// metadata (path, bytes, diff) instead of a synthesized placeholder.
type MutationHandler func(ctx context.Context, m FileMutation) (*ToolResult, error)

// queuedMutation bundles a mutation with the result channel for that specific
// Enqueue call, so the file worker can route the outcome back to exactly one
// caller.
type queuedMutation struct {
	mutation FileMutation
	result   chan FileMutationResult
}

// DefaultFileMutationQueue routes each mutation to a per-file worker goroutine.
// Workers are created lazily on the first mutation for a given real path and
// reused for subsequent mutations, so per-file FIFO ordering is preserved and
// distinct files progress in parallel. Workers are long-lived; call Close (via
// a cleanup) when the queue is no longer needed so goroutines do not leak.
type DefaultFileMutationQueue struct {
	// workers maps a resolved real path to its serialized worker channel.
	workers sync.Map // map[string]chan queuedMutation
	// handler applies each mutation. When nil a built-in handler that delegates
	// to the write/edit tools is used.
	handler MutationHandler
	// mu guards closed and workers during shutdown.
	mu     sync.Mutex
	closed bool
}

var _ FileMutationQueue = (*DefaultFileMutationQueue)(nil)

// mutationQueueOptions holds the configurable fields of a mutation queue
// during construction. It is an internal construction aid so that
// MutationQueueOption closures do not reference the concrete
// DefaultFileMutationQueue type.
type mutationQueueOptions struct {
	handler       MutationHandler
	fileTracker   *FileTracker
	diffGenerator DiffGenerator
}

// MutationQueueOption configures a DefaultFileMutationQueue.
type MutationQueueOption func(*mutationQueueOptions)

// WithMutationHandler sets the handler used to apply queued mutations. When
// omitted, DefaultFileMutationQueue uses a handler that delegates to the
// built-in write/edit tools.
func WithMutationHandler(h MutationHandler) MutationQueueOption {
	return func(o *mutationQueueOptions) { o.handler = h }
}

// WithMutationFileTracker injects a FileTracker into the default mutation
// handler so that backup checkpoints are created before each write/edit. This
// preserves fileTracker functionality when mutations are queued. When a custom
// handler is set via WithMutationHandler, this option has no effect.
func WithMutationFileTracker(ft *FileTracker) MutationQueueOption {
	return func(o *mutationQueueOptions) { o.fileTracker = ft }
}

// WithMutationDiffGenerator injects a DiffGenerator into the default mutation
// handler so that change previews are generated for overwrites. This preserves
// diffGenerator functionality when mutations are queued. When a custom handler
// is set via WithMutationHandler, this option has no effect.
func WithMutationDiffGenerator(dg DiffGenerator) MutationQueueOption {
	return func(o *mutationQueueOptions) { o.diffGenerator = dg }
}

// NewDefaultFileMutationQueue returns a DefaultFileMutationQueue using the
// built-in write/edit handler unless overridden via WithMutationHandler. When
// fileTracker or diffGenerator options are provided, they are injected into the
// default handler so backup checkpoints and change previews are preserved.
func NewDefaultFileMutationQueue(opts ...MutationQueueOption) FileMutationQueue {
	o := &mutationQueueOptions{}
	for _, opt := range opts {
		opt(o)
	}
	handler := o.handler
	if handler == nil {
		handler = newConfiguredMutationHandler(o.fileTracker, o.diffGenerator)
	}
	return &DefaultFileMutationQueue{handler: handler}
}

// Name returns the queue's canonical name.
func (q *DefaultFileMutationQueue) Name() string { return mutationQueueName }

// resolveRealPath resolves p to its canonical location, following any symlinks
// in the path. When the path does not yet exist (e.g. a yet-to-be-created file)
// it is returned cleaned, so later Enqueue calls on the same logical path still
// route to the same worker.
func resolveRealPath(p string) string {
	clean := filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(clean); err == nil {
		return r
	}
	return clean
}

// Enqueue resolves the mutation's file path, finds or lazily creates the
// per-file worker, and hands the mutation off for FIFO processing. It returns a
// receive-only result channel that yields a single FileMutationResult.
func (q *DefaultFileMutationQueue) Enqueue(ctx context.Context, mutation FileMutation) (<-chan FileMutationResult, error) {
	span, _ := tracing.SpanFromContext(ctx, mutationSpanName, tracing.SpanKindClient)
	defer span.End()

	realPath := resolveRealPath(mutation.FilePath)
	span.SetAttributes(
		tracing.Attribute{Key: "file_path", Value: realPath},
		tracing.Attribute{Key: "operation", Value: mutation.Operation},
		tracing.Attribute{Key: "tool_name", Value: mutation.ToolName},
	)
	logger := tracing.NewTraceLogger(span, slog.Default())
	logger.Info("mutation.enqueue", "path", realPath, "operation", mutation.Operation)

	resultCh := make(chan FileMutationResult, 1)

	q.mu.Lock()
	input, err := q.workerForLocked(realPath)
	if err != nil {
		q.mu.Unlock()
		return nil, err
	}
	select {
	case input <- queuedMutation{mutation: mutation, result: resultCh}:
		q.mu.Unlock()
	case <-ctx.Done():
		q.mu.Unlock()
		return nil, fmt.Errorf("mutation: enqueue %s: %w", realPath, ctx.Err())
	}
	return resultCh, nil
}

// workerForLocked returns the worker channel for realPath, creating it lazily
// on the first use. If the queue is closed it returns an error. The caller must
// hold q.mu so that Close cannot close the channel between the closed check and
// the subsequent send in Enqueue.
func (q *DefaultFileMutationQueue) workerForLocked(realPath string) (chan queuedMutation, error) {
	if q.closed {
		return nil, fmt.Errorf("mutation: queue is closed")
	}

	if v, ok := q.workers.Load(realPath); ok {
		//nolint:errcheck // map value is always a worker channel.
		return v.(chan queuedMutation), nil
	}

	input := make(chan queuedMutation)
	actual, loaded := q.workers.LoadOrStore(realPath, input)
	if loaded {
		//nolint:errcheck // map value is always a worker channel.
		return actual.(chan queuedMutation), nil
	}
	q.startWorker(input)
	return input, nil
}

// startWorker launches the per-file worker goroutine that consumes mutations
// FIFO and reports each result on its own result channel. Each mutation is
// processed inside a defer/recover so a panic in the handler does not kill the
// worker goroutine; instead the panic is reported as an error result and the
// loop continues.
func (q *DefaultFileMutationQueue) startWorker(input chan queuedMutation) {
	go func() {
		for qm := range input {
			tr, err := q.applySafe(qm.mutation)
			res := FileMutationResult{
				Success:    err == nil,
				Error:      err,
				ToolResult: tr,
			}
			qm.result <- res
			close(qm.result)
		}
	}()
}

// applySafe runs the configured handler inside a recover block so a panic
// is converted into an error result instead of killing the worker goroutine.
// On panic the returned ToolResult is nil.
func (q *DefaultFileMutationQueue) applySafe(m FileMutation) (tr *ToolResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("mutation: handler panicked", "path", m.FilePath, "operation", m.Operation, "panic", r)
			tr = nil
			err = fmt.Errorf("mutation: handler panic for %s: %v", m.FilePath, r)
		}
	}()
	tr, err = q.apply(context.Background(), m)
	return tr, err
}

// apply runs the configured handler, falling back to the built-in write/edit
// dispatch when no custom handler is configured.
func (q *DefaultFileMutationQueue) apply(ctx context.Context, m FileMutation) (*ToolResult, error) {
	h := q.handler
	if h == nil {
		h = defaultMutationHandler
	}
	return h(ctx, m)
}

// newConfiguredMutationHandler returns a MutationHandler that creates WriteTool
// and EditFileTool instances with the given fileTracker and diffGenerator
// injected, so that backup checkpoints and change previews are preserved when
// mutations are applied through the queue. When both ft and dg are nil the
// behavior is equivalent to the package-level defaultMutationHandler.
func newConfiguredMutationHandler(ft *FileTracker, dg DiffGenerator) MutationHandler {
	return func(ctx context.Context, m FileMutation) (*ToolResult, error) {
		switch m.Operation {
		case "write":
			toolOpts := []WriteToolOption{WithOverwrite(true)}
			if ft != nil {
				toolOpts = append(toolOpts, WithFileTracker(ft))
			}
			if dg != nil {
				toolOpts = append(toolOpts, WithDiffGenerator(dg))
			}
			tool := NewWriteTool(toolOpts...)
			call := ToolCall{Args: map[string]any{"path": m.FilePath}}
			if s, ok := m.Content.(string); ok {
				call.Args["content"] = s
			}
			return tool.Execute(ctx, call)
		case "edit":
			call := ToolCall{Args: map[string]any{"file_path": m.FilePath}}
			if cm, ok := m.Content.(map[string]any); ok {
				if v, ok := cm["old_string"]; ok {
					call.Args["old_string"] = v
				}
				if v, ok := cm["new_string"]; ok {
					call.Args["new_string"] = v
				}
			}
			toolOpts := []EditFileToolOption{}
			if ft != nil {
				toolOpts = append(toolOpts, WithEditFileTracker(ft))
			}
			if dg != nil {
				toolOpts = append(toolOpts, WithEditDiffGenerator(dg))
			}
			tool := NewEditFileTool(toolOpts...)
			return tool.Execute(ctx, call)
		default:
			return nil, fmt.Errorf("mutation: unsupported operation %q", m.Operation)
		}
	}
}

// defaultMutationHandler applies a mutation using the built-in write/edit tools.
func defaultMutationHandler(ctx context.Context, m FileMutation) (*ToolResult, error) {
	switch m.Operation {
	case "write":
		tool := NewWriteTool(WithOverwrite(true))
		call := ToolCall{Args: map[string]any{"path": m.FilePath}}
		if s, ok := m.Content.(string); ok {
			call.Args["content"] = s
		}
		return tool.Execute(ctx, call)
	case "edit":
		call := ToolCall{Args: map[string]any{"file_path": m.FilePath}}
		if cm, ok := m.Content.(map[string]any); ok {
			if v, ok := cm["old_string"]; ok {
				call.Args["old_string"] = v
			}
			if v, ok := cm["new_string"]; ok {
				call.Args["new_string"] = v
			}
		}
		tool := NewEditFileTool()
		return tool.Execute(ctx, call)
	default:
		return nil, fmt.Errorf("mutation: unsupported operation %q", m.Operation)
	}
}

// Close shuts down every per-file worker, waits for in-flight mutations to
// finish, and releases all goroutines. It returns an error if the queue is
// already closed. Calling methods on a closed queue returns an error.
func (q *DefaultFileMutationQueue) Close() error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return fmt.Errorf("mutation: queue is already closed")
	}
	q.closed = true
	channels := make([]chan queuedMutation, 0)
	q.workers.Range(func(_, v any) bool {
		//nolint:errcheck // map value is always a worker channel.
		channels = append(channels, v.(chan queuedMutation))
		return true
	})
	q.mu.Unlock()

	for _, ch := range channels {
		close(ch)
	}
	return nil
}
