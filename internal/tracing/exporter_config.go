package tracing

import "time"

// OTLPTraceExporterConfig configures an OTLPTraceExporter. It follows the OTLP
// HTTP contract but, to keep go-cli dependency-free, uses a JSON-over-HTTP
// framing instead of the protobuf wire format (see exporter_otlp.go).
type OTLPTraceExporterConfig struct {
	// Endpoint is the collector URL, e.g. "http://localhost:4318/v1/traces".
	Endpoint string
	// Headers are extra HTTP headers to attach to every export request.
	Headers map[string]string
	// Timeout bounds each HTTP export request.
	Timeout time.Duration
	// Insecure disables TLS verification. It is honored when constructing the
	// HTTP transport.
	Insecure bool
	// BatchSize is the maximum number of spans buffered before an immediate
	// export is triggered.
	BatchSize int
	// FlushInterval is how often buffered spans are flushed on a timer.
	FlushInterval time.Duration
}

// KafkaTraceExporterConfig configures a KafkaTraceExporter. It implements a
// minimal producer framing over stdlib net TCP rather than the full Kafka
// wire protocol (see exporter_kafka.go).
type KafkaTraceExporterConfig struct {
	// Brokers is the list of broker addresses, e.g. ["127.0.0.1:9092"].
	Brokers []string
	// Topic is the Kafka topic spans are published to.
	Topic string
	// PartitionKey is the partition key attached to each published batch.
	PartitionKey string
	// BatchSize is the maximum number of spans buffered before an immediate
	// export is triggered.
	BatchSize int
	// FlushInterval is how often buffered spans are flushed on a timer.
	FlushInterval time.Duration
	// Compression is the compression algorithm: "", "gzip", "snappy" or "lz4".
	// snappy and lz4 are mapped to gzip (the only algorithm in the stdlib).
	Compression string
	// SASL carries SASL authentication parameters honored in the frame, e.g.
	// {"mechanism": "PLAIN", "username": "...", "password": "..."}.
	SASL map[string]string
}

// exporterDefaultFlushInterval is used when a FlushInterval is zero or negative.
const exporterDefaultFlushInterval = time.Second

// exporterDefaultHTTPTimeout is used when an OTLP Timeout is zero or negative.
const exporterDefaultHTTPTimeout = 5 * time.Second

// normalizeBatchSize clamps batch sizes to a minimum of 1.
func normalizeBatchSize(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// normalizeInterval returns a positive flush interval, defaulting when needed.
func normalizeInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return exporterDefaultFlushInterval
	}
	return d
}
