package tracing

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// failingExporter rejects every export so callers that must not stall on
// failures can be exercised deterministically.
type failingExporter struct {
	exports atomic.Int32
}

func (e *failingExporter) ExportSpan(_ context.Context, _ TraceSpan) error {
	e.exports.Add(1)
	return errors.New("boom: cannot export")
}

func (e *failingExporter) Shutdown(context.Context) error { return nil }

func (e *failingExporter) count() int32 { return e.exports.Load() }

// TestMultiExporterContinuesAfterPartialFailure verifies that a failure in one
// exporter does not prevent the others from receiving the span, and that the
// last error seen is returned.
func TestMultiExporterContinuesAfterPartialFailure(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ok1 := &failingExporter{}
	ok2 := &recordingExporter{}
	failing := &failingExporter{}

	m := NewMultiExporter(ok1, ok2, failing)
	tr := NewTracer("trace-multi-partial", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)

	// First exporter fails, the failing third also fails, but the middle one
	// must still record the span.
	err := m.ExportSpan(context.Background(), span)
	require.Error(t, err, "a failing exporter must surface its error")
	assert.Equal(t, int32(1), ok1.count())
	assert.Equal(t, int32(1), failing.count())
	require.Len(t, ok2.got, 1, "pass-through exporter must still receive the span")
}

// TestMultiExporterShutdownContinuesOnError verifies that Shutdown keeps going
// when one exporter's Shutdown fails, logs the failure, and returns the first
// error encountered without aborting remaining shutdowns.
func TestMultiExporterShutdownContinuesOnError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	lc := verify.NewLogCapturer()
	ctx := lc.Attach(context.Background())
	defer lc.Detach()

	rec := &recordingExporter{}
	bad := &shutdownErrorExporter{}
	m := NewMultiExporter(bad, &shutdownErrorExporter{}, rec)
	require.Error(t, m.Shutdown(ctx), "MultiExporter.Shutdown must propagate the first per-exporter error")

	require.Eventually(t, func() bool {
		for _, e := range lc.Entries() {
			if e.Message == "exporter shutdown failed" {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)

	// Verify all exporters were still shut down despite the errors.
	require.True(t, rec.shutdownCalled, "MultiExporter.Shutdown must continue shutting down all exporters even when some fail")
}

// shutdownErrorExporter always fails on Shutdown.
type shutdownErrorExporter struct{}

func (shutdownErrorExporter) ExportSpan(context.Context, TraceSpan) error { return nil }
func (shutdownErrorExporter) Shutdown(context.Context) error              { return errors.New("shutdown failed") }

// TestJSONLTraceExporterEmptySpanFields verifies the JSON line encodes optional
// fields and that an empty/no-op span still produces a valid trailing line.
func TestJSONLTraceExporterEmptySpanFields(t *testing.T) {
	dir := t.TempDir()
	exp, err := NewJSONLTraceExporter(dir, "session-fields")
	require.NoError(t, err)
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-fields", exp)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	span.SetStatus(SpanStatusError, "with-message")
	require.NoError(t, exp.ExportSpan(context.Background(), span))

	lines, err := readLines(exp.FilePath())
	require.NoError(t, err)
	require.Len(t, lines, 1)

	var data SpanData
	require.NoError(t, json.Unmarshal(lines[0], &data))
	assert.Equal(t, SpanStatusError, data.Status)
	assert.Equal(t, "with-message", data.StatusMessage)
	assert.Equal(t, "trace-fields", data.TraceID)
}

// TestJSONLTraceExporterFilePath verifies the exported file path derives from
// the directory and session id.
func TestJSONLTraceExporterFilePath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "traces")
	exp, err := NewJSONLTraceExporter(dir, "session-xyz")
	require.NoError(t, err)
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	assert.Equal(t, filepath.Join(dir, "session-xyz.jsonl"), exp.FilePath())
	_, err = os.Stat(exp.FilePath())
	require.NoError(t, err)
}

// TestJSONLTraceExporterDoubleShutdown verifies Shutdown is safe to call more
// than once.
func TestJSONLTraceExporterDoubleShutdown(t *testing.T) {
	dir := t.TempDir()
	exp, err := NewJSONLTraceExporter(dir, "session-close")
	require.NoError(t, err)
	require.NoError(t, exp.Shutdown(context.Background()))
	require.NotPanics(t, func() {
		_ = exp.Shutdown(context.Background()) //nolint:errcheck // second shutdown is best-effort
	})
}

// TestJSONLTraceExporterCreateDirFailure verifies creation of the trace
// directory propagates as an error (using a path that cannot be created).
func TestJSONLTraceExporterCreateDirFailure(t *testing.T) {
	// A regular file as the parent makes MkdirAll fail.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	_, err := NewJSONLTraceExporter(filepath.Join(blocker, "sub"), "sid")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create trace dir")
}

// TestStdoutTraceExporterIndent verifies the indented marshaling path.
func TestStdoutTraceExporterIndent(t *testing.T) {
	var sb strings.Builder
	exp := NewStdoutTraceExporterWithWriter(true, &sb)

	tr := NewTracer("trace-stdout-indent", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	span.SetAttributes(Attribute{Key: "k", Value: "v"})
	require.NoError(t, exp.ExportSpan(context.Background(), span))

	out := sb.String()
	require.True(t, strings.HasPrefix(out, "[TRACE] "), "output %q must be prefixed", out)
	// Indented output contains the attribute key name and no trailing compact
	// JSON brace pattern.
	assert.Contains(t, out, "trace-stdout-indent")
	assert.Contains(t, out, `"k"`)
	// The prefixed line, when stripped, must still be valid JSON.
	line := strings.TrimPrefix(out, "[TRACE] ")
	var data SpanData
	require.NoError(t, json.Unmarshal([]byte(line), &data))
	assert.Equal(t, "trace-stdout-indent", data.TraceID)
}

// TestStdoutTraceExporterNilWriterFallsBack verifies that a nil writer selects
// os.Stdout rather than panicking.
func TestStdoutTraceExporterNilWriterFallsBack(t *testing.T) {
	exp := NewStdoutTraceExporterWithWriter(true, nil)
	require.NotNil(t, exp)
	require.NoError(t, exp.Shutdown(context.Background()))
}

// TestAsyncExporterExportFailureLogsWithoutStalling verifies that a failing
// inner exporter is logged and the worker keeps running (span is not re-tried
// but the loop is not wedged).
func TestAsyncExporterExportFailureLogsWithoutStalling(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	lc := verify.NewLogCapturer()
	ctx := lc.Attach(context.Background())
	defer lc.Detach()

	inner := &failingExporter{}
	async := NewAsyncExporter(inner, 16, 1)
	defer func() { _ = async.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-async-fail", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	require.NoError(t, async.ExportSpan(ctx, span))

	require.Eventually(t, func() bool {
		for _, e := range lc.Entries() {
			if e.Message == "failed to flush span" {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond, "expected a 'failed to flush span' log")

	// A second export must still be accepted (worker did not stall).
	span2, _ := tr.Start(context.Background(), "op2", SpanKindInternal)
	require.NoError(t, async.ExportSpan(ctx, span2))
	require.Eventually(t, func() bool { return inner.count() >= 2 }, 2*time.Second, 10*time.Millisecond)
}

// TestAsyncExporterShutdownPropagatesInnerError verifies Shutdown surfaces the
// inner exporter's shutdown error.
func TestAsyncExporterShutdownPropagatesInnerError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	async := NewAsyncExporter(&shutdownErrorExporter{}, 8, 4)
	err := async.Shutdown(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shutdown failed")
}

// TestOTLPTraceExporterConcurrentSpanExportWhileFlusherRuns verifies concurrent
// ExportSpan calls while the background flusher is draining batches, under the
// race detector. Every exported span must be attributable to a batch.
func TestOTLPTraceExporterConcurrentSpanExportWhileFlusherRuns(t *testing.T) {
	c := NewMockOTLPCollector()
	defer c.Close()

	exp := NewOTLPTraceExporter(OTLPTraceExporterConfig{
		Endpoint: c.Endpoint(), BatchSize: 10, FlushInterval: 10 * time.Millisecond,
	})
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-otlp-race", nil)

	const workers = 8
	const perWorker = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
				require.NoError(t, exp.ExportSpan(context.Background(), span))
			}
		}()
	}
	wg.Wait()

	require.NoError(t, exp.Shutdown(context.Background()))

	total := 0
	for _, body := range c.Bodies() {
		total += len(decodePayload(t, body).Spans)
	}
	assert.Equal(t, workers*perWorker, total, "all exported spans must be flushed")
}

// TestKafkaTraceExporterConcurrentPublishWhileFlusherRuns exercises concurrent
// ExportSpan against a live broker under the race detector.
func TestKafkaTraceExporterConcurrentPublishWhileFlusherRuns(t *testing.T) {
	broker := NewMockKafkaBroker(t)
	defer broker.Close()

	exp := NewKafkaTraceExporter(KafkaTraceExporterConfig{
		Brokers: []string{broker.Addr()}, Topic: "race", BatchSize: 8, FlushInterval: 10 * time.Millisecond,
	})
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-kafka-race", nil)

	const workers = 6
	const perWorker = 40
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
				require.NoError(t, exp.ExportSpan(context.Background(), span))
			}
		}()
	}
	wg.Wait()
	require.NoError(t, exp.Shutdown(context.Background()))

	// Broker frame decoding is asynchronous; poll until the full set of spans
	// has been published.
	total := func() int {
		n := 0
		for _, f := range broker.Frames() {
			n += len(f.Spans)
		}
		return n
	}
	require.Eventually(t, func() bool {
		return total() == workers*perWorker
	}, 3*time.Second, 10*time.Millisecond, "expected all exported spans to be published")
}

// TestEncodeKafkaFrameRoundTrip verifies encode/decode round-trips across the
// supported compression algorithms and metadata preservation.
func TestEncodeKafkaFrameRoundTrip(t *testing.T) {
	frames := []kafkaFrame{
		{
			Topic: "t", PartitionKey: "k0", Compression: "",
			Spans: []SpanData{{SpanID: "s1", Name: "op"}},
		},
		{
			Topic: "t", PartitionKey: "k1", Compression: "gzip",
			SASL:  map[string]string{"mechanism": "PLAIN", "username": "u"},
			Spans: []SpanData{{SpanID: "s2", Name: "op2"}},
		},
		{
			Topic: "t", Compression: "snappy",
			Spans: []SpanData{{SpanID: "s3", Name: "op3"}},
		},
		{
			Topic: "t", Compression: "lz4",
			Spans: []SpanData{{SpanID: "s4", Name: "op4"}},
		},
	}
	for _, f := range frames {
		raw, err := encodeKafkaFrame(f)
		require.NoError(t, err)
		got, err := decodeKafkaFrame(raw)
		require.NoError(t, err)
		assert.Equal(t, f.Topic, got.Topic)
		assert.Equal(t, f.PartitionKey, got.PartitionKey)
		assert.Equal(t, f.Compression, got.Compression)
		assert.Equal(t, f.SASL, got.SASL)
		require.Len(t, got.Spans, len(f.Spans))
		assert.Equal(t, f.Spans[0].SpanID, got.Spans[0].SpanID)
		assert.Equal(t, f.Spans[0].Name, got.Spans[0].Name)
	}
}

// TestDecodeKafkaFrameInvalidInputs exercises the decode error paths.
func TestDecodeKafkaFrameInvalidInputs(t *testing.T) {
	// Not JSON at all.
	_, err := decodeKafkaFrame([]byte("not json"))
	require.Error(t, err)

	// Base64 payload is invalid.
	_, err = decodeKafkaFrame([]byte(`{"topic":"t","compression":"","spans":"!!!not-base64"}`))
	require.Error(t, err)

	// Advertised gzip compression but the payload is not gzip data.
	badGzip := []byte(`{"topic":"t","compression":"gzip","spans":"aGVsbG8="}`) // "hello" base64
	_, err = decodeKafkaFrame(badGzip)
	require.Error(t, err)
}

// TestReadKafkaFrameTooLarge verifies the max frame guard.
func TestReadKafkaFrameTooLarge(t *testing.T) {
	var hdr [4]byte
	// 80 MiB, above the 64 MiB cap.
	hdr[0] = 0x05
	_, err := readKafkaFrame(strings.NewReader(string(hdr[:])))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "frame too large")
}

// TestReadKafkaFrameTruncated verifies a short read yields an error.
func TestReadKafkaFrameTruncated(t *testing.T) {
	var hdr [4]byte
	hdr[3] = 10 // 10 bytes promised, but none follow
	_, err := readKafkaFrame(strings.NewReader(string(hdr[:])))
	require.Error(t, err)
}

// TestReadKafkaFrameRoundTrip exercises a full frame through the wire helpers.
func TestReadKafkaFrameRoundTrip(t *testing.T) {
	payload := []byte(`{"topic":"t","compression":"gzip","spans":"e30="}`)
	var lengthBuf [4]byte
	lengthBuf[3] = byte(len(payload))

	var sb strings.Builder
	sb.Write(lengthBuf[:])
	sb.Write(payload)

	raw, err := readKafkaFrame(bufio.NewReader(strings.NewReader(sb.String())))
	require.NoError(t, err)
	assert.Equal(t, payload, raw)
}

// TestNormalizeBatchSizeClampsToMinimum exercises the config normalizers.
func TestNormalizeBatchSizeClampsToMinimum(t *testing.T) {
	assert.Equal(t, 1, normalizeBatchSize(0))
	assert.Equal(t, 1, normalizeBatchSize(-5))
	assert.Equal(t, 8, normalizeBatchSize(8))
}

func TestNormalizeIntervalDefaults(t *testing.T) {
	assert.Equal(t, exporterDefaultFlushInterval, normalizeInterval(0))
	assert.Equal(t, exporterDefaultFlushInterval, normalizeInterval(-time.Second))
	assert.Equal(t, 5*time.Millisecond, normalizeInterval(5*time.Millisecond))
}
