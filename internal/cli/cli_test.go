package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Contains(t, out, "Usage: go-cli")
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
