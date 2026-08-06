package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
)

type mockConfig struct {
	verbose bool
}

func (m *mockConfig) Verbose() bool { return m.verbose }

func run(ctx context.Context, cfg Config, args ...string) (string, error) {
	var out bytes.Buffer
	err := Run(ctx, cfg, args, &out)
	return out.String(), err
}

func TestRun_VersionFlag(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	out, err := run(t.Context(), cfg, "-version")
	require.NoError(t, err)
	assert.Contains(t, out, "go-cli ")
	assert.Contains(t, out, Version)
}

func TestRun_VersionCommand(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	out, err := run(t.Context(), cfg, "version")
	require.NoError(t, err)
	assert.Equal(t, "go-cli "+Version+"\n", out)
}

func TestRun_HelpCommand(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	out, err := run(t.Context(), cfg, "help")
	require.NoError(t, err)
	assert.Contains(t, out, "Usage: go-cli")
}

func TestRun_HelpFlag(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	out, err := run(t.Context(), cfg, "-help")
	require.NoError(t, err)
	assert.Contains(t, out, "Usage: go-cli")
}

func TestRun_NoArgs(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	out, err := run(t.Context(), cfg)
	require.NoError(t, err)
	// No subcommand defaults to interactive mode.
	assert.Contains(t, out, "Interactive session started")
}

func TestRun_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	cfg := &mockConfig{verbose: false}
	_, err := run(ctx, cfg, "version")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRun_Verbose(t *testing.T) {
	cfg := &mockConfig{verbose: true}
	_, err := run(t.Context(), cfg, "version")
	assert.NoError(t, err)
}

func TestRun_UnknownCommand(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	_, err := run(t.Context(), cfg, "unknown")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")

	var usageErr *UsageError
	assert.True(t, errors.As(err, &usageErr))
}

func TestRun_FlagParseError(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	_, err := run(t.Context(), cfg, "-invalid-flag")
	require.Error(t, err)

	var usageErr *UsageError
	assert.True(t, errors.As(err, &usageErr))
}

func TestRun_OutputWriterInjected(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	out, err := run(t.Context(), cfg, "version")
	require.NoError(t, err)
	assert.Equal(t, "go-cli "+Version+"\n", out)
}

func TestOutputWriter_Print(t *testing.T) {
	output := &bytes.Buffer{}
	ow := NewOutputWriter(output, false)
	ow.Print("hello %s", "world")
	assert.Equal(t, "hello world", output.String())
}

func TestOutputWriter_Verbose_Off(t *testing.T) {
	output := &bytes.Buffer{}
	ow := NewOutputWriter(output, false)
	ow.Verbose("debug info")
	assert.Empty(t, output.String())
}

func TestOutputWriter_Verbose_On(t *testing.T) {
	output := &bytes.Buffer{}
	ow := NewOutputWriter(output, true)
	ow.Verbose("debug info")
	assert.Equal(t, "debug info", output.String())
}

// --- ExecutionError tests ---

func TestExecutionError_Error_WithInnerError(t *testing.T) {
	inner := fmt.Errorf("something failed")
	e := newExecutionError("cmd", inner)
	assert.Equal(t, "cmd: something failed", e.Error())
}

func TestExecutionError_Error_WithoutInnerError(t *testing.T) {
	e := newExecutionError("cmd", nil)
	assert.Equal(t, "cmd", e.Error())
}

func TestExecutionError_Unwrap_ReturnsInner(t *testing.T) {
	inner := fmt.Errorf("root cause")
	e := newExecutionError("cmd", inner)
	assert.Equal(t, inner, e.Unwrap())
}

func TestExecutionError_Unwrap_NilInner(t *testing.T) {
	e := newExecutionError("cmd", nil)
	assert.Nil(t, e.Unwrap())
}

func TestExecutionError_ErrorsIs(t *testing.T) {
	inner := fmt.Errorf("base: %w", context.Canceled)
	e := newExecutionError("cmd", inner)
	assert.True(t, errors.Is(e, context.Canceled))
}

func TestExecutionError_ErrorsAs(t *testing.T) {
	inner := &UsageError{msg: "bad args"}
	e := newExecutionError("cmd", inner)

	var usageErr *UsageError
	assert.True(t, errors.As(e, &usageErr))
	assert.Equal(t, "bad args", usageErr.Error())
}

func TestNewExecutionError_Fields(t *testing.T) {
	err := newExecutionError("prompt", fmt.Errorf("timeout"))
	require.NotNil(t, err)
	assert.Equal(t, "prompt", err.msg)
	assert.NotNil(t, err.err)
}

// --- resolveModelName tests ---

func TestResolveModelName_FlagValueTakesPrecedence(t *testing.T) {
	rc := &config.Config{Provider: config.ProviderConfig{Model: "config-model"}, Model: config.ModelConfig{Name: "another"}}
	assert.Equal(t, "flag-model", resolveModelName("flag-model", rc))
}

func TestResolveModelName_ProviderModelFallback(t *testing.T) {
	rc := &config.Config{Provider: config.ProviderConfig{Model: "provider-model"}}
	assert.Equal(t, "provider-model", resolveModelName("", rc))
}

func TestResolveModelName_ModelNameFallback(t *testing.T) {
	rc := &config.Config{Model: config.ModelConfig{Name: "model-name"}}
	assert.Equal(t, "model-name", resolveModelName("", rc))
}

func TestResolveModelName_DefaultWhenNoConfig(t *testing.T) {
	assert.Equal(t, promptDefaultModel, resolveModelName("", nil))
}

func TestResolveModelName_DefaultWhenConfigEmpty(t *testing.T) {
	rc := &config.Config{}
	assert.Equal(t, promptDefaultModel, resolveModelName("", rc))
}

// --- resolveProviderName tests ---

func TestResolveProviderName_FlagValueTakesPrecedence(t *testing.T) {
	rc := &config.Config{Provider: config.ProviderConfig{Name: "config-provider"}}
	assert.Equal(t, "flag-provider", resolveProviderName("flag-provider", rc))
}

func TestResolveProviderName_ConfigFallback(t *testing.T) {
	rc := &config.Config{Provider: config.ProviderConfig{Name: "config-provider"}}
	assert.Equal(t, "config-provider", resolveProviderName("", rc))
}

func TestResolveProviderName_DefaultWhenNoConfig(t *testing.T) {
	assert.Equal(t, promptDefaultProvider, resolveProviderName("", nil))
}

func TestResolveProviderName_DefaultWhenConfigEmpty(t *testing.T) {
	rc := &config.Config{}
	assert.Equal(t, promptDefaultProvider, resolveProviderName("", rc))
}

// --- defaultConfig and NewDefaultConfig tests ---

func TestNewDefaultConfig_VerboseFalse(t *testing.T) {
	cfg := NewDefaultConfig(false)
	assert.False(t, cfg.Verbose())
}

func TestNewDefaultConfig_VerboseTrue(t *testing.T) {
	cfg := NewDefaultConfig(true)
	assert.True(t, cfg.Verbose())
}

// --- runCommand error wrapping test ---

func TestRunCommand_WrapsErrorAsExecutionError(t *testing.T) {
	reg := NewDefaultCommandRegistry()
	// Register a command that always fails.
	failingCmd := &failingCommand{}
	require.NoError(t, reg.Register(failingCmd))

	var out bytes.Buffer
	err := RunWithRegistry(t.Context(), &mockConfig{verbose: false}, []string{"fail"}, &out, reg)
	require.Error(t, err)

	var execErr *ExecutionError
	assert.True(t, errors.As(err, &execErr))
	assert.Equal(t, "fail", execErr.msg)
}

// failingCommand is a Command that always returns an error.
type failingCommand struct{}

func (c *failingCommand) Name() string     { return "fail" }
func (c *failingCommand) Synopsis() string { return "Always fails" }
func (c *failingCommand) Run(_ context.Context, _ Config, _ []string) error {
	return fmt.Errorf("command failed")
}

// --- versionCmd write error test ---

func TestVersionCmd_WriteError(t *testing.T) {
	// Use a writer that always fails.
	cmd := newVersionCmd(&failingWriter{})
	err := cmd.Run(t.Context(), &mockConfig{verbose: false}, nil)
	require.Error(t, err)

	var execErr *ExecutionError
	assert.True(t, errors.As(err, &execErr))
}

// failingWriter is an io.Writer that always returns an error.
type failingWriter struct{}

func (f *failingWriter) Write(_ []byte) (int, error) {
	return 0, fmt.Errorf("write failed")
}
