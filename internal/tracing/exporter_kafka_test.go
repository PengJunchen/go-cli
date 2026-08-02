package tracing

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// MockKafkaBroker simulates a Kafka broker receiver with a minimal TCP
// listener. It accepts connections, reads length-prefixed frames, decodes
// them, and records the decoded kafkaFrame plus the raw compression flag for
// assertions.
type MockKafkaBroker struct {
	ln     net.Listener
	mu     sync.Mutex
	frames []kafkaFrame
	wg     sync.WaitGroup
	closed atomicBool
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) set(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }
func (b *atomicBool) get() bool  { b.mu.Lock(); defer b.mu.Unlock(); return b.v }

// NewMockKafkaBroker starts a mock broker on a random local port.
func NewMockKafkaBroker(t *testing.T) *MockKafkaBroker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "mock broker must listen on an ephemeral port")

	b := &MockKafkaBroker{ln: ln}
	b.wg.Add(1)
	go b.serve()
	return b
}

func (b *MockKafkaBroker) serve() {
	defer b.wg.Done()
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			if b.closed.get() {
				return
			}
			continue
		}
		b.handle(conn)
	}
}

func (b *MockKafkaBroker) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }() //nolint:errcheck // best-effort close.
	raw, err := readKafkaFrame(conn)
	if err != nil {
		return
	}
	frame, err := decodeKafkaFrame(raw)
	if err != nil {
		return
	}
	b.mu.Lock()
	b.frames = append(b.frames, frame)
	b.mu.Unlock()
}

// Addr returns the broker address for configuring an exporter.
func (b *MockKafkaBroker) Addr() string { return b.ln.Addr().String() }

// Frames returns a copy of the decoded frames received so far.
func (b *MockKafkaBroker) Frames() []kafkaFrame {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]kafkaFrame(nil), b.frames...)
}

// Close shuts down the broker listener and waits for the accept goroutine.
func (b *MockKafkaBroker) Close() {
	b.closed.set(true)
	_ = b.ln.Close() //nolint:errcheck // best-effort close.
	b.wg.Wait()
}

func TestKafkaTraceExporterConfigFields(t *testing.T) {
	cfg := KafkaTraceExporterConfig{
		Brokers:       []string{"127.0.0.1:9092"},
		Topic:         "traces",
		PartitionKey:  "partition-0",
		BatchSize:     8,
		FlushInterval: time.Millisecond,
		Compression:   "gzip",
		SASL:          map[string]string{"mechanism": "PLAIN"},
	}
	exp := NewKafkaTraceExporter(cfg)
	require.NoError(t, exp.Shutdown(context.Background()))

	assert.Equal(t, []string{"127.0.0.1:9092"}, cfg.Brokers)
	assert.Equal(t, "traces", cfg.Topic)
	assert.Equal(t, "partition-0", cfg.PartitionKey)
	assert.Equal(t, 8, cfg.BatchSize)
	assert.Equal(t, "gzip", cfg.Compression)
	require.Equal(t, "PLAIN", cfg.SASL["mechanism"])
}

func TestKafkaTraceExporterSendsToTopic(t *testing.T) {
	broker := NewMockKafkaBroker(t)
	defer verify.AssertNoGoroutineLeak(t)()
	defer broker.Close()

	exp := NewKafkaTraceExporter(KafkaTraceExporterConfig{
		Brokers: []string{broker.Addr()}, Topic: "traces",
		BatchSize: 1, FlushInterval: time.Hour,
	})
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-kafka", exp)
	span := newTestSpan(tr, "op.kafka")
	require.NoError(t, exp.ExportSpan(context.Background(), span))

	require.Eventually(t, func() bool { return len(broker.Frames()) >= 1 }, 2*time.Second, 5*time.Millisecond)
	frames := broker.Frames()
	require.Len(t, frames, 1)
	assert.Equal(t, "traces", frames[0].Topic)
	require.Len(t, frames[0].Spans, 1)
	assert.Equal(t, span.SpanID(), frames[0].Spans[0].SpanID)
	assert.Equal(t, "op.kafka", frames[0].Spans[0].Name)
}

func TestKafkaTraceExporterBatchExportByBatchSize(t *testing.T) {
	broker := NewMockKafkaBroker(t)
	defer broker.Close()

	exp := NewKafkaTraceExporter(KafkaTraceExporterConfig{
		Brokers: []string{broker.Addr()}, Topic: "traces",
		BatchSize: 3, FlushInterval: time.Hour,
	})
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-kafka-batch", exp)
	for i := 0; i < 3; i++ {
		require.NoError(t, exp.ExportSpan(context.Background(), newTestSpan(tr, "op")))
	}

	require.Eventually(t, func() bool { return len(broker.Frames()) >= 1 }, 2*time.Second, 5*time.Millisecond)
	frames := broker.Frames()
	require.Len(t, frames, 1, "three spans with batch size 3 must publish as one batch")
	require.Len(t, frames[0].Spans, 3)
}

func TestKafkaTraceExporterBatchExportByFlushInterval(t *testing.T) {
	broker := NewMockKafkaBroker(t)
	defer broker.Close()

	exp := NewKafkaTraceExporter(KafkaTraceExporterConfig{
		Brokers: []string{broker.Addr()}, Topic: "traces",
		BatchSize: 100, FlushInterval: 40 * time.Millisecond,
	})
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-kafka-interval", exp)
	span := newTestSpan(tr, "op")
	require.NoError(t, exp.ExportSpan(context.Background(), span))

	require.Eventually(t, func() bool { return len(broker.Frames()) >= 1 }, 2*time.Second, 5*time.Millisecond)
	frames := broker.Frames()
	require.Len(t, frames, 1)
	require.Len(t, frames[0].Spans, 1)
	assert.Equal(t, span.SpanID(), frames[0].Spans[0].SpanID)
}

func TestKafkaTraceExporterShutdownFlushesRemainingBuffer(t *testing.T) {
	broker := NewMockKafkaBroker(t)
	defer broker.Close()

	exp := NewKafkaTraceExporter(KafkaTraceExporterConfig{
		Brokers: []string{broker.Addr()}, Topic: "traces",
		BatchSize: 10, FlushInterval: time.Hour,
	})

	tr := NewTracer("trace-kafka-shutdown", exp)
	for i := 0; i < 5; i++ {
		require.NoError(t, exp.ExportSpan(context.Background(), newTestSpan(tr, "op")))
	}

	require.NoError(t, exp.Shutdown(context.Background()))
	require.Eventually(t, func() bool { return len(broker.Frames()) >= 1 }, 2*time.Second, 5*time.Millisecond)
	frames := broker.Frames()
	require.Len(t, frames, 1, "shutdown must publish the buffered batch")
	require.Len(t, frames[0].Spans, 5, "shutdown must publish all remaining buffered spans")
}

func TestKafkaTraceExporterCompressionHonored(t *testing.T) {
	for _, comp := range []string{"gzip", "snappy", "lz4"} {
		t.Run(comp, func(t *testing.T) {
			broker := NewMockKafkaBroker(t)
			defer broker.Close()

			exp := NewKafkaTraceExporter(KafkaTraceExporterConfig{
				Brokers: []string{broker.Addr()}, Topic: "traces",
				BatchSize: 1, FlushInterval: time.Hour, Compression: comp,
			})
			defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

			tr := NewTracer("trace-kafka-compress", exp)
			require.NoError(t, exp.ExportSpan(context.Background(), newTestSpan(tr, "op")))

			require.Eventually(t, func() bool { return len(broker.Frames()) >= 1 }, 2*time.Second, 5*time.Millisecond)
			frames := broker.Frames()
			require.Len(t, frames, 1)
			assert.Equal(t, comp, frames[0].Compression,
				"frame must advertise the configured compression algorithm")
			require.Len(t, frames[0].Spans, 1)
			assert.Equal(t, "op", frames[0].Spans[0].Name)
		})
	}
}

func TestKafkaTraceExporterSASLHonored(t *testing.T) {
	broker := NewMockKafkaBroker(t)
	defer broker.Close()

	exp := NewKafkaTraceExporter(KafkaTraceExporterConfig{
		Brokers: []string{broker.Addr()}, Topic: "traces",
		BatchSize: 1, FlushInterval: time.Hour,
		SASL: map[string]string{"mechanism": "PLAIN", "username": "svc"},
	})
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-kafka-sasl", exp)
	require.NoError(t, exp.ExportSpan(context.Background(), newTestSpan(tr, "op")))

	require.Eventually(t, func() bool { return len(broker.Frames()) >= 1 }, 2*time.Second, 5*time.Millisecond)
	frames := broker.Frames()
	require.Len(t, frames, 1)
	require.NotNil(t, frames[0].SASL, "SASL parameters must be present in the frame")
	assert.Equal(t, "PLAIN", frames[0].SASL["mechanism"])
	assert.Equal(t, "svc", frames[0].SASL["username"])
}

func TestKafkaTraceExporterPartitionKey(t *testing.T) {
	broker := NewMockKafkaBroker(t)
	defer broker.Close()

	exp := NewKafkaTraceExporter(KafkaTraceExporterConfig{
		Brokers: []string{broker.Addr()}, Topic: "traces", PartitionKey: "shard-7",
		BatchSize: 1, FlushInterval: time.Hour,
	})
	defer func() { _ = exp.Shutdown(context.Background()) }() //nolint:errcheck // cleanup is best-effort

	tr := NewTracer("trace-kafka-key", exp)
	require.NoError(t, exp.ExportSpan(context.Background(), newTestSpan(tr, "op")))

	require.Eventually(t, func() bool { return len(broker.Frames()) >= 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, "shard-7", broker.Frames()[0].PartitionKey)
}

func TestKafkaTraceExporterExportFailureDoesNotBlock(t *testing.T) {
	// Start a listener, grab its address, then close it so dialing fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadAddr := ln.Addr().String()
	_ = ln.Close() //nolint:errcheck // intentionally closed to force dial failure

	lc := verify.NewLogCapturer()
	ctx := lc.Attach(context.Background())
	defer lc.Detach()

	exp := NewKafkaTraceExporter(KafkaTraceExporterConfig{
		Brokers: []string{deadAddr}, Topic: "traces", BatchSize: 1, FlushInterval: time.Hour,
	})
	tr := NewTracer("trace-kafka-fail", exp)

	start := time.Now()
	err = exp.ExportSpan(ctx, newTestSpan(tr, "op"))
	require.NoError(t, err, "ExportSpan should not block or fail on the caller")
	require.Less(t, time.Since(start), time.Second, "ExportSpan must not block on broker failure")

	_ = exp.Shutdown(ctx) //nolint:errcheck // may flush nothing
	require.Eventually(t, func() bool {
		for _, e := range lc.Entries() {
			if e.Level >= 0 && e.Message == "kafka export failed" {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond, "expected a 'kafka export failed' error log")
}

func TestKafkaTraceExporterNoGoroutineLeak(t *testing.T) {
	broker := NewMockKafkaBroker(t)
	defer verify.AssertNoGoroutineLeak(t)()
	defer broker.Close()

	tr := NewTracer("trace-kafka-leak", nil)
	for i := 0; i < 5; i++ {
		exp := NewKafkaTraceExporter(KafkaTraceExporterConfig{
			Brokers: []string{broker.Addr()}, Topic: "traces", BatchSize: 2, FlushInterval: 10 * time.Millisecond,
		})
		require.NoError(t, exp.ExportSpan(context.Background(), newTestSpan(tr, "op")))
		require.NoError(t, exp.Shutdown(context.Background()))
	}
}

func TestKafkaTraceExporterContextCancellationPropagates(t *testing.T) {
	broker := NewMockKafkaBroker(t)
	defer broker.Close()

	// Large batch + long flush interval so the single span stays buffered and
	// only Shutdown (with the canceled context) triggers the publish.
	exp := NewKafkaTraceExporter(KafkaTraceExporterConfig{
		Brokers: []string{broker.Addr()}, Topic: "traces",
		BatchSize: 128, FlushInterval: time.Hour,
	})

	tr := NewTracer("trace-kafka-cancel", exp)
	require.NoError(t, exp.ExportSpan(context.Background(), newTestSpan(tr, "op")))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the final Shutdown publish

	err := exp.Shutdown(ctx)
	require.Error(t, err, "a canceled shutdown context must fail the publish")
	assert.True(t, errors.Is(err, context.Canceled),
		"expected context.Canceled to propagate, got %v", err)
}
