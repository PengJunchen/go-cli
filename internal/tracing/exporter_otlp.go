package tracing

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// OTLP payload envelope. This is a JSON-over-HTTP interpretation of the OTLP
// HTTP /v1/traces contract. The real contract uses length-delimited protobuf
// SerializeTraceServiceRequest bodies; here each batch is carried as a JSON
// object with the span list under the "spans" key so the exporter stays free
// of the opentelemetry-proto and protobuf dependencies. A collector built on
// the true OTLP HTTP contract would need a gateway/converter; this framing is
// documented as a deliberate simplification.
type otlpPayload struct {
	Spans []SpanData `json:"spans"`
}

// OTLPTraceExporter exports spans to an OpenTelemetry-compatible collector via
// OTLP-over-HTTP. It buffers spans and exports them in batches, either when a
// batch reaches BatchSize or on a FlushInterval timer. A background goroutine
// performs the HTTP flushes so ExportSpan stays non-blocking. It does not emit
// its own spans; failures are logged with slog.ErrorContext and export latency
// with slog.DebugContext.
type OTLPTraceExporter struct {
	cfg          OTLPTraceExporterConfig
	client       *http.Client
	mu           sync.Mutex
	buffer       []SpanData
	batchSize    int
	trigger      chan struct{}
	done         chan struct{}
	wg           sync.WaitGroup
	shutdownOnce sync.Once
}

// Compile-time assertion that OTLPTraceExporter satisfies TraceExporter.
var _ TraceExporter = (*OTLPTraceExporter)(nil)

// NewOTLPTraceExporter creates an OTLPTraceExporter with the given config.
// BatchSize and FlushInterval drive batching; the HTTP client uses Timeout and
// Insecure to bound and secure each request.
func NewOTLPTraceExporter(cfg OTLPTraceExporterConfig) *OTLPTraceExporter {
	batchSize := normalizeBatchSize(cfg.BatchSize)
	interval := normalizeInterval(cfg.FlushInterval)
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = exporterDefaultHTTPTimeout
	}

	transport := http.DefaultTransport.(*http.Transport).Clone() //nolint:errcheck // DefaultTransport is *http.Transport by default.
	if cfg.Insecure {
		// Insecure is an explicit opt-in for self-signed collectors.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}

	e := &OTLPTraceExporter{
		cfg:       cfg,
		client:    &http.Client{Timeout: timeout, Transport: transport},
		buffer:    make([]SpanData, 0, batchSize),
		batchSize: batchSize,
		trigger:   make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	e.wg.Add(1)
	go e.flushLoop(interval)
	return e
}

// ExportSpan buffers the span and, when the batch fills, signals the flush
// goroutine. It never blocks on network I/O.
func (e *OTLPTraceExporter) ExportSpan(_ context.Context, span TraceSpan) error {
	data := SpanToData(span)

	e.mu.Lock()
	e.buffer = append(e.buffer, data)
	shouldFlush := len(e.buffer) >= e.batchSize
	e.mu.Unlock()

	if shouldFlush {
		// Non-blocking signal; the worker may already be flushing.
		select {
		case e.trigger <- struct{}{}:
		default:
		}
	}
	return nil
}

// Shutdown closes the exporter: it signals the flush goroutine to stop, waits
// for it to exit, then flushes any remaining buffered spans.
func (e *OTLPTraceExporter) Shutdown(ctx context.Context) error {
	e.shutdownOnce.Do(func() {
		close(e.done)
	})
	e.wg.Wait()
	return e.flushBatch(ctx)
}

// flushLoop flushes buffered spans on a FlushInterval timer or when a batch
// fills (trigger). The ticker is always stopped on exit so no goroutines leak.
func (e *OTLPTraceExporter) flushLoop(interval time.Duration) {
	defer e.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.trigger:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = e.flushBatch(ctx) //nolint:errcheck // best-effort; errors logged inside
			cancel()
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = e.flushBatch(ctx) //nolint:errcheck // best-effort; errors logged inside
			cancel()
		case <-e.done:
			return
		}
	}
}

// flushBatch snapshots the current buffer and exports it, returning the first
// export error encountered.
func (e *OTLPTraceExporter) flushBatch(ctx context.Context) error {
	e.mu.Lock()
	if len(e.buffer) == 0 {
		e.mu.Unlock()
		return nil
	}
	batch := make([]SpanData, len(e.buffer))
	copy(batch, e.buffer)
	e.buffer = e.buffer[:0]
	e.mu.Unlock()

	return e.exportBatch(ctx, batch)
}

// exportBatch POSTs the batch to the collector endpoint. On failure it logs
// via slog.ErrorContext and returns the error; latency is logged via
// slog.DebugContext. The request context is derived from ctx so cancellation
// and deadlines propagate to the HTTP call.
func (e *OTLPTraceExporter) exportBatch(ctx context.Context, batch []SpanData) error {
	start := time.Now()

	payload, err := json.Marshal(otlpPayload{Spans: batch})
	if err != nil {
		slog.ErrorContext(ctx, "otlp export failed", "op", "otlp.export", "spans", len(batch), "err", err)
		return fmt.Errorf("marshal otlp payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		slog.ErrorContext(ctx, "otlp export failed", "op", "otlp.export", "err", err)
		return fmt.Errorf("create otlp request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "otlp export failed", "op", "otlp.export", "spans", len(batch), "err", err)
		return fmt.Errorf("send otlp request: %w", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain response body.
	_ = resp.Body.Close()                 //nolint:errcheck // best-effort close.

	slog.DebugContext(ctx, "otlp export latency",
		"op", "otlp.export", "spans", len(batch), "duration_ms", time.Since(start).Milliseconds())

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("otlp collector returned status %d", resp.StatusCode)
		slog.ErrorContext(ctx, "otlp export failed", "op", "otlp.export", "status", resp.StatusCode, "err", err)
		return err
	}
	return nil
}
