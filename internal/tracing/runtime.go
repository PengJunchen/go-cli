package tracing

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"
)

var ErrUnsupportedExporter = errors.New("unsupported tracing exporter")

// UnsupportedExporterError reports a tracing sink that the CLI runtime does
// not implement as a supported exporter.
type UnsupportedExporterError struct {
	Name string
}

func (e *UnsupportedExporterError) Error() string {
	return fmt.Sprintf("%v: %q", ErrUnsupportedExporter, e.Name)
}

func (e *UnsupportedExporterError) Unwrap() error { return ErrUnsupportedExporter }

// TracingRuntimeConfig contains the settings needed to build the CLI tracing
// runtime without depending on the config package.
type TracingRuntimeConfig struct {
	Enabled        bool
	Exporter       string
	FilePath       string
	SessionID      string
	RedactionLevel RedactionLevel
	Stdout         io.Writer
}

// IsSupportedExporter reports whether name is accepted by the runtime. An
// empty name selects the default JSONL exporter.
func IsSupportedExporter(name string) bool {
	switch name {
	case "", "jsonl", "stdout", "none":
		return true
	default:
		return false
	}
}

// BuildTracingRuntime creates the invocation-owned tracer and exporter.
// Disabled tracing and "none" return nil runtime components without creating
// files or workers. The only enabled sinks are JSONL and stdout.
func BuildTracingRuntime(cfg TracingRuntimeConfig) (*Tracer, TraceExporter, error) {
	if !cfg.Enabled {
		return nil, nil, nil
	}

	exporterName := cfg.Exporter
	if exporterName == "" {
		exporterName = "jsonl"
	}
	if exporterName == "none" {
		return nil, nil, nil
	}
	if !IsSupportedExporter(exporterName) {
		return nil, nil, &UnsupportedExporterError{Name: exporterName}
	}

	var sink TraceExporter
	switch exporterName {
	case "stdout":
		sink = NewStdoutTraceExporterWithWriter(false, cfg.Stdout)
	case "jsonl":
		dir := cfg.FilePath
		if dir == "" {
			dir = filepath.Join(".go-cli", "traces")
		}
		sessionID := cfg.SessionID
		if sessionID == "" {
			sessionID = "session-" + time.Now().Format("20060102T150405")
		}
		jsonl, err := NewJSONLTraceExporter(dir, sessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("create jsonl tracing exporter: %w", err)
		}
		sink = jsonl
	default:
		return nil, nil, &UnsupportedExporterError{Name: exporterName}
	}

	level := cfg.RedactionLevel
	if level == "" {
		level = RedactionLevelRedact
	}
	wrapped := NewRedactingExporter(sink, level)
	exporter := NewAsyncExporter(wrapped, 1024, 16)
	return NewTracer("", exporter), exporter, nil
}
