//exempt:scan012
package tracing

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	defaultFlushInterval                = 100 * time.Millisecond
	defaultAsyncExportCallbackTimeout   = 5 * time.Second
	defaultAsyncDrainTimeout            = 10 * time.Second
	defaultAsyncShutdownCallbackTimeout = 10 * time.Second

	// DefaultAsyncExporterShutdownWaitBudget bounds the CLI's wait for both
	// the shared queue drain and the one inner shutdown callback.
	DefaultAsyncExporterShutdownWaitBudget = defaultAsyncDrainTimeout + defaultAsyncShutdownCallbackTimeout
)

// ErrExporterClosed indicates that ExportSpan was called after shutdown closed admission.
var ErrExporterClosed = errors.New("trace exporter is closed")

// ErrExporterQueueFull indicates that the bounded async queue rejected a span.
var ErrExporterQueueFull = errors.New("trace exporter queue is full")

// ErrShutdownPending indicates that this caller stopped waiting before the shared owner finished.
var ErrShutdownPending = errors.New("trace exporter shutdown is pending")

// AsyncExporterStats is the final or waiter-timeout snapshot for one async
// exporter owner. Accepted spans partition into delivered, sink failures,
// shutdown abandonment, or pending work.
type AsyncExporterStats struct {
	Accepted            int64
	Delivered           int64
	QueueFullDropped    int64
	SinkFailed          int64
	RejectedAfterClose  int64
	AbandonedOnShutdown int64
	Pending             int64
}

// AsyncExporterReport describes the exporter owner's state at the time a
// ShutdownWithReport caller returns. Delivered means only that ExportSpan
// returned nil; it does not claim durable storage.
type AsyncExporterReport struct {
	Stats          AsyncExporterStats
	OwnerFinished  bool
	CleanupPending bool
}

// ShutdownPendingError means this caller stopped waiting before the shared
// close owner finished. It does not cancel or replace that owner.
type ShutdownPendingError struct {
	cause error
}

func (e *ShutdownPendingError) Error() string { return ErrShutdownPending.Error() }
func (e *ShutdownPendingError) Unwrap() error { return e.cause }
func (e *ShutdownPendingError) Is(target error) bool {
	return target == ErrShutdownPending
}

// AsyncExporter wraps a TraceExporter with a bounded FIFO and one worker.
// Its mutex linearizes admission with shutdown and keeps queued work visible
// to the close owner until the shared drain deadline.
type AsyncExporter struct {
	inner         TraceExporter
	queue         []TraceSpan
	queueSize     int
	batchSize     int
	flushInterval time.Duration
	drainTimeout  time.Duration
	exportTimeout time.Duration
	closeTimeout  time.Duration

	mu            sync.Mutex
	wake          chan struct{}
	workerDone    chan struct{}
	closeDone     chan struct{}
	closing       bool
	drainDeadline time.Time
	inFlight      bool
	ownerFinished bool
	stats         AsyncExporterStats
	firstSinkErr  error // retain one actual callback error; counters cover all failures with bounded memory.
	closeErr      error
}

var _ TraceExporter = (*AsyncExporter)(nil)

// NewAsyncExporter creates an AsyncExporter with the given bounded queue and
// batch size. Non-positive queue and batch sizes are clamped to one.
func NewAsyncExporter(inner TraceExporter, queueSize, batchSize int) *AsyncExporter {
	return newAsyncExporter(inner, queueSize, batchSize, defaultFlushInterval)
}

// newAsyncExporter is the internal constructor that also takes a custom flush
// interval so tests can run with a shorter timer.
func newAsyncExporter(inner TraceExporter, queueSize, batchSize int, flushInterval time.Duration) *AsyncExporter {
	return newAsyncExporterWithTimeouts(
		inner,
		queueSize,
		batchSize,
		flushInterval,
		defaultAsyncDrainTimeout,
		defaultAsyncExportCallbackTimeout,
		defaultAsyncShutdownCallbackTimeout,
	)
}

// newAsyncExporterWithTimeouts keeps lifecycle budgets injectable only for
// package tests; production construction always uses the fixed defaults.
func newAsyncExporterWithTimeouts(
	inner TraceExporter,
	queueSize, batchSize int,
	flushInterval, drainTimeout, exportTimeout, closeTimeout time.Duration,
) *AsyncExporter {
	if queueSize < 1 {
		queueSize = 1
	}
	if batchSize < 1 {
		batchSize = 1
	}
	if flushInterval <= 0 {
		flushInterval = defaultFlushInterval
	}
	if drainTimeout <= 0 {
		drainTimeout = defaultAsyncDrainTimeout
	}
	if exportTimeout <= 0 {
		exportTimeout = defaultAsyncExportCallbackTimeout
	}
	if closeTimeout <= 0 {
		closeTimeout = defaultAsyncShutdownCallbackTimeout
	}
	e := &AsyncExporter{
		inner:         inner,
		queue:         make([]TraceSpan, 0, queueSize),
		queueSize:     queueSize,
		batchSize:     batchSize,
		flushInterval: flushInterval,
		drainTimeout:  drainTimeout,
		exportTimeout: exportTimeout,
		closeTimeout:  closeTimeout,
		wake:          make(chan struct{}, 1),
		workerDone:    make(chan struct{}),
		closeDone:     make(chan struct{}),
	}
	go e.process()
	return e
}

// ExportSpan enqueues a span without blocking. Queue-full and closed rejection
// are returned to the caller instead of being reported as successful enqueue.
func (e *AsyncExporter) ExportSpan(_ context.Context, span TraceSpan) error {
	e.mu.Lock()
	if e.closing {
		e.stats.RejectedAfterClose++
		e.mu.Unlock()
		return ErrExporterClosed
	}
	if len(e.queue) >= e.queueSize {
		e.stats.QueueFullDropped++
		e.mu.Unlock()
		return ErrExporterQueueFull
	}
	e.queue = append(e.queue, span)
	e.stats.Accepted++
	e.mu.Unlock()
	e.signalWorker()
	return nil
}

func (e *AsyncExporter) signalWorker() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// process exports FIFO spans. Once a batch is ready, it continues until that
// pending batch is empty. During close, queued spans are immediately eligible
// for the one shared drain window.
func (e *AsyncExporter) process() {
	defer close(e.workerDone)
	ticker := time.NewTicker(e.flushInterval)
	defer ticker.Stop()

	batchReady := false
	for {
		e.mu.Lock()
		if e.closing && !time.Now().Before(e.drainDeadline) {
			e.abandonQueuedLocked()
		}
		if len(e.queue) == 0 {
			batchReady = false
		} else if len(e.queue) >= e.batchSize {
			batchReady = true
		}
		if len(e.queue) > 0 && (e.closing || batchReady) {
			span := e.queue[0]
			e.queue[0] = nil
			e.queue = e.queue[1:]
			deadline := time.Now().Add(e.exportTimeout)
			if e.closing && e.drainDeadline.Before(deadline) {
				deadline = e.drainDeadline
			}
			if e.closing && !time.Now().Before(deadline) {
				e.stats.AbandonedOnShutdown++
				e.mu.Unlock()
				continue
			}
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			e.inFlight = true
			e.mu.Unlock()

			err := e.inner.ExportSpan(ctx, span)
			cancel()

			e.mu.Lock()
			e.inFlight = false
			if err != nil {
				e.stats.SinkFailed++
				if e.firstSinkErr == nil {
					e.firstSinkErr = err
				}
			} else {
				e.stats.Delivered++
			}
			e.mu.Unlock()
			continue
		}
		if e.closing && len(e.queue) == 0 && !e.inFlight {
			e.mu.Unlock()
			return
		}
		e.mu.Unlock()

		select {
		case <-e.wake:
		case <-ticker.C:
			batchReady = true
		}
	}
}

func (e *AsyncExporter) abandonQueuedLocked() {
	if len(e.queue) == 0 {
		return
	}
	e.stats.AbandonedOnShutdown += int64(len(e.queue))
	for i := range e.queue {
		e.queue[i] = nil
	}
	e.queue = e.queue[:0]
}

func (e *AsyncExporter) startCloseLocked() (time.Time, bool) {
	if e.closing {
		return e.drainDeadline, false
	}
	e.closing = true
	e.drainDeadline = time.Now().Add(e.drainTimeout)
	return e.drainDeadline, true
}

func (e *AsyncExporter) closeOwner(deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-e.workerDone:
	case <-timer.C:
		e.mu.Lock()
		e.abandonQueuedLocked()
		e.mu.Unlock()
		e.signalWorker()
		<-e.workerDone
	}

	//nolint:gosec // the single close owner must not inherit a waiter's canceled context.
	ctx, cancel := context.WithTimeout(context.Background(), e.closeTimeout)
	innerErr := e.inner.Shutdown(ctx)
	cancel()

	e.mu.Lock()
	var errList []error
	if e.firstSinkErr != nil {
		errList = append(errList, e.firstSinkErr)
	}
	if innerErr != nil {
		errList = append(errList, innerErr)
	}
	e.closeErr = errors.Join(errList...)
	e.ownerFinished = true
	close(e.closeDone)
	e.mu.Unlock()
}

// Shutdown stops admission and waits for the single close owner. The caller's
// context bounds only this wait; it never cancels the worker or close owner.
func (e *AsyncExporter) Shutdown(ctx context.Context) error {
	_, err := e.ShutdownWithReport(ctx)
	return err
}

// ShutdownWithReport returns one snapshot from the shared close owner. If the
// caller's context expires first, the returned pending error does not stop the
// owner, which retains its worker and sink until callbacks actually return.
func (e *AsyncExporter) ShutdownWithReport(ctx context.Context) (AsyncExporterReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	deadline, startOwner := e.startCloseLocked()
	e.mu.Unlock()
	e.signalWorker()
	if startOwner {
		go e.closeOwner(deadline) //nolint:gosec // G118: the shared close owner must outlive the caller's cancellable context.
	}

	select {
	case <-e.closeDone:
		return e.snapshotReport(), e.closeError()
	default:
	}
	select {
	case <-e.closeDone:
		return e.snapshotReport(), e.closeError()
	case <-ctx.Done():
		select {
		case <-e.closeDone:
			return e.snapshotReport(), e.closeError()
		default:
		}
		return e.snapshotReport(), &ShutdownPendingError{cause: ctx.Err()}
	}
}

func (e *AsyncExporter) snapshotReport() AsyncExporterReport {
	e.mu.Lock()
	defer e.mu.Unlock()
	stats := e.stats
	stats.Pending = stats.Accepted - stats.Delivered - stats.SinkFailed - stats.AbandonedOnShutdown
	if stats.Pending < 0 {
		stats.Pending = 0
	}
	return AsyncExporterReport{
		Stats:          stats,
		OwnerFinished:  e.ownerFinished,
		CleanupPending: !e.ownerFinished,
	}
}

func (e *AsyncExporter) closeError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closeErr
}
