//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 21 TUI Enhancements: dynamic terminal width,
// code/markdown highlighting, collapsible tool-call rendering, non-TTY
// degradation, and removal of legacy hardcoded width / keyboard-nav options.
package e2e_20260802

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tui"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// moduleRoot returns the project root directory (where go.mod lives) by
// walking up three directories from this test file's location.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller failed")
	// tests/e2e_20260802/28_..._test.go -> tests/e2e_20260802 -> tests -> root
	return filepath.Dir(filepath.Dir(filepath.Dir(filename)))
}

// isStdoutTTY reports whether os.Stdout is a terminal device. Test processes
// launched by `go test` typically have piped (non-TTY) stdout.
func isStdoutTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// searchGoFilesForPattern walks root recursively and returns paths of .go
// files whose contents contain pattern.
func searchGoFilesForPattern(t *testing.T, root, pattern string) []string {
	t.Helper()
	var matches []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(content), pattern) {
			matches = append(matches, path)
		}
		return nil
	})
	require.NoError(t, err)
	return matches
}

// =============================================================================
// Phase 21 TUI Enhancements E2E Tests
// =============================================================================

// TestET_Phase21_TUI_AC1_DynamicWidth verifies that the terminal size provider
// returns a positive width, falling back to 80 in non-TTY mode.
func TestET_Phase21_TUI_AC1_DynamicWidth(t *testing.T) {
	provider := tui.NewDefaultTerminalSizeProvider()
	width := provider.Width()

	assert.Greater(t, width, 0, "Width() must return a positive value, got %d", width)

	if !isStdoutTTY() {
		assert.Equal(t, 80, width, "in non-TTY mode Width() should fall back to 80")
	}
}

// TestET_Phase21_TUI_AC2_CodeHighlighting verifies that the code highlighter
// applies ANSI escape sequences in TTY mode and returns unchanged code in
// non-TTY mode.
func TestET_Phase21_TUI_AC2_CodeHighlighting(t *testing.T) {
	h := tui.NewDefaultCodeHighlighter()
	code := "func main() { return }"

	result := h.Highlight(code, "go")

	if isStdoutTTY() {
		assert.Contains(t, result, "\033[34m", "in TTY mode Go keywords should be colored blue")
	} else {
		assert.Equal(t, code, result, "in non-TTY mode code should be returned unchanged")
	}
}

// TestET_Phase21_TUI_AC3_MarkdownHighlighting verifies that markdown
// highlighting processes code blocks. In non-TTY mode the text is returned
// unchanged; in TTY mode the code block receives ANSI highlighting while
// non-code text is preserved.
func TestET_Phase21_TUI_AC3_MarkdownHighlighting(t *testing.T) {
	h := tui.NewDefaultCodeHighlighter()
	text := "Some text\n```go\nfunc main() {}\n```\nMore text"

	result := h.HighlightMarkdown(text)

	if isStdoutTTY() {
		// Code block should contain ANSI codes for Go keywords.
		assert.Contains(t, result, "\033[34m", "code block should have highlighted Go keywords")
		// Non-code text should be unchanged.
		assert.Contains(t, result, "Some text", "non-code text should be preserved")
		assert.Contains(t, result, "More text", "non-code text should be preserved")
	} else {
		assert.Equal(t, text, result, "in non-TTY mode markdown should be returned unchanged")
	}
}

// TestET_Phase21_TUI_AC4_ToolCallCollapsedRendering verifies that
// RenderCollapsed produces a single-line summary with the tool name and args.
func TestET_Phase21_TUI_AC4_ToolCallCollapsedRendering(t *testing.T) {
	r := tui.NewToolCallRenderer()
	result := r.RenderCollapsed("read", map[string]any{"path": "/tmp/test.txt"})

	assert.False(t, strings.Contains(result, "\n"), "collapsed render should be single-line")
	assert.Contains(t, result, "read", "should contain tool name")
	assert.Contains(t, result, "path", "should contain arg key")
	assert.Contains(t, result, "/tmp/test.txt", "should contain arg value")
}

// TestET_Phase21_TUI_AC5_ToolCallExpandedRendering verifies that
// RenderExpanded produces a multi-line output with full args and result.
func TestET_Phase21_TUI_AC5_ToolCallExpandedRendering(t *testing.T) {
	r := tui.NewToolCallRenderer()
	result := r.RenderExpanded("read", map[string]any{"path": "/tmp/test.txt"}, "file content")

	assert.True(t, strings.Contains(result, "\n"), "expanded render should be multi-line")
	assert.Contains(t, result, "read", "should contain tool name")
	assert.Contains(t, result, "path", "should contain arg key")
	assert.Contains(t, result, "/tmp/test.txt", "should contain arg value")
	assert.Contains(t, result, "file content", "should contain result")
}

// TestET_Phase21_TUI_AC6_NonTTYDegradation verifies that in non-TTY mode the
// highlighter returns code without any ANSI escape sequences.
func TestET_Phase21_TUI_AC6_NonTTYDegradation(t *testing.T) {
	h := tui.NewDefaultCodeHighlighter()
	code := "func main() { return }"

	result := h.Highlight(code, "go")

	if !isStdoutTTY() {
		assert.NotContains(t, result, "\033[", "non-TTY output should not contain ANSI escape sequences")
		assert.Equal(t, code, result, "non-TTY output should equal input")
	} else {
		t.Skip("stdout is a TTY; non-TTY degradation cannot be tested in this environment")
	}
}

// TestET_Phase21_TUI_AC7_TerminalSizeProviderFallback verifies that when stdout
// is not a TTY, Width() returns 80 and Height() returns 24.
func TestET_Phase21_TUI_AC7_TerminalSizeProviderFallback(t *testing.T) {
	if isStdoutTTY() {
		t.Skip("stdout is a TTY; fallback behavior cannot be tested in this environment")
	}

	provider := tui.NewDefaultTerminalSizeProvider()
	assert.Equal(t, 80, provider.Width(), "non-TTY Width() should fall back to 80")
	assert.Equal(t, 24, provider.Height(), "non-TTY Height() should fall back to 24")
}

// TestET_Phase21_TUI_AC8_NoWithoutKeyboardNavigation verifies that the
// WithoutKeyboardNavigation function no longer exists in the codebase.
func TestET_Phase21_TUI_AC8_NoWithoutKeyboardNavigation(t *testing.T) {
	root := moduleRoot(t)
	matches := searchGoFilesForPattern(t, filepath.Join(root, "internal"), "WithoutKeyboardNavigation")
	assert.Empty(t, matches, "WithoutKeyboardNavigation should not exist in the codebase, found in: %v", matches)
}

// TestET_Phase21_TUI_AC9_NoHardcodedWithWidth80 verifies that interactive.go
// does not call WithWidth(80) with a hardcoded value.
func TestET_Phase21_TUI_AC9_NoHardcodedWithWidth80(t *testing.T) {
	root := moduleRoot(t)
	interactivePath := filepath.Join(root, "internal", "cli", "interactive.go")

	content, err := os.ReadFile(interactivePath)
	require.NoError(t, err, "failed to read interactive.go")

	assert.NotContains(t, string(content), "WithWidth(80)",
		"interactive.go should not call WithWidth(80) with a hardcoded value; should use dynamic width from TerminalSizeProvider")
}

// TestET_Phase21_TUI_AC10_GoroutineLeakCheck verifies that no goroutine leaks
// occur after running TUI operations.
func TestET_Phase21_TUI_AC10_GoroutineLeakCheck(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	events := make(chan tui.AgentEvent, 5)
	app := tui.NewBubbleteaApp(events)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx)
	}()

	// Send a few events to exercise the TUI.
	events <- tui.AgentEvent{Type: "message", Content: "hello", ContentType: tui.ContentTypeAssistant}
	events <- tui.AgentEvent{Type: "tool", Content: "read", ContentType: tui.ContentTypeToolCall}

	// Close the event channel to signal the render loop to stop.
	close(events)

	select {
	case <-done:
		// App exited normally.
	case <-time.After(5 * time.Second):
		app.Quit()
		t.Fatal("app did not stop within 5 seconds")
	}

	// Ensure the app has fully cleaned up.
	select {
	case <-app.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("app did not signal Done() within 2 seconds")
	}
}
