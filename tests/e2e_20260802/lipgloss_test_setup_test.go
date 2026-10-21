//go:build !headless

package e2e_20260802 //nolint:staticcheck

import (
	"os"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestMain forces the lipgloss color profile to termenv.TrueColor for the whole
// test binary. Without this, tests run without a TTY and lipgloss would detect
// an ASCII/no-color profile, emitting plain text with no ANSI escape sequences.
// Pinning truecolor makes rendered output deterministic so style assertions can
// rely on stable 24-bit SGR codes (e.g. "\x1b[38;2;R;G;Bm" for foreground colors)
// while bold/italic/underline/faint keep their standard SGR codes (1/3/4/2).
//
// NO_COLOR is unset and CLICOLOR_FORCE, COLORTERM, and TERM are pinned so that
// resolveColorProfile(os.Stderr) — called by NewBubbleteaApp — also resolves to
// TrueColor even though stderr is not a TTY in the test environment. The
// application's NO_COLOR behavior is covered explicitly in internal/tui.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("NO_COLOR")
	os.Setenv("CLICOLOR_FORCE", "1")
	os.Setenv("COLORTERM", "truecolor")
	os.Setenv("TERM", "xterm-256color")
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}
