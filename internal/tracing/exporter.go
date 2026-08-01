package tracing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TraceExporter abstracts span export. Default implementations are
// JSONLTraceExporter (file) and StdoutTraceExporter (debug output). Users may
// implement this interface to export to OpenTelemetry/Kafka etc.
type TraceExporter interface {
	// ExportSpan exports a completed span. It should be non-blocking and
	// should guarantee at-least-once delivery.
	ExportSpan(ctx context.Context, span TraceSpan) error
	// Shutdown closes the exporter and flushes all buffered data.
	// No new spans should be accepted after Shutdown is called.
	Shutdown(ctx context.Context) error
}

// spanToData converts any TraceSpan to its serializable SpanData form.
func spanToData(span TraceSpan) SpanData {
	if ls, ok := span.(*localSpan); ok {
		return ls.ToSpanData()
	}
	// For custom TraceSpan implementations, only the fields exposed through
	// the interface are available.
	return SpanData{
		TraceID:      span.TraceID(),
		SpanID:       span.SpanID(),
		ParentSpanID: span.ParentSpanID(),
		Name:         span.Name(),
		StartTime:    span.StartTime().Format(time.RFC3339Nano),
		EndTime:      span.EndTime().Format(time.RFC3339Nano),
	}
}

// MultiExporter exports each span to multiple exporters. A failure in one
// exporter does not prevent the others from running.
type MultiExporter struct {
	exporters []TraceExporter
}

// NewMultiExporter creates a MultiExporter that fans out to the given exporters.
func NewMultiExporter(exporters ...TraceExporter) *MultiExporter {
	return &MultiExporter{exporters: exporters}
}

// ExportSpan exports the span to every exporter, returning the last error seen.
func (m *MultiExporter) ExportSpan(ctx context.Context, span TraceSpan) error {
	var lastErr error
	for _, exp := range m.exporters {
		if err := exp.ExportSpan(ctx, span); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// Shutdown shuts down all exporters.
func (m *MultiExporter) Shutdown(ctx context.Context) error {
	for _, exp := range m.exporters {
		if err := exp.Shutdown(ctx); err != nil {
			slog.Warn("exporter shutdown failed", "err", err)
		}
	}
	return nil
}

var _ TraceExporter = (*MultiExporter)(nil)

// JSONLTraceExporter writes spans to a file in JSON Lines format.
// Default path: .go-cli/traces/{session_id}.jsonl. Each line is one JSON
// object so the file can be analyzed with tools like jq.
type JSONLTraceExporter struct {
	filePath string
	mu       sync.Mutex
	file     *os.File
	encoder  *json.Encoder
}

var _ TraceExporter = (*JSONLTraceExporter)(nil)

// NewJSONLTraceExporter creates a file exporter. dir is typically
// .go-cli/traces; sessionID is used to derive the file name.
func NewJSONLTraceExporter(dir, sessionID string) (*JSONLTraceExporter, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create trace dir: %w", err)
	}

	filePath := filepath.Join(dir, sessionID+".jsonl")
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open trace file: %w", err)
	}

	return &JSONLTraceExporter{
		filePath: filePath,
		file:     file,
		encoder:  json.NewEncoder(file),
	}, nil
}

// FilePath returns the path of the trace file.
func (e *JSONLTraceExporter) FilePath() string { return e.filePath }

// ExportSpan serializes the span and writes one JSON line. It is
// concurrency-safe.
func (e *JSONLTraceExporter) ExportSpan(_ context.Context, span TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.encoder.Encode(spanToData(span))
}

// Shutdown closes the underlying trace file.
func (e *JSONLTraceExporter) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.file != nil {
		return e.file.Close()
	}
	return nil
}

// StdoutTraceExporter prints spans as JSON to an io.Writer, prefixed with
// "[TRACE] ". It is intended for debugging.
type StdoutTraceExporter struct {
	indent bool
	w      io.Writer
}

var _ TraceExporter = (*StdoutTraceExporter)(nil)

// NewStdoutTraceExporter creates a StdoutTraceExporter writing to os.Stdout.
func NewStdoutTraceExporter(indent bool) *StdoutTraceExporter {
	return NewStdoutTraceExporterWithWriter(indent, os.Stdout)
}

// NewStdoutTraceExporterWithWriter creates a StdoutTraceExporter writing to the
// given writer. The writer must not be nil.
func NewStdoutTraceExporterWithWriter(indent bool, w io.Writer) *StdoutTraceExporter {
	if w == nil {
		w = os.Stdout
	}
	return &StdoutTraceExporter{indent: indent, w: w}
}

// ExportSpan marshals the span and prints it to the configured writer.
func (e *StdoutTraceExporter) ExportSpan(_ context.Context, span TraceSpan) error {
	data := spanToData(span)

	var dataBytes []byte
	var err error
	if e.indent {
		dataBytes, err = json.MarshalIndent(data, "", "  ")
	} else {
		dataBytes, err = json.Marshal(data)
	}
	if err != nil {
		return fmt.Errorf("marshal span: %w", err)
	}

	_, err = fmt.Fprintf(e.w, "[TRACE] %s\n", dataBytes)
	return err
}

// Shutdown is a no-op for the stdout exporter.
func (e *StdoutTraceExporter) Shutdown(_ context.Context) error { return nil }
