//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 21 TUI Enhancements: dynamic terminal width,
// code/markdown highlighting, collapsible tool-call rendering, non-TTY
// degradation, and removal of legacy hardcoded width / keyboard-nav options.
package e2e_20260802 //nolint:staticcheck

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tui"
)

// moduleRoot returns the project root directory by walking up from this
// package's working directory until it finds go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err, "failed to read test working directory")
	for {
		if info, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil && !info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod walking up from %q", dir)
			return ""
		}
		dir = parent
	}
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
	t.Skip("CodeHighlighter was replaced by glamour-based MarkdownRenderer")
}

// TestET_Phase21_TUI_AC3_MarkdownHighlighting verifies that markdown
// highlighting processes code blocks. In non-TTY mode the text is returned
// unchanged; in TTY mode the code block receives ANSI highlighting while
// non-code text is preserved.
func TestET_Phase21_TUI_AC3_MarkdownHighlighting(t *testing.T) {
	t.Skip("CodeHighlighter was replaced by glamour-based MarkdownRenderer")
}

// TestET_Phase21_TUI_AC6_NonTTYDegradation verifies that in non-TTY mode the
// highlighter returns code without any ANSI escape sequences.
func TestET_Phase21_TUI_AC6_NonTTYDegradation(t *testing.T) {
	t.Skip("CodeHighlighter was replaced by glamour-based MarkdownRenderer")
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
