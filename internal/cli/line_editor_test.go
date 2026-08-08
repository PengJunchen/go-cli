package cli

import (
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
