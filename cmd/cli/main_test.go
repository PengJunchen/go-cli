package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func disabledCfg() *config.Config {
	return &config.Config{
		Tracing: config.TracingConfig{Exporter: "none"},
	}
}

func enabledCfg(filePath string) *config.Config {
	return &config.Config{
		Tracing: config.TracingConfig{
			Enabled:  boolPtr(true),
			Exporter: "jsonl",
			FilePath: filePath,
		},
	}
}

func boolPtr(b bool) *bool { return &b }

func TestRunSuccessVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr, func() (*config.Config, error) {
		return disabledCfg(), nil
	})
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout.String(), "go-cli")
	assert.Contains(t, stdout.String(), cli.Version)
}

func TestRunSuccessHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--help"}, &stdout, &stderr, func() (*config.Config, error) {
		return disabledCfg(), nil
	})
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout.String(), "Usage:")
}

func TestRunSuccessNoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr, func() (*config.Config, error) {
		return disabledCfg(), nil
	})
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout.String(), "Usage:")
}

func TestRunUsageErrorExitCode2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"definitely-not-a-command"}, &stdout, &stderr, func() (*config.Config, error) {
		return disabledCfg(), nil
	})
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr.String(), "error:")
}

func TestRunConfigErrorExitCode1(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr, func() (*config.Config, error) {
		return nil, errors.New("boom")
	})
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "boom")
}

func TestRunFlagParseErrorExitCode2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--bogus-flag"}, &stdout, &stderr, func() (*config.Config, error) {
		return disabledCfg(), nil
	})
	assert.Equal(t, 2, code)
}

func TestNewTracingDisabledReturnsNil(t *testing.T) {
	tr, exp := newTracing(&config.Config{Tracing: config.TracingConfig{Enabled: boolPtr(false), Exporter: "jsonl"}})
	assert.Nil(t, tr)
	assert.Nil(t, exp)
}

func TestNewTracingNoneReturnsNil(t *testing.T) {
	tr, exp := newTracing(&config.Config{Tracing: config.TracingConfig{Enabled: boolPtr(true), Exporter: "none"}})
	assert.Nil(t, tr)
	assert.Nil(t, exp)
}

func TestNewTracingUnknownDefaultNoCrash(t *testing.T) {
	tr, exp := newTracing(&config.Config{Tracing: config.TracingConfig{Enabled: boolPtr(true), Exporter: "bogus"}})
	assert.NotNil(t, tr)
	assert.NotNil(t, exp)
	// Shutdown the wrapping AsyncExporter so its worker goroutine does not leak.
	require.NoError(t, exp.Shutdown(context.Background()))
}

// TestRunEmitsRealSpans proves the production path produces real (non-noop)
// spans when tracing is enabled: cli.invocation + command.dispatch are
// exported to the JSONL trace file.
func TestRunEmitsRealSpans(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"version"}, &stdout, &stderr, func() (*config.Config, error) {
		return enabledCfg(dir), nil
	})
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout.String(), "go-cli")

	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "expected a JSONL trace file")

	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, "cli.invocation")
	assert.Contains(t, content, "command.dispatch")
}

// TestRunEmitsErrorSpanStatus verifies the root span reports an error status
// when the command exits non-zero (usage error path).
func TestRunEmitsErrorSpanStatus(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"not-a-real-command"}, &stdout, &stderr, func() (*config.Config, error) {
		return enabledCfg(dir), nil
	})
	assert.Equal(t, 2, code)

	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.NotEmpty(t, files)
	data, _ := os.ReadFile(files[0])
	assert.Contains(t, string(data), "cli.invocation")
	assert.Contains(t, string(data), "ERROR")
}

// TestNewTracingSpanNonNoop directly verifies that a tracer produced by
// newTracing emits a real exported span (not a noopSpan).
func TestNewTracingSpanNonNoop(t *testing.T) {
	dir := t.TempDir()
	tr, exp := newTracing(enabledCfg(dir))
	require.NotNil(t, tr)

	span, ctx := tr.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)
	span.SetStatus(tracing.SpanStatusOK, "")
	span.End()
	// Allow the span's asynchronous export goroutine to enqueue before Shutdown
	// drains the AsyncExporter (mirrors run()'s flush-on-exit behavior).
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, exp.Shutdown(ctx))

	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.NotEmpty(t, files, "a real tracer should export the ended span to the JSONL file")

	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Contains(t, string(data), "cli.invocation")
}