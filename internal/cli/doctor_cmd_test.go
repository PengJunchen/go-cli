package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDoctorCommand verifies that doctorCmd implements the Command interface,
// returns the correct name, and Run produces output containing check names.
func TestDoctorCommand(t *testing.T) {
	var out bytes.Buffer
	cmd := newDoctorCmd(&out)

	// Verify it satisfies the Command interface at compile time.
	var _ Command = cmd

	assert.Equal(t, "doctor", cmd.Name())
	assert.NotEmpty(t, cmd.Synopsis())
	assert.Contains(t, cmd.Synopsis(), "diagnostic")

	// Run the command. Some checks may warn or fail in the test environment
	// (e.g. network), but the output should still contain check names.
	_ = cmd.Run(context.Background(), &mockConfig{verbose: false}, nil) //nolint:errcheck

	output := out.String()
	assert.Contains(t, output, "go-version")
	assert.Contains(t, output, "tools")
}

// TestDoctorCommandRegistered verifies that RunWithRegistry registers the
// doctor command in the registry.
func TestDoctorCommandRegistered(t *testing.T) {
	reg := NewDefaultCommandRegistry()
	cfg := &mockConfig{verbose: false}
	var out bytes.Buffer

	// Use -version flag to trigger built-in command registration without
	// running a full subcommand.
	err := RunWithRegistry(context.Background(), cfg, []string{"-version"}, &out, reg)
	require.NoError(t, err)

	// Verify the doctor command appears in the registry.
	cmd, ok := reg.Get("doctor")
	require.True(t, ok, "doctor command should be registered")
	assert.Equal(t, "doctor", cmd.Name())
	assert.NotEmpty(t, cmd.Synopsis())

	// Also verify it appears in List().
	var found bool
	for _, c := range reg.List() {
		if c.Name() == "doctor" {
			found = true
			break
		}
	}
	assert.True(t, found, "doctor command should appear in List()")
}
