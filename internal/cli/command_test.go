package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionCmd_Run(t *testing.T) {
	var out bytes.Buffer
	cmd := newVersionCmd(&out)

	err := cmd.Run(t.Context(), &mockConfig{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "go-cli "+Version+"\n", out.String())
}

func TestVersionCmd_Metadata(t *testing.T) {
	cmd := newVersionCmd(&bytes.Buffer{})
	assert.Equal(t, "version", cmd.Name())
	assert.NotEmpty(t, cmd.Synopsis())
}

func TestHelpCmd_Run(t *testing.T) {
	var out bytes.Buffer
	usageCalled := false
	cmd := newHelpCmd(&out, func() { usageCalled = true })

	err := cmd.Run(t.Context(), &mockConfig{}, nil)
	require.NoError(t, err)
	assert.True(t, usageCalled)
}

func TestHelpCmd_Metadata(t *testing.T) {
	cmd := newHelpCmd(&bytes.Buffer{}, func() {})
	assert.Equal(t, "help", cmd.Name())
	assert.NotEmpty(t, cmd.Synopsis())
}
