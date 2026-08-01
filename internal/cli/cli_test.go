package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

type mockConfig struct {
	verbose bool
}

func (m *mockConfig) Verbose() bool { return m.verbose }

func TestRun_VersionFlag(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	err := Run(t.Context(), cfg, []string{"-version"})
	assert.NoError(t, err)
}

func TestRun_VersionCommand(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	err := Run(t.Context(), cfg, []string{"version"})
	assert.NoError(t, err)
}

func TestRun_HelpCommand(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	err := Run(t.Context(), cfg, []string{"help"})
	assert.NoError(t, err)
}

func TestRun_NoArgs(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	err := Run(t.Context(), cfg, []string{})
	assert.NoError(t, err)
}

func TestRun_UnknownCommand(t *testing.T) {
	cfg := &mockConfig{verbose: false}
	err := Run(t.Context(), cfg, []string{"unknown"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown command")
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
