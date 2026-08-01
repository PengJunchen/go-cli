package tracing

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// defaultFlushInterval is how often AsyncExporter flushes buffered spans when
// throughput is low.
const defaultFlushInterval = 100 * time.Millisecond

// AsyncExporter wraps a TraceExporter to perform exports asynchronously on a
// background goroutine. Spans are placed on a bounded channel; when the queue
// is full, spans are dropped (non-blocking) so tracing never stalls the main
// flow. The worker batches spans and flushes them to the inner exporter on a
// timer or when the batch fills. Shutdown waits for the worker to drain and
// exit, so no goroutines leak.
type AsyncExporter struct {
	inner         TraceExporter
	spanCh        chan TraceSpan
	done          chan struct{}
	batchSize     int
	flushInterval time.Duration
	wg            sync.WaitGroup
}

var _ TraceExporter = (*AsyncExporter)(nil)

// NewAsyncExporter creates an AsyncExporter with the given bounded queue and
// batch size.
func NewAsyncExporter(inner TraceExporter, queueSize, batchSize int) *AsyncExporter {
	return newAsyncExporter(inner, queueSize, batchSize, defaultFlushInterval)
}

// newAsyncExporter is the internal constructor that also takes a custom flush
// interval so tests can run with a shorter timer.
func newAsyncExporter(inner TraceExporter, queueSize, batchSize int, flushInterval time.Duration) *AsyncExporter {
	if batchSize < 1 {
		batchSize = 1
	}
	e := &AsyncExporter{
		inner:         inner,
		spanCh:        make(chan TraceSpan, queueSize),
		done:          make(chan struct{}),
		batchSize:     batchSize,
		flushInterval: flushInterval,
	}
	e.wg.Add(1)
	go e.process()
	return e
}

// ExportSpan enqueues the span for async export. It never blocks: when the
// queue is full the span is dropped.
func (e *AsyncExporter) ExportSpan(_ context.Context, span TraceSpan) error {
	select {
	case e.spanCh <- span:
		return nil
	default:
		slog.Warn("trace span dropped: async queue full", "queue_size", cap(e.spanCh))
		return nil
	}
}

// process runs on a background goroutine. It batches spans and flushes them to
// the inner exporter on a timer or on batch size. On Shutdown it drains any
// remaining queued spans before exiting.
func (e *AsyncExporter) process() {
	defer e.wg.Done()

	batch := make([]TraceSpan, 0, e.batchSize)
	flush := func() {
		for _, sp := range batch {
			if err := e.inner.ExportSpan(context.Background(), sp); err != nil {
				slog.Warn("failed to flush span", "span_id", sp.SpanID(), "err", err)
			}
		}
		batch = batch[:0]
	}

	ticker := time.NewTicker(e.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case span := <-e.spanCh:
			batch = append(batch, span)
			if len(batch) >= e.batchSize {
				flush()
			}
		case <-ticker.C:
			if len(batch) > 0 {
				flush()
			}
		case <-e.done:
			// Drain any spans still queued, then flush the remainder.
			for {
				select {
				case span := <-e.spanCh:
					batch = append(batch, span)
				default:
					flush()
					return
				}
			}
		}
	}
}

// Shutdown stops the worker (waiting for it to exit), flushing all buffered
// spans, then shuts down the inner exporter.
func (e *AsyncExporter) Shutdown(ctx context.Context) error {
	close(e.done)
	e.wg.Wait()
	return e.inner.Shutdown(ctx)
}
