package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

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

// TestNonTTYCancel verifies that readLineNonTTY returns promptly when ctx is
// canceled (AC-3: within 100ms) and that the scanner goroutine does not leak
// after the reader is closed (AC-5).
func TestNonTTYCancel(t *testing.T) {
	r, w := io.Pipe()
	le := NewDefaultLineEditor(r, io.Discard)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		_, err := le.ReadLine(ctx, "> ")
		assert.ErrorIs(t, err, context.Canceled)
		close(done)
	}()

	// Give the goroutine time to enter scanner.Scan().
	time.Sleep(50 * time.Millisecond)

	beforeCancel := runtime.NumGoroutine()

	start := time.Now()
	cancel()

	// AC-3: ReadLine must return within 100ms of cancel.
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ReadLine did not return within 200ms of cancel")
	}
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 100*time.Millisecond, "should return within 100ms of cancel")

	// Close the pipe to unblock the scanner goroutine (AC-5: no leak).
	w.Close()

	// Poll for the scanner goroutine to exit instead of a fixed sleep.
	deadline := time.After(500 * time.Millisecond)
	for {
		if runtime.NumGoroutine() < beforeCancel {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("scanner goroutine did not exit after pipe close (goroutines: before=%d, now=%d)",
				beforeCancel, runtime.NumGoroutine())
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestNonTTYCancel_Reenter verifies that ReadLine can be safely called again
// after a context cancellation while the previous scanner goroutine is still
// in-flight. The fix uses a scanDone channel to serialize access to the
// non-thread-safe bufio.Scanner. This test would fail under -race without
// the fix.
func TestNonTTYCancel_Reenter(t *testing.T) {
	r, w := io.Pipe()
	le := NewDefaultLineEditor(r, io.Discard)

	// First call: cancel while the scanner is blocked on Scan().
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() {
		_, err := le.ReadLine(ctx1, "> ")
		done1 <- err
	}()

	time.Sleep(50 * time.Millisecond) // let the scanner enter Scan()
	cancel1()

	select {
	case err := <-done1:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("first ReadLine did not return within 200ms of cancel")
	}

	// At this point the scanner goroutine from the first call is still
	// blocked on Scan() because the pipe writer is still open. The second
	// ReadLine call must wait for it to exit before using the scanner.

	// Write TWO lines then close the pipe. The first line unblocks the
	// orphaned scanner goroutine (whose result nobody reads — it goes to
	// the buffered resultCh and is discarded). Because le.scanner is reused
	// across calls, the second line remains in the scanner's internal buffer
	// and is returned by the second ReadLine call.
	go func() {
		w.Write([]byte("unblock first\nsecond line\n"))
		w.Close()
	}()

	// Second call with a fresh context — must not race with the first
	// goroutine. Under -race, a concurrent scanner access would be detected.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()

	line, err := le.ReadLine(ctx2, "> ")
	require.NoError(t, err)
	assert.Equal(t, "second line", line)

	// Subsequent reads should return EOF (pipe is closed, buffer exhausted).
	_, err = le.ReadLine(ctx2, "> ")
	assert.ErrorIs(t, err, io.EOF)
}
