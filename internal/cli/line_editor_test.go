package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDetectTTY_NoSttyDependency verifies that detectTTY returns false for
// non-TTY inputs (e.g. /dev/null or an in-memory reader) without relying on
// any external stty binary.
func TestDetectTTY_NoSttyDependency(t *testing.T) {
	// /dev/null is a character device but NOT a terminal. detectTTY must
	// return false without relying on any external stty binary.
	f, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	require.NoError(t, err)
	defer f.Close()

	le := NewDefaultLineEditor(f, io.Discard)
	assert.False(t, le.detectTTY(), "devnull should not be detected as a TTY")

	// A non-*os.File reader is never a TTY.
	le2 := NewDefaultLineEditor(strings.NewReader("hello\n"), io.Discard)
	assert.False(t, le2.detectTTY(), "strings.Reader should not be a TTY")
}

// TestTerminalWidth_FallbackNonTTY verifies that terminalWidth returns the
// 80-column fallback for non-TTY inputs without spawning any external process
// (the old stty-based implementation forked stty).
func TestTerminalWidth_FallbackNonTTY(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	require.NoError(t, err)
	defer f.Close()

	le := NewDefaultLineEditor(f, io.Discard)
	assert.Equal(t, 80, le.terminalWidth())

	// Non-*os.File reader also falls back to 80.
	le2 := NewDefaultLineEditor(strings.NewReader("hello"), io.Discard)
	assert.Equal(t, 80, le2.terminalWidth())
}

// TestReadLineNonTTY_BackwardCompat verifies that piped (non-TTY) input still
// works through the bufio.Scanner fallback path, preserving backward
// compatibility.
func TestReadLineNonTTY_BackwardCompat(t *testing.T) {
	input := "first line\nsecond line\n"
	le := NewDefaultLineEditor(strings.NewReader(input), io.Discard)

	ctx := context.Background()
	line, err := le.ReadLine(ctx, "> ")
	require.NoError(t, err)
	assert.Equal(t, "first line", line)

	line2, err := le.ReadLine(ctx, "> ")
	require.NoError(t, err)
	assert.Equal(t, "second line", line2)

	// Third read should return EOF.
	_, err = le.ReadLine(ctx, "> ")
	assert.ErrorIs(t, err, io.EOF)
}

// TestApplyCompletions_AlignedDisplay verifies that when multiple completion
// candidates are displayed, their descriptions are column-aligned using the
// longest Text width.
func TestApplyCompletions_AlignedDisplay(t *testing.T) {
	var out bytes.Buffer
	le := NewDefaultLineEditor(strings.NewReader(""), &out)

	completions := []Completion{
		{Text: "list", Description: "List all stored memories"},
		{Text: "add", Description: "Add a manual memory"},
		{Text: "delete", Description: "Delete a memory by ID"},
	}

	input := []rune("/memory ")
	pos := len(input)
	le.applyCompletions(&input, &pos, completions, 8, "> ")

	output := out.String()
	lines := strings.Split(output, "\r\n")

	// Extract the column at which each description starts.
	descs := []string{
		"List all stored memories",
		"Add a manual memory",
		"Delete a memory by ID",
	}
	var descCols []int
	for _, desc := range descs {
		found := false
		for _, line := range lines {
			if idx := strings.Index(line, desc); idx >= 0 {
				descCols = append(descCols, idx)
				found = true
				break
			}
		}
		require.True(t, found, "description %q not found in output", desc)
	}

	require.Len(t, descCols, 3)
	assert.Equal(t, descCols[0], descCols[1], "descriptions should be aligned (list vs add)")
	assert.Equal(t, descCols[1], descCols[2], "descriptions should be aligned (add vs delete)")
}
