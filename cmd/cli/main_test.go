package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	// Skip first-run onboarding wizard so the test reaches interactive mode.
	t.Setenv("GO_CLI_NO_ONBOARDING", "1")
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr, func() (*config.Config, error) {
		return disabledCfg(), nil
	})
	assert.Equal(t, 0, code)
	// No subcommand defaults to interactive mode.
	assert.Contains(t, stdout.String(), "Interactive session started")
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

	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
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

	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "a real tracer should export the ended span to the JSONL file")

	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Contains(t, string(data), "cli.invocation")
}

func TestNewTracingFileFailureDoesNotEmitRawPrompt(t *testing.T) {
	const promptCanary = "FAILED-TRACE-FILE-RAW-CANARY"
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	stdoutReader, stdoutWriter, err := os.Pipe()
	require.NoError(t, err)
	previousStdout := os.Stdout
	os.Stdout = stdoutWriter
	defer func() {
		os.Stdout = previousStdout
		_ = stdoutWriter.Close()
		_ = stdoutReader.Close()
	}()

	cfg := enabledCfg(filepath.Join(blocker, "traces"))
	tracer, exporter := newTracing(cfg)
	require.NotNil(t, tracer)
	span, _ := tracer.Start(context.Background(), "prompt.run", tracing.SpanKindInternal)
	span.SetAttributes(tracing.Attribute{Key: "args", Value: []string{promptCanary}})
	span.SetAttributes(tracing.Attribute{Key: "prompt_length", Value: len(promptCanary)})
	span.SetStatus(tracing.SpanStatusOK, "")
	span.End()
	require.NoError(t, exporter.Shutdown(context.Background()))
	require.NoError(t, stdoutWriter.Close())

	data, err := io.ReadAll(stdoutReader)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(data), promptCanary), "prompt canary leaked to stdout fallback")
	assert.True(t, strings.Contains(string(data), "***REDACTED***"), "stdout fallback must retain redaction markers")
	assert.True(t, strings.Contains(string(data), "prompt.run"), "stdout fallback must retain span identity")
	assert.True(t, strings.Contains(string(data), "prompt_length"), "stdout fallback must retain prompt length metadata")
}

// TestDefaultTraceContainsNoPrompt drives a prompt and tool call through the
// production CLI, Assembly, Harness, trace exporters, and slog handler. Prompt
// and tool-argument canaries must not be retained by default trace/log output.
func TestDefaultTraceContainsNoPrompt(t *testing.T) {
	const (
		promptCanary = "PROMPT-RAW-CANARY-42"
		toolCanary   = "TOOL-ARG-SECRET-CANARY-73"
	)

	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		switch requestCount {
		case 1:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"todo_write","arguments":"{\"action\":\"add\",\"content\":\"` + toolCanary + `\"}"}}]}}]}`)) //nolint:errcheck
		case 2:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_2","type":"function","function":{"name":"enter_plan_mode","arguments":"{\"reason\":\"` + promptCanary + `\"}"}}]}}]}`)) //nolint:errcheck
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)) //nolint:errcheck
		}
	}))
	defer srv.Close()

	traceDir := t.TempDir()
	previousDir, err := os.Getwd()
	require.NoError(t, err)
	workDir := t.TempDir()
	require.NoError(t, os.Chdir(workDir))
	defer func() { _ = os.Chdir(previousDir) }()

	previousLogger := slog.Default()
	defer slog.SetDefault(previousLogger)

	cfg := &config.Config{Provider: config.ProviderConfig{
		Name:    "test",
		BaseURL: srv.URL,
		APIKey:  "test",
		Model:   "test-model",
	}}
	cfg.Tracing = config.TracingConfig{
		Enabled:  boolPtr(true),
		Exporter: "jsonl",
		FilePath: traceDir,
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"prompt", "-approve", "auto", promptCanary}, &stdout, &stderr, func() (*config.Config, error) {
		return cfg, nil
	})
	require.Equal(t, 0, code, "stderr: %s", stderr.String())
	require.Equal(t, 3, requestCount, "expected two tool calls and a final response")

	files, err := filepath.Glob(filepath.Join(traceDir, "*.jsonl"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(files), 2, "main CLI and Assembly should both export spans")
	logData, err := os.ReadFile(filepath.Join(workDir, ".go-cli", "logs", "go-cli.log"))
	require.NoError(t, err)
	var trace strings.Builder
	for _, path := range files {
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		trace.Write(data)
	}
	allRecords := trace.String() + string(logData)
	assert.False(t, strings.Contains(allRecords, promptCanary), "prompt canary leaked to trace or log output")
	assert.False(t, strings.Contains(allRecords, toolCanary), "tool argument canary leaked to trace or log output")
	assert.True(t, strings.Contains(string(logData), "core.plan_mode.enter"), "the real plan-mode reason log must remain observable")
	assert.True(t, strings.Contains(allRecords, "prompt.run"), "trace must retain span identity")
	assert.True(t, strings.Contains(allRecords, "command_name"), "trace must retain command identity")
	assert.True(t, strings.Contains(allRecords, "user_message_length"), "trace must retain prompt length metadata")
	assert.True(t, strings.Contains(allRecords, "duration_ms"), "trace must retain timing metadata")
	assert.True(t, strings.Contains(allRecords, "bytes="), "trace must retain redacted byte lengths")
	assert.True(t, strings.Contains(allRecords, "status"), "trace must retain status metadata")

	// The trace file remains JSONL so metadata stays consumable by tooling.
	for _, line := range strings.Split(strings.TrimSpace(trace.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record))
	}
}

func TestDefaultTraceAndLogsRedactProviderErrorBody(t *testing.T) {
	const canary = "PROVIDER-ERROR-PROMPT-RAW-CANARY-91"
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "provider rejected request and echoed: "+canary) //nolint:errcheck // http test body write
	}))
	defer srv.Close()

	traceDir := t.TempDir()
	previousDir, err := os.Getwd()
	require.NoError(t, err)
	workDir := t.TempDir()
	require.NoError(t, os.Chdir(workDir))
	defer func() { _ = os.Chdir(previousDir) }()

	previousLogger := slog.Default()
	defer slog.SetDefault(previousLogger)
	cfg := &config.Config{Provider: config.ProviderConfig{
		Name:    "test",
		BaseURL: srv.URL,
		APIKey:  "test",
		Model:   "test-model",
	}}
	cfg.Tracing = config.TracingConfig{
		Enabled:  boolPtr(true),
		Exporter: "jsonl",
		FilePath: traceDir,
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"prompt", "-approve", "auto", canary}, &stdout, &stderr, func() (*config.Config, error) {
		return cfg, nil
	})
	require.Equal(t, 1, code, "provider rejection must remain a failed CLI invocation")
	require.Equal(t, 1, requestCount)

	files, err := filepath.Glob(filepath.Join(traceDir, "*.jsonl"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(files), 2, "main CLI and Assembly must both export spans")
	logData, err := os.ReadFile(filepath.Join(workDir, ".go-cli", "logs", "go-cli.log"))
	require.NoError(t, err)
	var trace strings.Builder
	var failedStatus bool
	var providerCode bool
	var providerErrorType bool
	for _, path := range files {
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		trace.Write(data)
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var span tracing.SpanData
			require.NoError(t, json.Unmarshal([]byte(line), &span))
			failedStatus = failedStatus || span.Status == tracing.SpanStatusError
			for _, attr := range span.Attributes {
				if attr.Key == "http_status_code" {
					providerCode = providerCode || attr.Value == float64(http.StatusBadRequest)
				}
				if attr.Key == "error_type" {
					providerErrorType = providerErrorType || attr.Value == "server"
				}
			}
		}
	}
	allRecords := trace.String() + string(logData) + stdout.String() + stderr.String()
	if strings.Contains(allRecords, canary) {
		t.Fatal("provider error response canary leaked to default trace or log output")
	}
	assert.True(t, failedStatus, "trace must preserve the provider failure status")
	assert.True(t, providerCode, "trace must preserve the safe provider HTTP status code")
	assert.True(t, providerErrorType, "trace must preserve the safe provider error category")
	if !strings.Contains(allRecords, "loop.run") {
		t.Fatal("trace must retain the failed loop identity")
	}
	if !strings.Contains(allRecords, "llm_stream_start") {
		t.Fatal("log must retain provider operation identity")
	}
	if !strings.Contains(allRecords, "bytes=") {
		t.Fatal("redaction must retain response length metadata")
	}
}

func TestDefaultInteractiveTraceContainsNoPrompt(t *testing.T) {
	const promptCanary = "INTERACTIVE-RAW-CANARY-58"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)) //nolint:errcheck
	}))
	defer srv.Close()

	traceDir := t.TempDir()
	previousDir, err := os.Getwd()
	require.NoError(t, err)
	workDir := t.TempDir()
	require.NoError(t, os.Chdir(workDir))
	defer func() { _ = os.Chdir(previousDir) }()

	stdinReader, stdinWriter, err := os.Pipe()
	require.NoError(t, err)
	previousStdin := os.Stdin
	os.Stdin = stdinReader
	defer func() {
		os.Stdin = previousStdin
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
	}()
	go func() {
		if _, writeErr := stdinWriter.Write([]byte(promptCanary + "\n")); writeErr != nil {
			t.Errorf("write prompt input: %v", writeErr)
			return
		}
		time.Sleep(200 * time.Millisecond)
		if _, writeErr := stdinWriter.Write([]byte("exit\n")); writeErr != nil {
			t.Errorf("write exit input: %v", writeErr)
		}
		_ = stdinWriter.Close()
	}()

	t.Setenv("GO_CLI_NO_ONBOARDING", "1")
	cfg := &config.Config{Provider: config.ProviderConfig{
		Name:    "test",
		BaseURL: srv.URL,
		APIKey:  "test",
		Model:   "test-model",
	}}
	cfg.Tracing = config.TracingConfig{
		Enabled:  boolPtr(true),
		Exporter: "jsonl",
		FilePath: traceDir,
	}
	cfg.Session.StorePath = filepath.Join(workDir, "sessions.jsonl")
	cfg.History.Path = filepath.Join(workDir, "history.jsonl")
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr, func() (*config.Config, error) { return cfg, nil })
	require.Equal(t, 0, code, "stderr: %s", stderr.String())

	files, err := filepath.Glob(filepath.Join(traceDir, "*.jsonl"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	logData, err := os.ReadFile(filepath.Join(workDir, ".go-cli", "logs", "go-cli.log"))
	require.NoError(t, err)
	var trace strings.Builder
	for _, path := range files {
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		trace.Write(data)
	}
	allRecords := trace.String() + string(logData)
	assert.False(t, strings.Contains(allRecords, promptCanary), "interactive prompt canary leaked to trace or log output")
	assert.True(t, strings.Contains(allRecords, "interactive.turn"), "trace must retain turn identity")
	assert.True(t, strings.Contains(allRecords, "user_message_length"), "trace must retain prompt length metadata")
	assert.True(t, strings.Contains(allRecords, "bytes="), "trace must retain redacted byte lengths")
	assert.True(t, strings.Contains(allRecords, "duration_ms"), "trace must retain timing metadata")
}
