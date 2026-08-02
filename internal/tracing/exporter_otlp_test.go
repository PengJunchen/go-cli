package tracing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// MockOTLPCollector simulates an OpenTelemetry collector HTTP receiver. It
// records the raw bodies of every export request so tests can decode the
// JSON-over-HTTP payload and assert correctness. It also records the request
// headers so header passthrough (AC for cfg.Headers) can be verified.
type MockOTLPCollector struct {
	server  *httptest.Server
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	status  atomic.Int32
}

// NewMockOTLPCollector starts a mock collector and returns it.
func NewMockOTLPCollector() *MockOTLPCollector {
	m := &MockOTLPCollector{status: atomic.Int32{}}
	m.status.Store(http.StatusOK)
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body) //nolint:errcheck // request body read is best-effort
		m.mu.Lock()
		m.bodies = append(m.bodies, body)
		m.headers = append(m.headers, r.Header.Clone())
		m.mu.Unlock()
		w.WriteHeader(int(m.status.Load()))
	}))
	return m
}

// URL returns the collector base URL.
func (m *MockOTLPCollector) URL() string { return m.server.URL }

// Endpoint returns the /v1/traces export endpoint.
func (m *MockOTLPCollector) Endpoint() string { return m.server.URL + "/v1/traces" }

// SetStatus programs the HTTP status returned for the next requests.
func (m *MockOTLPCollector) SetStatus(code int) { m.status.Store(int32(code)) }

// Bodies returns a copy of the raw request bodies.
func (m *MockOTLPCollector) Bodies() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]byte(nil), m.bodies...)
}

// Requests returns the number of export requests received.
func (m *MockOTLPCollector) Requests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bodies)
}

// LastHeader returns the header of the most recent request.
func (m *MockOTLPCollector) LastHeader() http.Header {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.headers) == 0 {
		return nil
	}
	return m.headers[len(m.headers)-1].Clone()
}

// Close shuts down the mock server.
func (m *MockOTLPCollector) Close() { m.server.Close() }

// decodePayload decodes a single collector body into the OTLP envelope.
func decodePayload(t *testing.T, body []byte) otlpPayload {
	t.Helper()
	var p otlpPayload
	require.NoError(t, json.Unmarshal(body, &p))
	return p
}

func newTestSpan(tr *Tracer, name string) TraceSpan {
	span, _ := tr.Start(context.Background(), name, SpanKindInternal)
	span.SetStatus(SpanStatusOK, "")
	return span
}

func TestOTLPTraceExporterConfigFields(t *testing.T) {
	cfg := OTLPTraceExporterConfig{
		Endpoint:      "http://collector.invalid/v1/traces",
		Headers:       map[string]string{"X-Api-Key": "secret-key-here"},
		Timeout:       time.Second,
		Insecure:      true,
		BatchSize:     16,
		FlushInterval: time.Millisecond,
	}
	exp := NewOTLPTraceExporter(cfg)
	require.NotNil(t, exp)
	require.NoError(t, exp.Shutdown(context.Background()))

	assert.Equal(t, "http://collector.invalid/v1/traces", cfg.Endpoint)
	assert.Equal(t, 16, cfg.BatchSize)
	assert.Equal(t, time.Millisecond, cfg.FlushInterval)
	assert.True(t, cfg.Insecure)
	require.Len(t, cfg.Headers, 1)
	require.Equal(t, "secret-key-here", cfg.Headers["X-Api-Key"])
}

func TestOTLPTraceExporterSendsSpanToCollector(t *testing.T) {
	c := NewMockOTLPCollector()
	defer verify.AssertNoGoroutineLeak(t)()
	defer c.Close()

	exp := NewOTLPTraceExporter(OTLPTraceExporterConfig{
		Endpoint: c.Endpoint(), BatchSize: 1, FlushInterval: time.Hour,
	})
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-otlp", exp)
	span := newTestSpan(tr, "op.otlp")

	require.NoError(t, exp.ExportSpan(context.Background(), span))

	require.Eventually(t, func() bool { return c.Requests() >= 1 }, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, 1, c.Requests(), "batch size 1 must export per span")

	body := c.Bodies()[0]
	payload := decodePayload(t, body)
	require.Len(t, payload.Spans, 1)
	got := payload.Spans[0]
	assert.Equal(t, "trace-otlp", got.TraceID)
	assert.Equal(t, span.SpanID(), got.SpanID)
	assert.Equal(t, "op.otlp", got.Name)
	assert.Equal(t, SpanKindInternal, got.SpanKind)
	assert.Equal(t, SpanStatusOK, got.Status)
	assert.Equal(t, "application/json", c.LastHeader().Get("Content-Type"))
}

func TestOTLPTraceExporterBatchExportByBatchSize(t *testing.T) {
	c := NewMockOTLPCollector()
	defer c.Close()

	exp := NewOTLPTraceExporter(OTLPTraceExporterConfig{
		Endpoint: c.Endpoint(), BatchSize: 3, FlushInterval: time.Hour,
	})
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-otlp-batch", exp)
	for i := 0; i < 3; i++ {
		span := newTestSpan(tr, "op")
		require.NoError(t, exp.ExportSpan(context.Background(), span))
	}

	require.Eventually(t, func() bool { return c.Requests() >= 1 }, 2*time.Second, 5*time.Millisecond)
	require.Equal(t, 1, c.Requests(), "three spans with batch size 3 must export in one batch")
	payload := decodePayload(t, c.Bodies()[0])
	require.Len(t, payload.Spans, 3)
}

func TestOTLPTraceExporterBatchExportByFlushInterval(t *testing.T) {
	c := NewMockOTLPCollector()
	defer c.Close()

	exp := NewOTLPTraceExporter(OTLPTraceExporterConfig{
		Endpoint: c.Endpoint(), BatchSize: 100, FlushInterval: 40 * time.Millisecond,
	})
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-otlp-interval", exp)
	span := newTestSpan(tr, "op")
	require.NoError(t, exp.ExportSpan(context.Background(), span))

	require.Eventually(t, func() bool { return c.Requests() >= 1 }, 2*time.Second, 5*time.Millisecond)
	payload := decodePayload(t, c.Bodies()[0])
	require.Len(t, payload.Spans, 1)
	assert.Equal(t, span.SpanID(), payload.Spans[0].SpanID)
}

func TestOTLPTraceExporterShutdownFlushesRemainingBuffer(t *testing.T) {
	c := NewMockOTLPCollector()
	defer c.Close()

	// Batch size larger than the delivered spans, so final delivery only
	// happens via the Shutdown drain.
	exp := NewOTLPTraceExporter(OTLPTraceExporterConfig{
		Endpoint: c.Endpoint(), BatchSize: 10, FlushInterval: time.Hour,
	})

	tr := NewTracer("trace-otlp-shutdown", exp)
	for i := 0; i < 4; i++ {
		span := newTestSpan(tr, "op")
		require.NoError(t, exp.ExportSpan(context.Background(), span))
	}

	require.NoError(t, exp.Shutdown(context.Background()))
	require.Equal(t, 1, c.Requests(), "shutdown must flush the buffered batch")
	payload := decodePayload(t, c.Bodies()[0])
	require.Len(t, payload.Spans, 4, "shutdown must flush all remaining buffered spans")
}

func TestOTLPTraceExporterHonorsCustomHeaders(t *testing.T) {
	c := NewMockOTLPCollector()
	defer c.Close()

	exp := NewOTLPTraceExporter(OTLPTraceExporterConfig{
		Endpoint: c.Endpoint(), BatchSize: 1, FlushInterval: time.Hour,
		Headers: map[string]string{"X-Tenant": "team-a"},
	})
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-otlp-headers", exp)
	require.NoError(t, exp.ExportSpan(context.Background(), newTestSpan(tr, "op")))

	require.Eventually(t, func() bool { return c.Requests() >= 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, "team-a", c.LastHeader().Get("X-Tenant"))
}

func TestOTLPTraceExporterExportFailureLogsWithoutBlocking(t *testing.T) {
	c := NewMockOTLPCollector()
	c.SetStatus(http.StatusInternalServerError)
	defer c.Close()

	lc := verify.NewLogCapturer()
	ctx := lc.Attach(context.Background())
	defer lc.Detach()

	exp := NewOTLPTraceExporter(OTLPTraceExporterConfig{
		Endpoint: c.Endpoint(), BatchSize: 1, FlushInterval: time.Hour,
		Timeout: 2 * time.Second,
	})
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-otlp-fail", exp)
	start := time.Now()
	err := exp.ExportSpan(ctx, newTestSpan(tr, "op"))
	require.NoError(t, err, "a single buffered span should not block the caller")
	require.Less(t, time.Since(start), time.Second, "ExportSpan must not block on collector failure")

	// The worker flushes asynchronously against the still-500 collector; wait
	// for the error log before resetting the collector so the failure is real.
	require.Eventually(t, func() bool {
		for _, e := range lc.Entries() {
			if e.Level >= 0 && e.Message == "otlp export failed" {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond, "expected an 'otlp export failed' error log")

	c.SetStatus(http.StatusOK) // let any remaining settled flush succeed
	_ = exp.Shutdown(ctx)      //nolint:errcheck // may flush nothing
}

func TestOTLPTraceExporterContextCancellationPropagates(t *testing.T) {
	c := NewMockOTLPCollector()
	defer c.Close()

	// Large batch + long flush interval so the single span stays buffered and
	// only Shutdown (with the canceled context) triggers the flush.
	exp := NewOTLPTraceExporter(OTLPTraceExporterConfig{
		Endpoint: c.Endpoint(), BatchSize: 128, FlushInterval: time.Hour,
		Timeout: 5 * time.Second,
	})

	tr := NewTracer("trace-otlp-cancel", exp)
	require.NoError(t, exp.ExportSpan(context.Background(), newTestSpan(tr, "op")))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the final Shutdown flush

	err := exp.Shutdown(ctx)
	require.Error(t, err, "a canceled shutdown context must fail the flush")
	assert.True(t, errors.Is(err, context.Canceled),
		"expected context.Canceled to propagate, got %v", err)
}

func TestOTLPTraceExporterNoGoroutineLeak(t *testing.T) {
	c := NewMockOTLPCollector()
	defer verify.AssertNoGoroutineLeak(t)()
	defer c.Close()

	tr := NewTracer("trace-otlp-leak", nil)
	for i := 0; i < 5; i++ {
		exp := NewOTLPTraceExporter(OTLPTraceExporterConfig{
			Endpoint: c.Endpoint(), BatchSize: 2, FlushInterval: 10 * time.Millisecond,
		})
		require.NoError(t, exp.ExportSpan(context.Background(), newTestSpan(tr, "op")))
		require.NoError(t, exp.Shutdown(context.Background()))
	}
}
