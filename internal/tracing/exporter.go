package tracing

import (
	"context"
	"encoding/json"
	"errors"
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

// SpanToData converts any TraceSpan to its serializable SpanData form.
// It is exported so that external TraceExporter implementations (e.g. mock
// exporters in tests) can access the full span data including attributes,
// events, and status.
func SpanToData(span TraceSpan) SpanData {
	if dp, ok := span.(SpanDataProvider); ok {
		return dp.SpanData()
	}
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

// SpanDataProvider is implemented by spans that carry pre-computed SpanData.
// SpanToData checks for this interface first so wrappers like RedactingExporter
// can supply modified data without re-reading the original span.
type SpanDataProvider interface {
	SpanData() SpanData
}

// dataSpan wraps a SpanData so it can be passed as a TraceSpan to inner
// exporters. It implements both TraceSpan and SpanDataProvider; SpanToData
// returns the embedded data directly without re-reading span fields.
type dataSpan struct {
	data SpanData
	ctx  context.Context
}

func (s *dataSpan) SpanData() SpanData   { return s.data }
func (s *dataSpan) TraceID() string      { return s.data.TraceID }
func (s *dataSpan) SpanID() string       { return s.data.SpanID }
func (s *dataSpan) ParentSpanID() string { return s.data.ParentSpanID }
func (s *dataSpan) Name() string         { return s.data.Name }
func (s *dataSpan) StartTime() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s.data.StartTime)
	return t
}
func (s *dataSpan) EndTime() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s.data.EndTime)
	return t
}
func (s *dataSpan) SetAttributes(...Attribute)    {}
func (s *dataSpan) AddEvent(string, ...Attribute) {}
func (s *dataSpan) SetStatus(SpanStatus, string)  {}
func (s *dataSpan) End()                          {}
func (s *dataSpan) Context() context.Context      { return s.ctx }

var _ TraceSpan = (*dataSpan)(nil)

// RedactionLevel controls how the RedactingExporter treats attributes.
type RedactionLevel string

const (
	// RedactionLevelFull exports all attributes as-is.
	RedactionLevelFull RedactionLevel = "full"
	// RedactionLevelRedact masks sensitive attributes (default).
	RedactionLevelRedact RedactionLevel = "redact"
	// RedactionLevelOff strips ALL attributes from exported spans.
	RedactionLevelOff RedactionLevel = "off"
)

// redactedValue is the placeholder used for masked sensitive attributes.
const redactedValue = "***REDACTED***"

// RedactingExporter wraps a TraceExporter and masks or strips sensitive
// attributes before delegating to the inner exporter. It is non-destructive:
// the original span is never modified.
type RedactingExporter struct {
	inner TraceExporter
	level RedactionLevel
}

var _ TraceExporter = (*RedactingExporter)(nil)

// NewRedactingExporter wraps inner with a redaction layer. When level is
// empty, RedactionLevelRedact is used.
func NewRedactingExporter(inner TraceExporter, level RedactionLevel) *RedactingExporter {
	return &RedactingExporter{inner: inner, level: level}
}

// ExportSpan applies the redaction policy to a copy of the span data and
// forwards it to the inner exporter.
func (e *RedactingExporter) ExportSpan(ctx context.Context, span TraceSpan) error {
	data := SpanToData(span)
	// Deep-copy mutable slices so the original span is never modified.
	data.Attributes = cloneAttributes(data.Attributes)
	data.Events = cloneEvents(data.Events)

	switch e.level {
	case RedactionLevelOff:
		// Strip all attributes.
		data.Attributes = nil
		for i := range data.Events {
			data.Events[i].Attributes = nil
		}
	case RedactionLevelFull:
		// Export as-is, no modification.
	default: // RedactionLevelRedact or empty
		for i := range data.Attributes {
			if data.Attributes[i].Sensitive {
				data.Attributes[i].Value = redactedValue
			}
		}
		for i := range data.Events {
			for j := range data.Events[i].Attributes {
				if data.Events[i].Attributes[j].Sensitive {
					data.Events[i].Attributes[j].Value = redactedValue
				}
			}
		}
	}

	return e.inner.ExportSpan(ctx, &dataSpan{data: data, ctx: ctx})
}

// cloneAttributes returns a shallow copy of attrs. The elements (Attribute
// structs) are copied by value, so mutating an element's field does not affect
// the original slice.
func cloneAttributes(attrs []Attribute) []Attribute {
	if attrs == nil {
		return nil
	}
	cp := make([]Attribute, len(attrs))
	copy(cp, attrs)
	return cp
}

// cloneEvents returns a deep copy of events, including each event's attributes.
func cloneEvents(events []SpanEvent) []SpanEvent {
	if events == nil {
		return nil
	}
	cp := make([]SpanEvent, len(events))
	copy(cp, events)
	for i := range cp {
		cp[i].Attributes = cloneAttributes(cp[i].Attributes)
	}
	return cp
}

// Shutdown delegates to the inner exporter.
func (e *RedactingExporter) Shutdown(ctx context.Context) error {
	return e.inner.Shutdown(ctx)
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

// ExportSpan exports the span to every exporter, returning all errors
// aggregated via errors.Join. A failure in one exporter does not prevent the
// others from running; every exporter is attempted. The returned error is nil
// only when all exporters succeed.
func (m *MultiExporter) ExportSpan(ctx context.Context, span TraceSpan) error {
	var errs []error
	for _, exp := range m.exporters {
		if err := exp.ExportSpan(ctx, span); err != nil {
			slog.Warn("multi-exporter: export failed", "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Shutdown shuts down all exporters, returning the first error encountered.
func (m *MultiExporter) Shutdown(ctx context.Context) error {
	var firstErr error
	for _, exp := range m.exporters {
		if err := exp.Shutdown(ctx); err != nil {
			slog.Warn("exporter shutdown failed", "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
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
	return e.encoder.Encode(SpanToData(span))
}

// Shutdown closes the underlying trace file.
func (e *JSONLTraceExporter) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.file != nil {
		err := e.file.Close()
		e.file = nil
		return err
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
	data := SpanToData(span)

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

	_, err = fmt.Fprintf(e.w, "[TRACE] %s\n", dataBytes) //nolint:errcheck
	return err
}

// Shutdown is a no-op for the stdout exporter.
func (e *StdoutTraceExporter) Shutdown(_ context.Context) error { return nil }
