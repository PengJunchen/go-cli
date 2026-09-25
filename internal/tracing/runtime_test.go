package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSupportedExporterNames(t *testing.T) {
	tests := []struct {
		name      string
		supported bool
	}{
		{name: "", supported: true},
		{name: "jsonl", supported: true},
		{name: "stdout", supported: true},
		{name: "none", supported: true},
		{name: "otlp", supported: false},
		{name: "kafka", supported: false},
		{name: "bogus", supported: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.supported, IsSupportedExporter(tt.name))
		})
	}
}

func TestBuildTracingRuntimeDisabledAndNoneCreateNoArtifacts(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		exporter string
	}{
		{name: "disabled", enabled: false, exporter: "jsonl"},
		{name: "none", enabled: true, exporter: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "traces")
			var stdout bytes.Buffer
			tracer, exporter, err := BuildTracingRuntime(TracingRuntimeConfig{
				Enabled:  tt.enabled,
				Exporter: tt.exporter,
				FilePath: dir,
				Stdout:   &stdout,
			})
			require.NoError(t, err)
			assert.Nil(t, tracer)
			assert.Nil(t, exporter)
			assert.Empty(t, stdout.String())
			_, err = os.Stat(dir)
			assert.ErrorIs(t, err, os.ErrNotExist, "disabled tracing must not create a trace directory")
		})
	}
}

func TestBuildTracingRuntimeJSONLUsesSingleTraceIdentity(t *testing.T) {
	dir := t.TempDir()
	tracer, exporter, err := BuildTracingRuntime(TracingRuntimeConfig{
		Enabled:   true,
		Exporter:  "jsonl",
		FilePath:  dir,
		SessionID: "one-owner",
	})
	require.NoError(t, err)
	require.NotNil(t, tracer)
	require.NotNil(t, exporter)

	root, rootCtx := tracer.Start(context.Background(), "cli.invocation", SpanKindInternal)
	child, _ := tracer.Start(rootCtx, "command.dispatch", SpanKindInternal)
	child.End()
	root.End()
	require.NoError(t, exporter.Shutdown(context.Background()))

	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	var spans []SpanData
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var span SpanData
		require.NoError(t, json.Unmarshal([]byte(line), &span))
		spans = append(spans, span)
	}
	require.Len(t, spans, 2)
	for _, span := range spans {
		assert.Equal(t, tracer.TraceID(), span.TraceID)
	}
}

func TestBuildTracingRuntimeStdoutUsesConfiguredWriter(t *testing.T) {
	var stdout bytes.Buffer
	dir := filepath.Join(t.TempDir(), "unused")
	tracer, exporter, err := BuildTracingRuntime(TracingRuntimeConfig{
		Enabled:  true,
		Exporter: "stdout",
		FilePath: dir,
		Stdout:   &stdout,
	})
	require.NoError(t, err)
	require.NotNil(t, tracer)
	require.NotNil(t, exporter)
	span, _ := tracer.Start(context.Background(), "command.dispatch", SpanKindInternal)
	span.End()
	require.NoError(t, exporter.Shutdown(context.Background()))
	assert.Contains(t, stdout.String(), "[TRACE]")
	assert.Contains(t, stdout.String(), "command.dispatch")
	_, err = os.Stat(dir)
	assert.ErrorIs(t, err, os.ErrNotExist, "stdout tracing must not create a trace directory")
}

func TestBuildTracingRuntimeRejectsUnsupportedExportersBeforeSideEffects(t *testing.T) {
	for _, name := range []string{"otlp", "kafka", "bogus"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "traces")
			var stdout bytes.Buffer
			tracer, exporter, err := BuildTracingRuntime(TracingRuntimeConfig{
				Enabled:  true,
				Exporter: name,
				FilePath: dir,
				Stdout:   &stdout,
			})
			require.Error(t, err)
			assert.Nil(t, tracer)
			assert.Nil(t, exporter)
			var unsupported *UnsupportedExporterError
			assert.ErrorAs(t, err, &unsupported)
			assert.ErrorIs(t, err, ErrUnsupportedExporter)
			assert.Equal(t, name, unsupported.Name)
			assert.Empty(t, stdout.String())
			_, err = os.Stat(dir)
			assert.ErrorIs(t, err, os.ErrNotExist, "unsupported exporter must not create trace artifacts")
		})
	}
}

func TestBuildTracingRuntimeJSONLFailureDoesNotFallbackToStdout(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	var stdout bytes.Buffer
	tracer, exporter, err := BuildTracingRuntime(TracingRuntimeConfig{
		Enabled:  true,
		Exporter: "jsonl",
		FilePath: filepath.Join(blocker, "traces"),
		Stdout:   &stdout,
	})
	require.Error(t, err)
	assert.Nil(t, tracer)
	assert.Nil(t, exporter)
	assert.False(t, errors.Is(err, ErrUnsupportedExporter))
	assert.Empty(t, stdout.String())
}
