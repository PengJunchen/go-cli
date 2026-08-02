package tracing

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Kafka frame. This exporter implements a minimal producer over stdlib net TCP
// rather than the full Kafka wire protocol (which carries 16+ request/response
// message types, record batches and CRC validation). Each flush is encoded as
// a single length-prefixed JSON frame written to the broker:
//
//	[ 4-byte big-endian payload length ][ payload ]
//
// The payload is a JSON object describing the batch. Its "spans" array is
// gzip-compressed when a compression algorithm is configured, while the frame
// metadata (topic, partition key, compression, SASL) stays uncompressed so a
// receiver can always read and honor it. This is documented as a deliberate
// simplification of a real Kafka producer.
type kafkaFrame struct {
	Topic        string            `json:"topic"`
	PartitionKey string            `json:"partition_key"`
	Compression  string            `json:"compression"`
	SASL         map[string]string `json:"sasl,omitempty"`
	Spans        []SpanData        `json:"spans,omitempty"`
}

// kafkaFrameWire is the on-the-wire representation: the spans array is carried
// as a base64 string of (possibly gzip-compressed) JSON, so arbitrary binary
// bytes survive JSON transport and a receiver can decode them based on the
// advertised Compression value.
type kafkaFrameWire struct {
	Topic        string            `json:"topic"`
	PartitionKey string            `json:"partition_key"`
	Compression  string            `json:"compression"`
	SASL         map[string]string `json:"sasl,omitempty"`
	Spans        string            `json:"spans"`
}

// KafkaTraceExporter exports spans to a Kafka topic with a minimal TCP
// framing. It buffers spans and publishes batches either when BatchSize is
// reached or on a FlushInterval timer. Compression and SASL are honored in the
// frame. A background goroutine performs the publishes so ExportSpan stays
// non-blocking. It does not emit its own spans; failures are logged with
// slog.ErrorContext and latency with slog.DebugContext.
type KafkaTraceExporter struct {
	cfg          KafkaTraceExporterConfig
	batchSize    int
	mu           sync.Mutex
	buffer       []SpanData
	trigger      chan struct{}
	done         chan struct{}
	wg           sync.WaitGroup
	shutdownOnce sync.Once
}

// Compile-time assertion that KafkaTraceExporter satisfies TraceExporter.
var _ TraceExporter = (*KafkaTraceExporter)(nil)

// NewKafkaTraceExporter creates a KafkaTraceExporter with the given config.
func NewKafkaTraceExporter(cfg KafkaTraceExporterConfig) *KafkaTraceExporter {
	batchSize := normalizeBatchSize(cfg.BatchSize)
	interval := normalizeInterval(cfg.FlushInterval)

	e := &KafkaTraceExporter{
		cfg:       cfg,
		batchSize: batchSize,
		buffer:    make([]SpanData, 0, batchSize),
		trigger:   make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	e.wg.Add(1)
	go e.flushLoop(interval)
	return e
}

// ExportSpan buffers the span and, when the batch fills, signals the flush
// goroutine. It never blocks on network I/O.
func (e *KafkaTraceExporter) ExportSpan(_ context.Context, span TraceSpan) error {
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

// Shutdown stops the flush goroutine, waits for it to exit, then publishes any
// remaining buffered spans.
func (e *KafkaTraceExporter) Shutdown(ctx context.Context) error {
	e.shutdownOnce.Do(func() {
		close(e.done)
	})
	e.wg.Wait()
	return e.flushBatch(ctx)
}

// flushLoop publishes buffered spans on a FlushInterval timer or when a batch
// fills (trigger). The ticker is always stopped on exit so no goroutines leak.
func (e *KafkaTraceExporter) flushLoop(interval time.Duration) {
	defer e.wg.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.trigger:
			_ = e.flushBatch(context.Background()) //nolint:errcheck // best-effort; errors logged inside
		case <-ticker.C:
			_ = e.flushBatch(context.Background()) //nolint:errcheck // best-effort; errors logged inside
		case <-e.done:
			return
		}
	}
}

// flushBatch snapshots the current buffer and publishes it.
func (e *KafkaTraceExporter) flushBatch(ctx context.Context) error {
	e.mu.Lock()
	if len(e.buffer) == 0 {
		e.mu.Unlock()
		return nil
	}
	batch := e.buffer
	e.buffer = e.buffer[:0]
	e.mu.Unlock()

	return e.sendBatch(ctx, batch)
}

// sendBatch encodes the spans into a Kafka frame, applies the configured
// compression, and writes it to the first broker over a TCP connection.
func (e *KafkaTraceExporter) sendBatch(ctx context.Context, batch []SpanData) error {
	if len(e.cfg.Brokers) == 0 {
		err := fmt.Errorf("kafka export failed: no brokers configured")
		slog.ErrorContext(ctx, "kafka export failed", "op", "kafka.export", "err", err)
		return err
	}

	start := time.Now()

	frame := kafkaFrame{
		Topic:        e.cfg.Topic,
		PartitionKey: e.cfg.PartitionKey,
		Compression:  e.cfg.Compression,
		SASL:         e.cfg.SASL,
		Spans:        batch,
	}

	compressed, err := encodeKafkaFrame(frame)
	if err != nil {
		slog.ErrorContext(ctx, "kafka export failed", "op", "kafka.export", "spans", len(batch), "err", err)
		return fmt.Errorf("encode kafka frame: %w", err)
	}

	addr := e.cfg.Brokers[0]
	if err := writeKafkaFrame(ctx, addr, compressed); err != nil {
		slog.ErrorContext(ctx, "kafka export failed", "op", "kafka.export", "addr", addr, "spans", len(batch), "err", err)
		return err
	}

	slog.DebugContext(ctx, "kafka export latency",
		"op", "kafka.export", "spans", len(batch), "duration_ms", time.Since(start).Milliseconds())
	return nil
}

// encodeKafkaFrame marshals the frame to JSON, gzip-compressing the spans
// array when a compression algorithm is configured. snappy and lz4 map to gzip
// (the only compression algorithm in the stdlib); the frame metadata still
// advertises the configured algorithm so receivers observe it.
func encodeKafkaFrame(frame kafkaFrame) ([]byte, error) {
	spansBytes, err := json.Marshal(frame.Spans)
	if err != nil {
		return nil, err
	}

	compression := frame.Compression
	switch compression {
	case "gzip", "snappy", "lz4":
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(spansBytes); err != nil {
			return nil, err
		}
		if err := gw.Close(); err != nil {
			return nil, err
		}
		spansBytes = buf.Bytes()
	}

	wire := kafkaFrameWire{
		Topic:        frame.Topic,
		PartitionKey: frame.PartitionKey,
		Compression:  frame.Compression,
		SASL:         frame.SASL,
		Spans:        base64.StdEncoding.EncodeToString(spansBytes),
	}
	return json.Marshal(wire)
}

// writeKafkaFrame dials addr and writes frame as a length-prefixed payload,
// honoring ctx cancellation/Deadline on both dial and write.
func writeKafkaFrame(ctx context.Context, addr string, frame []byte) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }() //nolint:errcheck // best-effort close.

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline) //nolint:errcheck // best-effort deadline.
	}

	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))

	if _, err := conn.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := conn.Write(frame); err != nil {
		return err
	}
	return nil
}

// readKafkaFrame reads a single length-prefixed frame from r and returns the
// raw (possibly compressed) payload.
func readKafkaFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(hdr[:])
	if length > 64<<20 {
		return nil, fmt.Errorf("kafka frame too large: %d bytes", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// decodeKafkaFrame decodes a raw frame payload. When the advertised
// compression is one of the recognized algorithms, the spans array is first
// un-gzipped.
func decodeKafkaFrame(raw []byte) (kafkaFrame, error) {
	var wire kafkaFrameWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return kafkaFrame{}, err
	}

	spansBytes, err := base64.StdEncoding.DecodeString(wire.Spans)
	if err != nil {
		return kafkaFrame{}, err
	}
	switch wire.Compression {
	case "gzip", "snappy", "lz4":
		gr, gerr := gzip.NewReader(bytes.NewReader(spansBytes))
		if gerr != nil {
			return kafkaFrame{}, gerr
		}
		defer func() { _ = gr.Close() }() //nolint:errcheck // best-effort close.
		spansBytes, err = io.ReadAll(gr)
		if err != nil {
			return kafkaFrame{}, err
		}
	}

	var spans []SpanData
	if err := json.Unmarshal(spansBytes, &spans); err != nil {
		return kafkaFrame{}, err
	}

	return kafkaFrame{
		Topic:        wire.Topic,
		PartitionKey: wire.PartitionKey,
		Compression:  wire.Compression,
		SASL:         wire.SASL,
		Spans:        spans,
	}, nil
}
