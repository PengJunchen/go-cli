package memory

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

const maxExtractionInputBytes = 16 << 10

// DefaultExtractionTimeout bounds one automatic extraction including publication.
const DefaultExtractionTimeout = 30 * time.Second

// ExtractionJob contains immutable, positively completed user/assistant text.
// Eligible rechecks current policy after publication-lock waits.
type ExtractionJob struct {
	Source   Provenance
	Messages []llm.Message
	Eligible func() bool
	epoch    uint64
}

// WorkerOptions controls bounded auxiliary work. Pending jobs use latest-job
// semantics: replacing one pending job skips that source rather than growing
// an unbounded queue or resending full conversation history.
type WorkerOptions struct {
	Timeout  time.Duration
	Eligible func() bool
	Logger   *slog.Logger
}

// Worker owns at most one extraction and one bounded pending snapshot.
type Worker struct {
	ctx       context.Context
	cancel    context.CancelFunc
	store     *FileMemoryStore
	extractor MemoryExtractor
	options   WorkerOptions
	mu        sync.Mutex
	commitMu  sync.Mutex
	pending   *ExtractionJob
	active    bool
	stopped   bool
	changed   chan struct{}
	wake      chan struct{}
	done      chan struct{}
	jobCancel context.CancelFunc
	enabled   atomic.Bool
	epoch     atomic.Uint64
}

// NewWorker starts an Assembly-owned worker; Stop then Done proves teardown.
func NewWorker(ctx context.Context, store *FileMemoryStore, extractor MemoryExtractor, options WorkerOptions) *Worker {
	ctx, cancel := context.WithCancel(ctx)
	if options.Timeout <= 0 {
		options.Timeout = DefaultExtractionTimeout
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	w := &Worker{ctx: ctx, cancel: cancel, store: store, extractor: extractor, options: options, changed: make(chan struct{}), wake: make(chan struct{}, 1), done: make(chan struct{})}
	w.enabled.Store(true)
	go w.run()
	return w
}

// Submit replaces only the pending snapshot; caller memory is never retained.
func (w *Worker) Submit(job ExtractionJob) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || !w.enabled.Load() || w.ctx.Err() != nil || !w.eligible(job) {
		return false
	}
	if len(job.Source.SourceIDs) == 0 || len(job.Source.SourceIDs) > 256 || len(job.Source.WorkspaceID) > 256 || len(job.Source.SessionID) > 256 || len(job.Source.BranchID) > 256 {
		return false
	}
	sourceBytes := 0
	for _, id := range job.Source.SourceIDs {
		sourceBytes += len(id)
		if sourceBytes > 16<<10 {
			return false
		}
	}
	job.Source.SourceIDs = append([]string(nil), job.Source.SourceIDs...)
	job.Messages = boundedExtractionMessages(job.Messages)
	if len(job.Messages) < 2 {
		return false
	}
	job.epoch = w.epoch.Load()
	if w.pending != nil {
		w.options.Logger.Debug("memory.pending_replaced", "source", w.pending.Source.Key())
	}
	w.pending = &job
	w.signalLocked()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return true
}

// SetEnabled revokes queued/in-flight work before returning when disabled.
// A later enable admits new jobs; revoked jobs never regain authority.
func (w *Worker) SetEnabled(enabled bool) {
	w.enabled.Store(enabled)
	w.epoch.Add(1)
	w.mu.Lock()
	if !enabled {
		w.pending = nil
		if w.jobCancel != nil {
			w.jobCancel()
		}
	}
	w.signalLocked()
	w.mu.Unlock()
	// Cancel first so a publication waiting for another process's lock can
	// unwind before this caller waits for the publication linearization point.
	w.commitMu.Lock()
	w.commitMu.Unlock() //nolint:staticcheck // SA2001: publication barrier waits for an in-flight commit to exit after cancellation.
}

// Stop seals admission and cancels work. Already synced writes remain facts;
// no new publication begins after Stop returns.
func (w *Worker) Stop() {
	w.cancel()
	w.SetEnabled(false)
	w.mu.Lock()
	w.stopped = true
	w.cancel()
	w.signalLocked()
	w.mu.Unlock()
}

// Done closes only when the extractor and owned worker have actually exited.
func (w *Worker) Done() <-chan struct{} { return w.done }

// Drain waits for current eligible work without canceling it.
func (w *Worker) Drain(ctx context.Context) error {
	for {
		w.mu.Lock()
		idle := !w.active && w.pending == nil
		changed := w.changed
		w.mu.Unlock()
		if idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (w *Worker) signalLocked() { close(w.changed); w.changed = make(chan struct{}) }

func (w *Worker) eligible(job ExtractionJob) bool {
	return (job.Eligible == nil || job.Eligible()) && (w.options.Eligible == nil || w.options.Eligible())
}

func (w *Worker) run() {
	defer close(w.done)
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.wake:
		}
		for {
			w.mu.Lock()
			job := w.pending
			w.pending = nil
			if job == nil || w.stopped {
				w.signalLocked()
				w.mu.Unlock()
				break
			}
			ctx, cancel := context.WithTimeout(w.ctx, w.options.Timeout)
			w.jobCancel = cancel
			w.active = true
			w.signalLocked()
			w.mu.Unlock()
			w.process(ctx, *job)
			cancel()
			w.mu.Lock()
			w.active = false
			w.jobCancel = nil
			w.signalLocked()
			w.mu.Unlock()
		}
	}
}

func (w *Worker) process(ctx context.Context, job ExtractionJob) {
	span, ctx := tracing.SpanFromContext(ctx, "memory.extract", tracing.SpanKindInternal)
	defer span.End()
	if !w.enabled.Load() || job.epoch != w.epoch.Load() || !w.eligible(job) {
		return
	}
	processed, err := w.store.Processed(ctx, job.Source)
	if err != nil {
		w.options.Logger.Warn("memory.processed_check_failed", "workspace", job.Source.WorkspaceID, "source", job.Source.Key())
		return
	}
	if processed {
		return
	}
	facts, err := w.extractor.Extract(ctx, job.Messages)
	if err != nil {
		w.options.Logger.Warn("memory.extraction_failed", "workspace", job.Source.WorkspaceID, "source", job.Source.Key())
		return
	}
	w.commitMu.Lock()
	defer w.commitMu.Unlock()
	eligible := func() bool {
		return w.enabled.Load() && job.epoch == w.epoch.Load() && ctx.Err() == nil && w.eligible(job)
	}
	if !eligible() {
		return
	}
	if _, err := w.store.PublishExtraction(ctx, job.Source, facts, eligible); err != nil {
		w.options.Logger.Warn("memory.publication_failed", "workspace", job.Source.WorkspaceID, "source", job.Source.Key())
	}
}

func boundedExtractionMessages(messages []llm.Message) []llm.Message {
	remaining := maxExtractionInputBytes
	var result []llm.Message
	eligible := 0
	for _, message := range messages {
		if message.Role == llm.RoleUser || message.Role == llm.RoleAssistant {
			eligible++
			if eligible == 32 {
				break
			}
		}
	}
	for _, message := range messages {
		if message.Role != llm.RoleUser && message.Role != llm.RoleAssistant {
			continue
		}
		content := message.Content
		if eligible == 0 {
			break
		}
		budget := remaining / eligible
		eligible--
		if len(content) > budget {
			content = content[:budget]
			for !utf8.ValidString(content) {
				content = content[:len(content)-1]
			}
		}
		remaining -= len(content)
		if content != "" {
			result = append(result, llm.Message{Role: message.Role, Content: content})
		}
		if remaining == 0 {
			break
		}
		if eligible == 0 {
			break
		}
	}
	return result
}
