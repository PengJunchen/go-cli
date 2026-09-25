package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

type assemblyTraceExporter struct {
	mu        sync.Mutex
	spans     []tracing.SpanData
	shutdowns atomic.Int32
}

func (e *assemblyTraceExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, tracing.SpanToData(span))
	return nil
}

func (e *assemblyTraceExporter) Shutdown(context.Context) error {
	e.shutdowns.Add(1)
	return nil
}

func (e *assemblyTraceExporter) exportedSpans() []tracing.SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]tracing.SpanData(nil), e.spans...)
}

func TestAssembleAgentWithoutInvocationTracerIsNoop(t *testing.T) {
	traceDir := filepath.Join(t.TempDir(), "traces")
	cfg := newAssembleTestConfig()
	cfg.Tracing = config.TracingConfig{
		Enabled:  boolPointer(true),
		Exporter: "jsonl",
		FilePath: traceDir,
	}

	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard)
	require.NoError(t, err)
	assert.Nil(t, assembly.Tracer, "standalone assembly must not create a second tracer owner")
	assembly.Cleanup()

	_, err = os.Stat(traceDir)
	assert.ErrorIs(t, err, os.ErrNotExist, "standalone assembly must not create an implicit trace file")
}

func TestAssembleAgentReusesInvocationTracerAndDoesNotShutdownIt(t *testing.T) {
	traceDir := filepath.Join(t.TempDir(), "traces")
	cfg := newAssembleTestConfig()
	cfg.Tracing = config.TracingConfig{
		Enabled:  boolPointer(true),
		Exporter: "jsonl",
		FilePath: traceDir,
	}
	traceExporter := &assemblyTraceExporter{}
	tracer := tracing.NewTracer("invocation-trace", traceExporter)
	rootSpan, rootCtx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	assembly, err := AssembleAgent(rootCtx, cfg, "openai", "test-model", io.Discard)
	require.NoError(t, err)
	require.Same(t, tracer, assembly.Tracer)
	assert.Equal(t, "invocation-trace", assembly.Tracer.TraceID())

	spans := traceExporter.exportedSpans()
	var assemblySpan *tracing.SpanData
	for i := range spans {
		if spans[i].Name == "assemble.agent" {
			assemblySpan = &spans[i]
			break
		}
	}
	require.NotNil(t, assemblySpan)
	assert.Equal(t, rootSpan.TraceID(), assemblySpan.TraceID)
	assert.Equal(t, rootSpan.SpanID(), assemblySpan.ParentSpanID)

	assembly.Cleanup()
	assert.EqualValues(t, 0, traceExporter.shutdowns.Load(), "assembly cleanup must leave the root exporter open")
	rootSpan.End()
	require.NoError(t, traceExporter.Shutdown(context.Background()))
	assert.EqualValues(t, 1, traceExporter.shutdowns.Load(), "the invocation owner closes its exporter once")

	_, err = os.Stat(traceDir)
	assert.ErrorIs(t, err, os.ErrNotExist, "assembly reuse must not create another trace file")
}

func boolPointer(value bool) *bool { return &value }
