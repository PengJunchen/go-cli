package cli

import (
	"bytes"
	"context"
	"errors"
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

// TestReverseSearch verifies Ctrl+R reverse incremental search:
// enter search mode, type to filter, Enter confirms the matched entry.
func TestReverseSearch(t *testing.T) {
	history := []string{"hello world", "foo bar", "hello there"}
	le := NewDefaultLineEditor(strings.NewReader(""), io.Discard)
	le.SetHistory(history)

	// Key sequence: Ctrl+R, 'h', 'e', 'l', 'l', 'o', Enter
	keys := []byte{0x12, 'h', 'e', 'l', 'l', 'o', '\r'}
	idx := 0
	readByte := func() (byte, error) {
		if idx >= len(keys) {
			return 0, io.EOF
		}
		b := keys[idx]
		idx++
		return b, nil
	}

	line, err := le.readSingleLineTTY(readByte, "> ")
	require.NoError(t, err)
	// "hello there" is the newest match (index 2, searched in reverse).
	assert.Equal(t, "hello there", line)
}

// TestReverseSearch_NextMatch verifies that pressing Ctrl+R again cycles
// to the next older match.
func TestReverseSearch_NextMatch(t *testing.T) {
	history := []string{"hello world", "foo bar", "hello there"}
	le := NewDefaultLineEditor(strings.NewReader(""), io.Discard)
	le.SetHistory(history)

	// Ctrl+R, 'h', 'e', 'l', 'l', 'o', Ctrl+R (next match), Enter
	keys := []byte{0x12, 'h', 'e', 'l', 'l', 'o', 0x12, '\r'}
	idx := 0
	readByte := func() (byte, error) {
		if idx >= len(keys) {
			return 0, io.EOF
		}
		b := keys[idx]
		idx++
		return b, nil
	}

	line, err := le.readSingleLineTTY(readByte, "> ")
	require.NoError(t, err)
	// First match: "hello there" (index 2). Second Ctrl+R: "hello world" (index 0).
	assert.Equal(t, "hello world", line)
}

// TestReverseSearch_Cancel verifies that Esc cancels search and restores
// the original buffer content.
func TestReverseSearch_Cancel(t *testing.T) {
	history := []string{"hello world", "foo bar"}
	le := NewDefaultLineEditor(strings.NewReader(""), io.Discard)
	le.SetHistory(history)

	// Type "abc", Ctrl+R, 'h', Esc, Enter
	// After Esc, buffer should be restored to "abc".
	keys := []byte{'a', 'b', 'c', 0x12, 'h', 0x1B, '\r'}
	idx := 0
	readByte := func() (byte, error) {
		if idx >= len(keys) {
			return 0, io.EOF
		}
		b := keys[idx]
		idx++
		return b, nil
	}

	line, err := le.readSingleLineTTY(readByte, "> ")
	require.NoError(t, err)
	assert.Equal(t, "abc", line, "Esc should restore original buffer")
}

// TestReverseSearch_NoMatch verifies behavior when no history entry matches.
func TestReverseSearch_NoMatch(t *testing.T) {
	history := []string{"hello world", "foo bar"}
	le := NewDefaultLineEditor(strings.NewReader(""), io.Discard)
	le.SetHistory(history)

	// Ctrl+R, 'x', 'y', 'z', Enter — no match, should restore original buffer.
	keys := []byte{0x12, 'x', 'y', 'z', '\r'}
	idx := 0
	readByte := func() (byte, error) {
		if idx >= len(keys) {
			return 0, io.EOF
		}
		b := keys[idx]
		idx++
		return b, nil
	}

	line, err := le.readSingleLineTTY(readByte, "> ")
	require.NoError(t, err)
	assert.Equal(t, "", line, "no match should restore empty original buffer")
}

// TestHomeEndKeys verifies that ESC[H moves cursor to beginning and ESC[F
// moves cursor to end.
func TestHomeEndKeys(t *testing.T) {
	le := NewDefaultLineEditor(strings.NewReader(""), io.Discard)

	// Type "hello", Home (ESC[H), type "X" → "Xhello"
	// Then End (ESC[F), type "Y" → "XhelloY"
	keys := []byte{'h', 'e', 'l', 'l', 'o', 0x1B, '[', 'H', 'X', 0x1B, '[', 'F', 'Y', '\r'}
	idx := 0
	readByte := func() (byte, error) {
		if idx >= len(keys) {
			return 0, io.EOF
		}
		b := keys[idx]
		idx++
		return b, nil
	}

	line, err := le.readSingleLineTTY(readByte, "> ")
	require.NoError(t, err)
	assert.Equal(t, "XhelloY", line)
}

// TestCtrlACtrlE verifies that Ctrl+A moves cursor to beginning of line and
// Ctrl+E moves cursor to end of line, and that they coexist with Home/End
// without conflicts (AC-6).
func TestCtrlACtrlE(t *testing.T) {
	le := NewDefaultLineEditor(strings.NewReader(""), io.Discard)

	// Type "hello", Ctrl+A (beginning), type "X" → "Xhello"
	// Then Ctrl+E (end), type "Y" → "XhelloY"
	keys := []byte{'h', 'e', 'l', 'l', 'o', 0x01, 'X', 0x05, 'Y', '\r'}
	idx := 0
	readByte := func() (byte, error) {
		if idx >= len(keys) {
			return 0, io.EOF
		}
		b := keys[idx]
		idx++
		return b, nil
	}

	line, err := le.readSingleLineTTY(readByte, "> ")
	require.NoError(t, err)
	assert.Equal(t, "XhelloY", line)
}

// TestCtrlAE_HomeEnd_NoConflict verifies that Ctrl+A/E and Home/End can be
// interleaved without interfering with each other (AC-6).
func TestCtrlAE_HomeEnd_NoConflict(t *testing.T) {
	le := NewDefaultLineEditor(strings.NewReader(""), io.Discard)

	// Type "abc", Ctrl+A (beginning), type "1" → "1abc"
	// Home (ESC[H) is already at beginning, type "2" → "21abc"
	// Ctrl+E (end), type "3" → "21abc3"
	// End (ESC[F) is already at end, type "4" → "21abc34"
	keys := []byte{'a', 'b', 'c', 0x01, '1', 0x1B, '[', 'H', '2', 0x05, '3', 0x1B, '[', 'F', '4', '\r'}
	idx := 0
	readByte := func() (byte, error) {
		if idx >= len(keys) {
			return 0, io.EOF
		}
		b := keys[idx]
		idx++
		return b, nil
	}

	line, err := le.readSingleLineTTY(readByte, "> ")
	require.NoError(t, err)
	assert.Equal(t, "21abc34", line)
}

// TestFindReverseMatch unit-tests the search helper directly.
func TestFindReverseMatch(t *testing.T) {
	entries := []string{"hello world", "foo bar", "hello there", "World peace"}

	// Search "hello" — newest match first (index 2).
	idx := findReverseMatch(entries, "hello", -1)
	assert.Equal(t, 2, idx)

	// Next match (older): index 0.
	idx = findReverseMatch(entries, "hello", idx)
	assert.Equal(t, 0, idx)

	// No more matches.
	idx = findReverseMatch(entries, "hello", idx)
	assert.Equal(t, -1, idx)

	// Case-insensitive: "world" matches both "hello world" and "World peace".
	idx = findReverseMatch(entries, "world", -1)
	assert.Equal(t, 3, idx) // "World peace" (newest)
	idx = findReverseMatch(entries, "world", idx)
	assert.Equal(t, 0, idx) // "hello world"

	// No match.
	assert.Equal(t, -1, findReverseMatch(entries, "xyz", -1))

	// Empty query returns -1.
	assert.Equal(t, -1, findReverseMatch(entries, "", -1))
}

// ---------------------------------------------------------------------------
// Graded Ctrl+C semantics tests
// ---------------------------------------------------------------------------

// TestInterruptedError_ErrorsIs verifies that *interruptedError satisfies
// errors.Is(err, errInterrupted) so the REPL loop can detect Ctrl+C.
func TestInterruptedError_ErrorsIs(t *testing.T) {
	err := &interruptedError{hadContent: true}
	assert.True(t, errors.Is(err, errInterrupted))
	assert.True(t, errors.Is(err, errInterrupted))

	err2 := &interruptedError{hadContent: false}
	assert.True(t, errors.Is(err2, errInterrupted))
}

// TestInterruptedError_ErrorsAs verifies that the hadContent field can be
// extracted via errors.As.
func TestInterruptedError_ErrorsAs(t *testing.T) {
	err := &interruptedError{hadContent: true}
	var ie *interruptedError
	require.True(t, errors.As(err, &ie))
	assert.True(t, ie.hadContent)

	err2 := &interruptedError{hadContent: false}
	var ie2 *interruptedError
	require.True(t, errors.As(err2, &ie2))
	assert.False(t, ie2.hadContent)
}

// TestReadSingleLineTTY_CtrlC_WithContent verifies that pressing Ctrl+C
// when the buffer has content returns an interruptedError with
// hadContent=true (AC-1: clears current input and re-prompts).
func TestReadSingleLineTTY_CtrlC_WithContent(t *testing.T) {
	var out bytes.Buffer
	le := NewDefaultLineEditor(strings.NewReader(""), &out)

	// Type "hello", then Ctrl+C
	keys := []byte{'h', 'e', 'l', 'l', 'o', 0x03}
	idx := 0
	readByte := func() (byte, error) {
		if idx >= len(keys) {
			return 0, io.EOF
		}
		b := keys[idx]
		idx++
		return b, nil
	}

	line, err := le.readSingleLineTTY(readByte, "> ")
	assert.Equal(t, "", line)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errInterrupted))

	var ie *interruptedError
	require.True(t, errors.As(err, &ie))
	assert.True(t, ie.hadContent, "hadContent should be true when buffer has text")
}

// TestReadSingleLineTTY_CtrlC_EmptyBuffer verifies that pressing Ctrl+C
// on an empty buffer returns an interruptedError with hadContent=false
// (AC-2: shows 'press again to exit' message).
func TestReadSingleLineTTY_CtrlC_EmptyBuffer(t *testing.T) {
	var out bytes.Buffer
	le := NewDefaultLineEditor(strings.NewReader(""), &out)

	// Just Ctrl+C on empty line
	keys := []byte{0x03}
	idx := 0
	readByte := func() (byte, error) {
		if idx >= len(keys) {
			return 0, io.EOF
		}
		b := keys[idx]
		idx++
		return b, nil
	}

	line, err := le.readSingleLineTTY(readByte, "> ")
	assert.Equal(t, "", line)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errInterrupted))

	var ie *interruptedError
	require.True(t, errors.As(err, &ie))
	assert.False(t, ie.hadContent, "hadContent should be false when buffer is empty")
}

// TestEvaluateCtrlC_HadContent verifies that when the input buffer had
// content, the action is always ctrlCClearLine regardless of timing
// (AC-1).
func TestEvaluateCtrlC_HadContent(t *testing.T) {
	now := time.Now()
	last := now.Add(-500 * time.Millisecond)

	action, newLast := evaluateCtrlC(true, last, now, ctrlCDoublePressWindow)
	assert.Equal(t, ctrlCClearLine, action)
	assert.True(t, newLast.IsZero(), "timer should be reset after clear-line")

	// Even if within the double-press window, content present → clear line.
	action, newLast = evaluateCtrlC(true, now.Add(-100*time.Millisecond), now, ctrlCDoublePressWindow)
	assert.Equal(t, ctrlCClearLine, action)
	assert.True(t, newLast.IsZero())
}

// TestEvaluateCtrlC_EmptyFirstPress verifies that the first Ctrl+C on an
// empty line shows the exit prompt (AC-2).
func TestEvaluateCtrlC_EmptyFirstPress(t *testing.T) {
	now := time.Now()

	action, newLast := evaluateCtrlC(false, time.Time{}, now, ctrlCDoublePressWindow)
	assert.Equal(t, ctrlCShowExitPrompt, action)
	assert.Equal(t, now, newLast, "timestamp should be recorded")
}

// TestEvaluateCtrlC_DoublePressWithinWindow verifies that a second Ctrl+C
// within the window exits the REPL (AC-3).
func TestEvaluateCtrlC_DoublePressWithinWindow(t *testing.T) {
	now := time.Now()
	last := now.Add(-800 * time.Millisecond) // 800ms < 1500ms window

	action, newLast := evaluateCtrlC(false, last, now, ctrlCDoublePressWindow)
	assert.Equal(t, ctrlCExit, action)
	assert.True(t, newLast.IsZero(), "timer should be reset after exit")
}

// TestEvaluateCtrlC_DoublePressAfterWindow verifies that a Ctrl+C after
// the window expires shows the exit prompt again instead of exiting.
func TestEvaluateCtrlC_DoublePressAfterWindow(t *testing.T) {
	now := time.Now()
	last := now.Add(-2 * time.Second) // 2000ms > 1500ms window

	action, newLast := evaluateCtrlC(false, last, now, ctrlCDoublePressWindow)
	assert.Equal(t, ctrlCShowExitPrompt, action)
	assert.Equal(t, now, newLast, "timestamp should be refreshed")
}

// TestEvaluateCtrlC_DoublePressAtWindowBoundary verifies the boundary
// condition: exactly at the window limit, the press still exits.
func TestEvaluateCtrlC_DoublePressAtWindowBoundary(t *testing.T) {
	now := time.Now()
	last := now.Add(-ctrlCDoublePressWindow) // exactly at boundary

	action, _ := evaluateCtrlC(false, last, now, ctrlCDoublePressWindow)
	assert.Equal(t, ctrlCExit, action, "press at exactly the window boundary should exit")
}

// TestEvaluateCtrlC_Sequence simulates the full double-press sequence:
// first press → show prompt, second press (within window) → exit.
func TestEvaluateCtrlC_Sequence(t *testing.T) {
	window := ctrlCDoublePressWindow
	var lastInterrupt time.Time

	// First Ctrl+C on empty line.
	t1 := time.Now()
	action, lastInterrupt := evaluateCtrlC(false, lastInterrupt, t1, window)
	assert.Equal(t, ctrlCShowExitPrompt, action)

	// Second Ctrl+C within window.
	t2 := t1.Add(500 * time.Millisecond)
	action, lastInterrupt = evaluateCtrlC(false, lastInterrupt, t2, window)
	assert.Equal(t, ctrlCExit, action)
	assert.True(t, lastInterrupt.IsZero())

	// After exit, a new press should show the prompt again (timer reset).
	t3 := t2.Add(200 * time.Millisecond)
	action, lastInterrupt = evaluateCtrlC(false, lastInterrupt, t3, window)
	assert.Equal(t, ctrlCShowExitPrompt, action)
}
