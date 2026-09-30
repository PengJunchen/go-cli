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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
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

type synchronizedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

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

func TestRunGlobalHelpAndVersionBypassBrokenConfig(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantOutput string
	}{
		{name: "help", args: []string{"--help"}, wantOutput: "Usage:"},
		{name: "version", args: []string{"--version"}, wantOutput: cli.Version},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var stdout, stderr bytes.Buffer
			var loadCalls int
			code := run(tt.args, &stdout, &stderr, func() (*config.Config, error) {
				loadCalls++
				return nil, errors.New("broken config")
			})

			assert.Zero(t, code, "stdout=%q stderr=%q", stdout.String(), stderr.String())
			assert.Zero(t, loadCalls, "config loader must not run for parser-recognized global help/version")
			assert.Contains(t, stdout.String(), tt.wantOutput)
			assert.Empty(t, stderr.String())
		})
	}
}

func TestRunBrokenConfigStillBlocksNonEarlyArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "version command", args: []string{"version"}},
		{name: "unknown command", args: []string{"unknown-command"}},
		{name: "help disabled", args: []string{"--help=false"}},
		{name: "version disabled", args: []string{"--version=false"}},
		{name: "subcommand help", args: []string{"prompt", "--help"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var stdout, stderr bytes.Buffer
			var loadCalls int
			code := run(tt.args, &stdout, &stderr, func() (*config.Config, error) {
				loadCalls++
				return nil, errors.New("broken config")
			})

			assert.Equal(t, 1, code, "stdout=%q stderr=%q", stdout.String(), stderr.String())
			assert.Equal(t, 1, loadCalls)
			assert.Contains(t, stderr.String(), "broken config")
		})
	}
}

func TestCLIBinaryGlobalHelpAndVersionIgnoreBrokenConfig(t *testing.T) {
	binaryPath := buildCLIBinary(t)

	tests := []struct {
		name       string
		args       []string
		wantExit   int
		wantOutput string
		wantError  string
	}{
		{name: "help", args: []string{"--help"}, wantOutput: "Usage:"},
		{name: "version", args: []string{"--version"}, wantOutput: cli.Version},
		{name: "unknown command still loads config", args: []string{"unknown-command"}, wantExit: 1, wantError: "config:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			homeDir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(workDir, ".go-cli.yaml"), []byte("provider: [\n"), 0o600))
			globalConfigDir := filepath.Join(homeDir, ".go-cli")
			require.NoError(t, os.MkdirAll(globalConfigDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(globalConfigDir, "config.yaml"), []byte("provider: [\n"), 0o600))

			command := exec.Command(binaryPath, tt.args...)
			command.Dir = workDir
			command.Env = environmentWithHome(homeDir)
			output, runErr := command.CombinedOutput()
			if tt.wantExit == 0 {
				require.NoError(t, runErr, "output=%q", output)
			} else {
				var exitErr *exec.ExitError
				require.ErrorAs(t, runErr, &exitErr, "output=%q", output)
				assert.Equal(t, tt.wantExit, exitErr.ExitCode(), "output=%q", output)
				assert.Contains(t, string(output), tt.wantError)
				return
			}
			assert.Contains(t, string(output), tt.wantOutput)
			if tt.name == "help" {
				assert.Contains(t, string(output), "trust", "the trust command must be registered before early root help dispatch")
			}
			assert.NotContains(t, string(output), "Interactive session started")
			_, sessionErr := os.Stat(filepath.Join(workDir, ".go-cli", "sessions"))
			assert.ErrorIs(t, sessionErr, os.ErrNotExist, "help/version must not create a session store")
		})
	}
}

func buildCLIBinary(t *testing.T) string {
	t.Helper()
	workDirForBuild, err := os.Getwd()
	require.NoError(t, err)
	binaryPath := filepath.Join(t.TempDir(), "go-cli")
	build := exec.Command("go", "build", "-o", binaryPath, ".")
	build.Dir = workDirForBuild
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build CLI binary: %v\n%s", buildErr, output)
	}
	return binaryPath
}

func runCLIBinary(t *testing.T, binaryPath, workDir, home string, args ...string) (int, string) {
	t.Helper()
	command := exec.Command(binaryPath, args...)
	command.Dir = workDir
	command.Env = environmentWithHome(home)
	output, runErr := command.CombinedOutput()
	if runErr == nil {
		return 0, string(output)
	}
	var exitErr *exec.ExitError
	require.ErrorAs(t, runErr, &exitErr, "output=%q", output)
	return exitErr.ExitCode(), string(output)
}

func TestCLIBinaryTrustGrantRejectsNonTTYAndListRevokeWorkNonTTY(t *testing.T) {
	binaryPath := buildCLIBinary(t)
	workDir := t.TempDir()
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, ".go-cli.yaml"), []byte("model:\n  name: project\n"), 0o600))
	root, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)
	storePath := filepath.Join(home, ".config", "go-cli", "trust.json")

	code, output := runCLIBinary(t, binaryPath, workDir, home, "trust", "grant")
	require.Equal(t, 1, code, "output=%q", output)
	assert.Contains(t, output, "trust grant requires an interactive terminal")
	_, err = os.Stat(storePath)
	assert.ErrorIs(t, err, os.ErrNotExist, "non-TTY grant must not create a trust entry")

	code, output = runCLIBinary(t, binaryPath, workDir, home, "trust", "list")
	require.Zero(t, code, "output=%q", output)
	assert.Contains(t, output, "No trust entries.")

	code, output = runCLIBinary(t, binaryPath, workDir, home, "trust", "revoke", root)
	require.Zero(t, code, "output=%q", output)
	assert.Contains(t, output, "Revoked trust for "+root)
}

func TestCLIBinaryTrustListFailsClosedOnCorruptOrNonPrivateStore(t *testing.T) {
	binaryPath := buildCLIBinary(t)
	workDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, ".go-cli.yaml"), []byte("model:\n  name: project\n"), 0o600))

	t.Run("corrupt file", func(t *testing.T) {
		home := t.TempDir()
		storeDir := filepath.Join(home, ".config", "go-cli")
		require.NoError(t, os.MkdirAll(storeDir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(storeDir, "trust.json"), []byte("{broken"), 0o600))
		code, output := runCLIBinary(t, binaryPath, workDir, home, "trust", "list")
		require.Equal(t, 1, code, "output=%q", output)
		assert.Contains(t, output, "list trust entries")
		assert.NotContains(t, output, "No trust entries.")
	})

	t.Run("replaceable parent directory", func(t *testing.T) {
		home := t.TempDir()
		storeDir := filepath.Join(home, ".config", "go-cli")
		require.NoError(t, os.MkdirAll(storeDir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(storeDir, "trust.json"), []byte("{}"), 0o600))
		require.NoError(t, os.Chmod(storeDir, 0o755))
		code, output := runCLIBinary(t, binaryPath, workDir, home, "trust", "list")
		require.Equal(t, 1, code, "output=%q", output)
		assert.Contains(t, output, "list trust entries")
		assert.NotContains(t, output, "No trust entries.")
	})
}

func environmentWithHome(home string) []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, "HOME=") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, "HOME="+home)
}

func TestRunSuccessNoArgs(t *testing.T) {
	// Skip first-run onboarding wizard so the test reaches interactive mode.
	t.Setenv("GO_CLI_NO_ONBOARDING", "1")
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	var requests atomic.Int64
	// A local scoped credential lets production assembly pass strict auth while
	// keeping this bare-argv startup test independent of real user config/network.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(server.Close)
	cfg := disabledCfg()
	cfg.Provider = config.ProviderConfig{
		Name:    "openai",
		APIKey:  "test-local-key",
		BaseURL: server.URL,
		Model:   "test-model",
	}
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr, func() (*config.Config, error) {
		return cfg, nil
	})
	assert.Equal(t, 0, code, "stdout=%q stderr=%q", stdout.String(), stderr.String())
	// No subcommand defaults to interactive mode.
	assert.Contains(t, stdout.String(), "Interactive session started")
	assert.Empty(t, stderr.String())
	assert.Zero(t, requests.Load(), "starting an empty interactive session must not call the model")
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
	tr, exp, err := newTracing(&config.Config{Tracing: config.TracingConfig{Enabled: boolPtr(false), Exporter: "jsonl"}}, io.Discard)
	require.NoError(t, err)
	assert.Nil(t, tr)
	assert.Nil(t, exp)
}

func TestNewTracingNoneReturnsNil(t *testing.T) {
	tr, exp, err := newTracing(&config.Config{Tracing: config.TracingConfig{Enabled: boolPtr(true), Exporter: "none"}}, io.Discard)
	require.NoError(t, err)
	assert.Nil(t, tr)
	assert.Nil(t, exp)
}

func TestNewTracingUnsupportedExporterReturnsTypedError(t *testing.T) {
	for _, name := range []string{"otlp", "kafka", "bogus"} {
		t.Run(name, func(t *testing.T) {
			tr, exp, err := newTracing(&config.Config{Tracing: config.TracingConfig{Enabled: boolPtr(true), Exporter: name}}, io.Discard)
			require.Error(t, err)
			assert.Nil(t, tr)
			assert.Nil(t, exp)
			var unsupported *tracing.UnsupportedExporterError
			assert.ErrorAs(t, err, &unsupported)
			assert.ErrorIs(t, err, tracing.ErrUnsupportedExporter)
			assert.Equal(t, name, unsupported.Name)
		})
	}
}

func TestRunDisabledAndNoneTracingCreateNoTraceArtifacts(t *testing.T) {
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
			workDir := t.TempDir()
			previousDir, err := os.Getwd()
			require.NoError(t, err)
			require.NoError(t, os.Chdir(workDir))
			t.Cleanup(func() { _ = os.Chdir(previousDir) })

			traceDir := filepath.Join(workDir, "configured-traces")
			cfg := &config.Config{Tracing: config.TracingConfig{
				Enabled:  boolPtr(tt.enabled),
				Exporter: tt.exporter,
				FilePath: traceDir,
			}}
			var stdout, stderr bytes.Buffer
			code := run([]string{"version"}, &stdout, &stderr, func() (*config.Config, error) { return cfg, nil })
			assert.Equal(t, 0, code)
			assert.Contains(t, stdout.String(), cli.Version)
			assert.NotContains(t, stdout.String(), "[TRACE]")
			assert.Empty(t, stderr.String())
			_, err = os.Stat(traceDir)
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestRunStdoutTracingUsesCLIStdoutWriter(t *testing.T) {
	workDir := t.TempDir()
	previousDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(previousDir) })

	var stdout synchronizedBuffer
	var stderr bytes.Buffer
	cfg := &config.Config{Tracing: config.TracingConfig{
		Enabled:  boolPtr(true),
		Exporter: "stdout",
	}}
	code := run([]string{"version"}, &stdout, &stderr, func() (*config.Config, error) { return cfg, nil })
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout.String(), cli.Version)
	assert.Contains(t, stdout.String(), "[TRACE]")
	assert.Empty(t, stderr.String())
	_, err = os.Stat(filepath.Join(workDir, ".go-cli", "traces"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestRunRejectsStdoutTracingForStructuredPromptOutputBeforeRuntime(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantError string
	}{
		{name: "long separate json", args: []string{"prompt", "--output", "json", "hello"}, wantError: "prompt --output json"},
		{name: "long joined json", args: []string{"prompt", "--output=json", "hello"}, wantError: "prompt --output json"},
		{name: "short separate stream", args: []string{"prompt", "-output", "stream", "hello"}, wantError: "prompt --output stream"},
		{name: "short joined stream", args: []string{"prompt", "-output=stream", "hello"}, wantError: "prompt --output stream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			t.Chdir(workDir)

			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			}))
			t.Cleanup(server.Close)

			traceDir := filepath.Join(workDir, "configured-traces")
			cfg := &config.Config{
				Provider: config.ProviderConfig{
					Name: "test", BaseURL: server.URL, APIKey: "test-key", Model: "test-model",
				},
				Tracing: config.TracingConfig{Enabled: boolPtr(true), Exporter: "stdout", FilePath: traceDir},
			}
			var stdout synchronizedBuffer
			var stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr, func() (*config.Config, error) { return cfg, nil })

			assert.Equal(t, 2, code)
			assert.Empty(t, stdout.String(), "the structured-output stream must stay empty")
			assert.Contains(t, stderr.String(), "stdout tracing")
			assert.Contains(t, stderr.String(), tt.wantError)
			assert.Zero(t, requests.Load(), "preflight must run before model assembly or HTTP")
			_, err := os.Stat(traceDir)
			assert.ErrorIs(t, err, os.ErrNotExist)
			_, err = os.Stat(filepath.Join(workDir, ".go-cli", "traces"))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestStdoutTracingOutputConflictIsTypedAndHonorsDefaultEnabled(t *testing.T) {
	for _, tt := range []struct {
		mode string
		want cli.OutputMode
	}{
		{mode: "json", want: cli.OutputJSON},
		{mode: "stream", want: cli.OutputStream},
	} {
		err := stdoutTracingOutputConflict(&config.Config{Tracing: config.TracingConfig{Exporter: "stdout"}}, []string{"prompt", "--output=" + tt.mode, "hello"})
		var conflict *cli.StdoutTracingOutputConflictError
		require.ErrorAs(t, err, &conflict)
		assert.Equal(t, tt.want, conflict.Mode)
	}
}

func TestRunStdoutTracingStructuredOutputPreflightPreservesOtherModes(t *testing.T) {
	tests := []struct {
		name      string
		enabled   bool
		exporter  string
		args      []string
		wantTrace bool
	}{
		{name: "disabled json", enabled: false, exporter: "stdout", args: []string{"prompt", "--output=json", "hello"}},
		{name: "none json", enabled: true, exporter: "none", args: []string{"prompt", "--output=json", "hello"}},
		{name: "stdout text", enabled: true, exporter: "stdout", args: []string{"prompt", "hello"}, wantTrace: true},
		{name: "stdout positional then output flag", enabled: true, exporter: "stdout", args: []string{"prompt", "hello", "--output=json"}, wantTrace: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			t.Chdir(workDir)

			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			}))
			t.Cleanup(server.Close)

			cfg := &config.Config{
				Provider: config.ProviderConfig{
					Name: "test", BaseURL: server.URL, APIKey: "test-key", Model: "test-model",
				},
				Tracing: config.TracingConfig{Enabled: boolPtr(tt.enabled), Exporter: tt.exporter},
			}
			var stdout synchronizedBuffer
			var stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr, func() (*config.Config, error) { return cfg, nil })

			assert.Equal(t, 0, code, "stdout=%q stderr=%q", stdout.String(), stderr.String())
			assert.Empty(t, stderr.String())
			assert.EqualValues(t, 1, requests.Load(), "a bypassed mode should execute the prompt")
			if tt.wantTrace {
				assert.Contains(t, stdout.String(), "[TRACE]")
			} else {
				assert.NotContains(t, stdout.String(), "[TRACE]")
			}
			assert.Contains(t, stdout.String(), "ok")
		})
	}
}

func TestRunStdoutTracingKeepsPromptUsageAndGlobalHelpBehavior(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantError  string
		wantOutput string
		wantTrace  bool
	}{
		{name: "prompt help remains usage error", args: []string{"prompt", "--help", "hello"}, wantCode: 2, wantError: "help requested", wantTrace: true},
		{name: "invalid output remains usage error", args: []string{"prompt", "--output=invalid", "hello"}, wantCode: 2, wantError: "invalid output mode", wantTrace: true},
		{name: "missing message remains usage error", args: []string{"prompt", "--output=json"}, wantCode: 2, wantError: "missing message argument", wantTrace: true},
		{name: "global help wins before dispatch", args: []string{"--help", "prompt", "--output=json", "hello"}, wantCode: 0, wantOutput: "Usage:"},
		{name: "global version wins before dispatch", args: []string{"--version", "prompt", "--output=json", "hello"}, wantCode: 0, wantOutput: cli.Version},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			t.Chdir(workDir)
			cfg := &config.Config{Tracing: config.TracingConfig{Enabled: boolPtr(true), Exporter: "stdout"}}
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr, func() (*config.Config, error) { return cfg, nil })

			assert.Equal(t, tt.wantCode, code)
			if tt.wantTrace {
				assert.Contains(t, stdout.String(), "[TRACE]")
			} else {
				assert.NotContains(t, stdout.String(), "[TRACE]")
			}
			if tt.wantError != "" {
				assert.Contains(t, stderr.String(), tt.wantError)
				assert.NotContains(t, stderr.String(), "stdout tracing")
			} else {
				assert.Empty(t, stderr.String())
				assert.Contains(t, stdout.String(), tt.wantOutput)
			}
		})
	}
}

func TestRunUnsupportedTracingExporterFailsBeforeCommand(t *testing.T) {
	for _, name := range []string{"otlp", "kafka", "bogus"} {
		t.Run(name, func(t *testing.T) {
			workDir := t.TempDir()
			previousDir, err := os.Getwd()
			require.NoError(t, err)
			require.NoError(t, os.Chdir(workDir))
			t.Cleanup(func() { _ = os.Chdir(previousDir) })

			traceDir := filepath.Join(workDir, "traces")
			cfg := &config.Config{Tracing: config.TracingConfig{
				Enabled:  boolPtr(true),
				Exporter: name,
				FilePath: traceDir,
			}}
			var stdout, stderr bytes.Buffer
			code := run([]string{"version"}, &stdout, &stderr, func() (*config.Config, error) { return cfg, nil })
			assert.Equal(t, 1, code)
			assert.Contains(t, stderr.String(), "unsupported tracing exporter")
			assert.NotContains(t, stdout.String(), "[TRACE]")
			_, err = os.Stat(traceDir)
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
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
	require.Len(t, files, 1, "the invocation and command should share one JSONL owner")

	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	var spans []tracing.SpanData
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var span tracing.SpanData
		require.NoError(t, json.Unmarshal([]byte(line), &span))
		spans = append(spans, span)
	}
	require.Len(t, spans, 2)
	assert.ElementsMatch(t, []string{"cli.invocation", "command.dispatch"}, []string{spans[0].Name, spans[1].Name})
	assert.Equal(t, spans[0].TraceID, spans[1].TraceID)
}

func TestRunConsecutiveInvocationsUseDistinctTraceFiles(t *testing.T) {
	workDir := t.TempDir()
	previousDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workDir))
	t.Cleanup(func() { _ = os.Chdir(previousDir) })
	dir := filepath.Join(workDir, "traces")
	for i := 0; i < 2; i++ {
		var stdout, stderr bytes.Buffer
		code := run([]string{"version"}, &stdout, &stderr, func() (*config.Config, error) {
			return enabledCfg(dir), nil
		})
		require.Equal(t, 0, code, "invocation %d stderr: %s", i, stderr.String())
	}

	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 2, "each invocation must own a distinct JSONL file")

	seenTraceIDs := make(map[string]struct{}, 2)
	for _, path := range files {
		data, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		var fileTraceIDs = make(map[string]struct{})
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var span tracing.SpanData
			require.NoError(t, json.Unmarshal([]byte(line), &span))
			fileTraceIDs[span.TraceID] = struct{}{}
		}
		require.Len(t, fileTraceIDs, 1, "one invocation file must not mix trace identities")
		for traceID := range fileTraceIDs {
			_, alreadySeen := seenTraceIDs[traceID]
			assert.False(t, alreadySeen, "separate invocation files must have distinct trace IDs")
			seenTraceIDs[traceID] = struct{}{}
		}
	}
	require.Len(t, seenTraceIDs, 2)
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
	require.Len(t, files, 1)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Contains(t, string(data), "cli.invocation")
	assert.Contains(t, string(data), "ERROR")
}

func TestPromptUsesFullPositionalTextAndFailureSpan(t *testing.T) {
	requestBodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read model request: %v", err)
			http.Error(w, "unable to read request", http.StatusInternalServerError)
			return
		}
		requestBodies <- body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"rejected test prompt","type":"invalid_request_error","code":"invalid_request"}}`)
	}))
	defer srv.Close()

	traceDir := t.TempDir()
	workDir := t.TempDir()
	t.Chdir(workDir)
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
	code := run([]string{"prompt", "-approve", "auto", "hello", "world"}, &stdout, &stderr, func() (*config.Config, error) {
		return cfg, nil
	})
	require.Equal(t, 1, code, "a provider rejection must remain a failed invocation: %s", stderr.String())

	var requestBody []byte
	select {
	case requestBody = <-requestBodies:
	case <-time.After(5 * time.Second):
		t.Fatal("CLI did not send a model request")
	}
	var modelRequest struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(requestBody, &modelRequest))
	var userPrompt string
	for _, message := range modelRequest.Messages {
		if message.Role == "user" {
			userPrompt = message.Content
		}
	}
	assert.Equal(t, "hello world", userPrompt, "all positional prompt words must reach the model")

	files, err := filepath.Glob(filepath.Join(traceDir, "*.jsonl"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	spansByName := make(map[string]tracing.SpanData)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var span tracing.SpanData
		require.NoError(t, json.Unmarshal([]byte(line), &span))
		spansByName[span.Name] = span
	}
	for _, spanName := range []string{"cli.invocation", "command.dispatch", "prompt.run"} {
		span, ok := spansByName[spanName]
		require.True(t, ok, "trace must contain %s", spanName)
		assert.Equal(t, tracing.SpanStatusError, span.Status, "%s must preserve the provider failure", spanName)
	}
	var promptLength any
	for _, attribute := range spansByName["prompt.run"].Attributes {
		if attribute.Key == "prompt_length" {
			promptLength = attribute.Value
		}
	}
	assert.Equal(t, float64(len(userPrompt)), promptLength, "prompt span length must describe the complete positional text")
}

type finalizerGateExporter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	err     error
}

func (*finalizerGateExporter) ExportSpan(context.Context, tracing.TraceSpan) error { return nil }

func (e *finalizerGateExporter) Shutdown(ctx context.Context) error {
	e.once.Do(func() { close(e.started) })
	select {
	case <-e.release:
		return e.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type finalizerReportExporter struct {
	report tracing.AsyncExporterReport
	err    error
}

func (*finalizerReportExporter) ExportSpan(context.Context, tracing.TraceSpan) error { return nil }

func (e *finalizerReportExporter) Shutdown(context.Context) error { return e.err }

func (e *finalizerReportExporter) ShutdownWithReport(context.Context) (tracing.AsyncExporterReport, error) {
	return e.report, e.err
}

func TestTracingFinalizerReportsAllCountersForCompletedLossyShutdown(t *testing.T) {
	// This finalizer-visible rejection models a production late producer; low-level rejection probes test that counter independently.
	exporter := &finalizerReportExporter{
		report: tracing.AsyncExporterReport{
			Stats: tracing.AsyncExporterStats{
				Accepted:            7,
				Delivered:           3,
				QueueFullDropped:    1,
				SinkFailed:          2,
				RejectedAfterClose:  1,
				AbandonedOnShutdown: 1,
				Pending:             1,
			},
			OwnerFinished: true,
		},
	}
	var stderr bytes.Buffer

	code := finalizeTracingExporter(context.Background(), exporter, &stderr, time.Second)

	assert.Equal(t, 1, code, "delivery anomalies in a completed report must affect command status")
	assert.Equal(t,
		"error: tracing exporter shutdown completed with delivery issues (accepted=7 delivered=3 queue_full_dropped=1 sink_failed=2 rejected_after_close=1 abandoned_on_shutdown=1 pending=1)\n",
		stderr.String(),
		"the bounded report must include every counter without exporter payloads",
	)
}

func TestTracingFinalizerFailureReportIncludesAllCountersWithoutCallbackText(t *testing.T) {
	canary := "private exporter callback detail"
	exporter := &finalizerReportExporter{
		report: tracing.AsyncExporterReport{
			Stats: tracing.AsyncExporterStats{
				Accepted:            7,
				Delivered:           3,
				QueueFullDropped:    1,
				SinkFailed:          2,
				RejectedAfterClose:  1,
				AbandonedOnShutdown: 1,
				Pending:             1,
			},
			OwnerFinished: true,
		},
		err: errors.New(canary),
	}
	var stderr bytes.Buffer

	code := finalizeTracingExporter(context.Background(), exporter, &stderr, time.Second)

	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "tracing exporter shutdown failed")
	assert.Contains(t, stderr.String(), "queue_full_dropped=1")
	assert.Contains(t, stderr.String(), "sink_failed=2")
	assert.Contains(t, stderr.String(), "rejected_after_close=1")
	assert.Contains(t, stderr.String(), "abandoned_on_shutdown=1")
	assert.Contains(t, stderr.String(), "pending=1")
	assert.NotContains(t, stderr.String(), canary, "stderr must not expose arbitrary callback text")
}

func TestTracingFinalizerWaitsDespiteCanceledSignalContext(t *testing.T) {
	sourceCtx, cancel := context.WithCancel(context.Background())
	cancel()
	inner := &finalizerGateExporter{started: make(chan struct{}), release: make(chan struct{})}
	exporter := tracing.NewAsyncExporter(inner, 8, 1)
	var stderr bytes.Buffer
	result := make(chan int, 1)
	go func() { result <- finalizeTracingExporter(sourceCtx, exporter, &stderr, time.Second) }()

	select {
	case <-inner.started:
	case <-time.After(time.Second):
		close(inner.release)
		t.Fatal("inner shutdown callback did not start")
	}
	select {
	case code := <-result:
		close(inner.release)
		t.Fatalf("finalizer returned before the owner completed, code=%d", code)
	case <-time.After(30 * time.Millisecond):
	}

	close(inner.release)
	select {
	case code := <-result:
		assert.Zero(t, code)
	case <-time.After(time.Second):
		t.Fatal("finalizer did not observe the completed owner")
	}
	assert.Empty(t, stderr.String())
}

func TestTracingFinalizerDeadlineReturnsFailureWithoutCallbackText(t *testing.T) {
	canary := "private exporter callback detail"
	inner := &finalizerGateExporter{
		started: make(chan struct{}), release: make(chan struct{}), err: errors.New(canary),
	}
	exporter := tracing.NewAsyncExporter(inner, 8, 1)
	var stderr bytes.Buffer
	startedAt := time.Now()
	code := finalizeTracingExporter(context.Background(), exporter, &stderr, 30*time.Millisecond)
	assert.Equal(t, 1, code, "pending shutdown must affect the process exit code")
	assert.Less(t, time.Since(startedAt), time.Second, "finalizer must remain bounded")
	assert.Contains(t, stderr.String(), "pending")
	assert.NotContains(t, stderr.String(), canary, "stderr must not expose arbitrary callback text")

	close(inner.release)
	assert.Error(t, exporter.Shutdown(context.Background()), "the late callback error remains available to the waiter")
}

func TestTracingFinalizerDoesNotPrintCallbackErrorText(t *testing.T) {
	canary := "private callback error canary"
	inner := &finalizerGateExporter{
		started: make(chan struct{}), release: make(chan struct{}), err: errors.New(canary),
	}
	close(inner.release)
	exporter := tracing.NewAsyncExporter(inner, 8, 1)
	var stderr bytes.Buffer
	code := finalizeTracingExporter(context.Background(), exporter, &stderr, time.Second)
	assert.Equal(t, 1, code)
	assert.Contains(t, stderr.String(), "shutdown failed")
	assert.NotContains(t, stderr.String(), canary)
}

// TestNewTracingSpanNonNoop directly verifies that a tracer produced by
// newTracing emits a real exported span (not a noopSpan).
func TestNewTracingSpanNonNoop(t *testing.T) {
	dir := t.TempDir()
	tr, exp, err := newTracing(enabledCfg(dir), io.Discard)
	require.NoError(t, err)
	require.NotNil(t, tr)

	span, ctx := tr.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)
	span.SetStatus(tracing.SpanStatusOK, "")
	span.End()
	require.NoError(t, exp.Shutdown(ctx))

	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "a real tracer should export the ended span to the JSONL file")

	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Contains(t, string(data), "cli.invocation")
}

func TestNewTracingFileFailureDoesNotFallbackToStdout(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	var stdout bytes.Buffer
	cfg := enabledCfg(filepath.Join(blocker, "traces"))
	tracer, exporter, err := newTracing(cfg, &stdout)
	require.Error(t, err)
	assert.Nil(t, tracer)
	assert.Nil(t, exporter)
	assert.Empty(t, stdout.String())
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
	require.Len(t, files, 1, "the CLI root and Agent assembly must share one exporter")
	logData, err := os.ReadFile(filepath.Join(workDir, ".go-cli", "logs", "go-cli.log"))
	require.NoError(t, err)
	var trace strings.Builder
	traceIDs := make(map[string]struct{})
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
			traceIDs[span.TraceID] = struct{}{}
		}
	}
	require.Len(t, traceIDs, 1, "all CLI and assembly spans must use one trace identity")
	allRecords := trace.String() + string(logData)
	assert.False(t, strings.Contains(allRecords, promptCanary), "prompt canary leaked to trace or log output")
	assert.False(t, strings.Contains(allRecords, toolCanary), "tool argument canary leaked to trace or log output")
	assert.NotContains(t, string(logData), "core.plan_mode.enter", "a model-requested plan-mode transition must not execute")
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

func TestUserPlanModeReasonLogKeepsEventWithoutRawReason(t *testing.T) {
	const reasonCanary = "PRIVATE-PLAN-REASON-CANARY-42"
	var logData bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(tracing.NewRedactingSlogHandler(slog.NewTextHandler(&logData, nil))))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	require.NoError(t, core.NewDefaultPlanModeController().Enter(context.Background(), reasonCanary))
	assert.Contains(t, logData.String(), "core.plan_mode.enter")
	assert.NotContains(t, logData.String(), reasonCanary)
	assert.Contains(t, logData.String(), "bytes=")
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
	require.Len(t, files, 1, "the CLI root and Agent assembly must share one exporter")
	logData, err := os.ReadFile(filepath.Join(workDir, ".go-cli", "logs", "go-cli.log"))
	require.NoError(t, err)
	var trace strings.Builder
	var failedStatus bool
	var providerCode bool
	var providerErrorType bool
	traceIDs := make(map[string]struct{})
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
			traceIDs[span.TraceID] = struct{}{}
			failedStatus = failedStatus || span.Status == tracing.SpanStatusError
			for _, attr := range span.Attributes {
				if attr.Key == "http_status_code" {
					providerCode = providerCode || attr.Value == float64(http.StatusBadRequest)
				}
				if attr.Key == "error_type" {
					providerErrorType = providerErrorType || attr.Value == "invalid_request"
				}
			}
		}
	}
	require.Len(t, traceIDs, 1, "all failed command spans must use one trace identity")
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
	cfg.Session.StorePath = filepath.Join(workDir, "sessions")
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
