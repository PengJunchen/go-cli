package cli

import (
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// -----------------------------------------------------------------------------
// CompositeCompleter tests
// -----------------------------------------------------------------------------

// TestCompositeCompleter_SlashCommand verifies that the CompositeCompleter
// delegates to SlashCommandCompleter when the input starts with "/".
func TestCompositeCompleter_SlashCommand(t *testing.T) {
	c := NewCompositeCompleter(
		NewSlashCommandCompleter([]string{"help", "cost", "exit"}),
		NewFilePathCompleter(),
	)
	completions, start := c.Complete("/he", 3)
	require.Len(t, completions, 1)
	assert.Equal(t, "/help", completions[0].Text)
	assert.Equal(t, 0, start)
}

// TestCompositeCompleter_SlashFullMatch verifies that a fully typed slash
// command still returns a match (prefix match includes the exact string).
func TestCompositeCompleter_SlashFullMatch(t *testing.T) {
	c := NewCompositeCompleter(
		NewSlashCommandCompleter([]string{"help", "cost", "exit"}),
		NewFilePathCompleter(),
	)
	completions, _ := c.Complete("/help", 5)
	require.Len(t, completions, 1)
	assert.Equal(t, "/help", completions[0].Text)
}

// TestCompositeCompleter_FilePath verifies that the CompositeCompleter falls
// through to FilePathCompleter for non-slash input.
func TestCompositeCompleter_FilePath(t *testing.T) {
	dir := t.TempDir()
	origWd, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origWd) //nolint:errcheck

	require.NoError(t, os.Chdir(dir))
	require.NoError(t, os.WriteFile("hello.txt", []byte("x"), 0644))

	c := NewCompositeCompleter(
		NewSlashCommandCompleter([]string{"help"}),
		NewFilePathCompleter(),
	)
	completions, start := c.Complete("he", 2)
	require.Len(t, completions, 1)
	assert.Equal(t, "hello.txt", completions[0].Text)
	assert.Equal(t, "file", completions[0].Description)
	assert.Equal(t, 0, start)
}

// TestCompositeCompleter_FilePathWithSlash verifies path completion with a
// directory separator.
func TestCompositeCompleter_FilePathWithSlash(t *testing.T) {
	dir := t.TempDir()
	origWd, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origWd) //nolint:errcheck

	require.NoError(t, os.Chdir(dir))
	require.NoError(t, os.Mkdir("sub", 0755))
	require.NoError(t, os.WriteFile(filepath.Join("sub", "world.go"), []byte("x"), 0644))

	c := NewCompositeCompleter(
		NewSlashCommandCompleter([]string{"help"}),
		NewFilePathCompleter(),
	)
	completions, start := c.Complete("sub/wor", 7)
	require.Len(t, completions, 1)
	assert.Equal(t, "sub/world.go", completions[0].Text)
	assert.Equal(t, 0, start)
}

// TestCompositeCompleter_NoMatch verifies that the completer returns nil, 0
// when no child produces matches.
func TestCompositeCompleter_NoMatch(t *testing.T) {
	dir := t.TempDir()
	origWd, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origWd) //nolint:errcheck

	require.NoError(t, os.Chdir(dir))

	c := NewCompositeCompleter(
		NewSlashCommandCompleter([]string{"help"}),
		NewFilePathCompleter(),
	)
	completions, start := c.Complete("zzznonexistent", 14)
	assert.Nil(t, completions)
	assert.Equal(t, 0, start)
}

// TestCompositeCompleter_SlashFallsThrough verifies that when the slash
// command completer returns no matches, the composite falls through to the
// next child (FilePathCompleter).
func TestCompositeCompleter_SlashFallsThrough(t *testing.T) {
	dir := t.TempDir()
	origWd, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origWd) //nolint:errcheck

	require.NoError(t, os.Chdir(dir))
	// Create a file whose name starts with "/" — on Unix this is just a
	// regular filename in the current directory (the "/" is part of the
	// word prefix extraction). Actually, FilePathCompleter splits on "/",
	// so a file named "xy" would match the word "/xy" with prefix "xy"
	// in dir "/". Instead, create a file that matches after the slash.
	require.NoError(t, os.WriteFile("xyfile.txt", []byte("x"), 0644))

	c := NewCompositeCompleter(
		NewSlashCommandCompleter([]string{"help"}),
		NewFilePathCompleter(),
	)
	// "/xy" - SlashCommandCompleter returns no match (no command starts
	// with "/xy"). FilePathCompleter extracts word "/xy", splits on "/" to
	// get dir="/" and prefix="xy". It reads "/" and finds "xyfile.txt" is
	// NOT there. Let's instead use input "xyf" without the slash.
	completions, _ := c.Complete("xyf", 3)
	// SlashCommandCompleter: no match (doesn't start with "/").
	// FilePathCompleter: word="xyf", dir=".", prefix="xyf" -> matches xyfile.txt.
	require.Len(t, completions, 1)
	assert.Equal(t, "xyfile.txt", completions[0].Text)
}

// TestCompositeCompleter_EmptyChildren verifies that a composite with no
// children returns nil, 0.
func TestCompositeCompleter_EmptyChildren(t *testing.T) {
	c := NewCompositeCompleter()
	completions, start := c.Complete("anything", 8)
	assert.Nil(t, completions)
	assert.Equal(t, 0, start)
}

// TestCompositeCompleter_NilChildSkipped verifies that nil children are
// skipped without panicking.
func TestCompositeCompleter_NilChildSkipped(t *testing.T) {
	c := NewCompositeCompleter(
		nil,
		NewSlashCommandCompleter([]string{"help"}),
		nil,
	)
	completions, _ := c.Complete("/he", 3)
	require.Len(t, completions, 1)
	assert.Equal(t, "/help", completions[0].Text)
}

// -----------------------------------------------------------------------------
// Registry-backed completer tests
// -----------------------------------------------------------------------------

// TestSlashCommandCompleter_RegistryReturnsRealDescription verifies that a
// registry-backed completer returns the handler's real Description instead of
// the hardcoded "slash command" string.
func TestSlashCommandCompleter_RegistryReturnsRealDescription(t *testing.T) {
	c := NewSlashCommandCompleterFromRegistry(defaultSlashReg)
	completions, _ := c.Complete("/mem", 4)
	require.Len(t, completions, 1)
	assert.Equal(t, "/memory", completions[0].Text)
	assert.Equal(t, "Manage cross-session memories: list, add, delete, search, clear",
		completions[0].Description)
}

// TestSlashCommandCompleter_SubcommandCompletion verifies that subcommand
// completion works for handlers implementing SubcommandProvider.
func TestSlashCommandCompleter_SubcommandCompletion(t *testing.T) {
	c := NewSlashCommandCompleterFromRegistry(defaultSlashReg)

	// "/memory " (trailing space, cursor at end) returns all subcommands.
	completions, start := c.Complete("/memory ", 8)
	require.Len(t, completions, 5)
	assert.Equal(t, 8, start) // subcommand starts right after the space
	names := make([]string, len(completions))
	for i, comp := range completions {
		names[i] = comp.Text
	}
	assert.Contains(t, names, "list")
	assert.Contains(t, names, "add")
	assert.Contains(t, names, "delete")
	assert.Contains(t, names, "search")
	assert.Contains(t, names, "clear")

	// "/memory li" returns only "list".
	completions, start = c.Complete("/memory li", 10)
	require.Len(t, completions, 1)
	assert.Equal(t, "list", completions[0].Text)
	assert.Equal(t, "List all stored memories", completions[0].Description)
	assert.Equal(t, 8, start)
}

// TestSlashCommandCompleter_NoSubcommandsFallsThrough verifies that a handler
// without SubcommandProvider returns nil on space (backward compat).
func TestSlashCommandCompleter_NoSubcommandsFallsThrough(t *testing.T) {
	c := NewSlashCommandCompleterFromRegistry(defaultSlashReg)
	// /cost does not implement SubcommandProvider.
	completions, _ := c.Complete("/cost ", 6)
	assert.Nil(t, completions)
}

// TestSlashCommandCompleter_BackwardCompatNamesOnly verifies that the old
// constructor with []string still works: descriptions are "slash command" and
// spaces return nil.
func TestSlashCommandCompleter_BackwardCompatNamesOnly(t *testing.T) {
	c := NewSlashCommandCompleter([]string{"help", "cost", "exit"})

	// Command name completion with default description.
	completions, _ := c.Complete("/he", 3)
	require.Len(t, completions, 1)
	assert.Equal(t, "/help", completions[0].Text)
	assert.Equal(t, "slash command", completions[0].Description)

	// Space returns nil (no registry, no subcommand completion).
	completions, _ = c.Complete("/help ", 6)
	assert.Nil(t, completions)
}

// TestEscStandalone_TriggersCancel verifies that a standalone ESC byte (with
// no following bytes within escSequenceTimeout) triggers cancelFn.
func TestEscStandalone_TriggersCancel(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close() //nolint:errcheck

	var cancelled atomic.Bool
	h := NewInterruptHandler(func() { cancelled.Store(true) })
	h.Start(r)

	// Write a standalone ESC byte.
	go func() {
		w.Write([]byte{0x1B}) //nolint:errcheck
	}()

	// Wait beyond escSequenceTimeout (50ms) for the cancel to fire.
	require.Eventually(t, func() bool {
		return cancelled.Load()
	}, 500*time.Millisecond, 10*time.Millisecond, "cancelFn should be called after standalone Esc")

	// Close the write end so the reader goroutine gets EOF and exits.
	w.Close()
	h.Stop()
}

// TestEscSequence_CSI_NoCancel verifies that a CSI escape sequence (ESC [ A,
// i.e. the up-arrow key) does NOT trigger cancelFn because the following
// bytes arrive within escSequenceTimeout.
func TestEscSequence_CSI_NoCancel(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close() //nolint:errcheck

	var cancelled atomic.Bool
	h := NewInterruptHandler(func() { cancelled.Store(true) })
	h.Start(r)

	// Write the full CSI sequence (ESC [ A = up arrow) in one Write call so
	// all bytes are available immediately.
	go func() {
		w.Write([]byte{0x1B, '[', 'A'}) //nolint:errcheck
	}()

	// Wait beyond escSequenceTimeout to ensure no cancel fires.
	time.Sleep(150 * time.Millisecond)
	assert.False(t, cancelled.Load(), "cancelFn should NOT be called for CSI sequence")

	// Close the write end so the reader goroutine gets EOF and exits.
	w.Close()
	h.Stop()
}

// TestEsc_StopWaitsForMonitor verifies that Stop blocks until the Esc monitor
// goroutine has fully exited.
func TestEsc_StopWaitsForMonitor(t *testing.T) {
	r, w := io.Pipe()

	var cancelled atomic.Bool
	h := NewInterruptHandler(func() { cancelled.Store(true) })
	h.Start(r)

	// Close the write end so the reader gets EOF and the monitor exits.
	w.Close()

	// Stop should return without hanging.
	done := make(chan struct{})
	go func() {
		h.Stop()
		close(done)
	}()
	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2s")
	}
	r.Close() //nolint:errcheck
}

// TestEsc_StopInterruptsBlockedRead verifies that Stop can interrupt the
// monitor even when it is blocked waiting for input (pipe still open).
func TestEsc_StopInterruptsBlockedRead(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close() //nolint:errcheck
	defer w.Close() //nolint:errcheck

	var cancelled atomic.Bool
	h := NewInterruptHandler(func() { cancelled.Store(true) })
	h.Start(r)

	// Stop should return promptly even though no bytes have been read.
	done := make(chan struct{})
	go func() {
		h.Stop()
		close(done)
	}()
	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2s while monitor blocked on read")
	}
}

// TestInterruptHandler_NilReaderNoEsc verifies that Start(nil) does not start
// the Esc monitor, maintaining backward compatibility with non-TTY callers.
func TestInterruptHandler_NilReaderNoEsc(t *testing.T) {
	var cancelled atomic.Bool
	h := NewInterruptHandler(func() { cancelled.Store(true) })
	h.Start(nil)

	// escActive should be false.
	assert.False(t, h.escActive, "escActive should be false when Start(nil) is called")
	assert.Nil(t, h.escDone, "escDone should be nil when Start(nil) is called")

	// Stop should work fine without waiting on escDone.
	h.Stop()

	// cancelFn should never have been called.
	assert.False(t, cancelled.Load(), "cancelFn should not be called with nil reader")
}

// TestInterruptHandler_StartNilBackwardCompat is a regression test ensuring
// existing callers that pass nil (e.g. steer_test.go) still work.
func TestInterruptHandler_StartNilBackwardCompat(t *testing.T) {
	cancel := func() {}
	h := NewInterruptHandler(cancel)
	h.Start(nil)
	defer h.Stop()

	require.NoError(t, h.SendSteer("test message"))

	select {
	case msg := <-h.SteerChannel():
		assert.Equal(t, "test message", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for steer message")
	}
}
