package tui

import (
	"os"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestMain forces the lipgloss color profile to termenv.TrueColor for the whole
// test binary. Without this, tests run without a TTY and lipgloss would detect
// an ASCII/no-color profile (profile value 3 on this host), emitting plain text
// with no ANSI escape sequences. Pinning truecolor makes rendered output
// deterministic so style assertions can rely on stable 24-bit SGR codes (e.g.
// "\x1b[38;2;R;G;Bm" for foreground colors) while bold/italic/underline/
// strikethrough/faint keep their standard SGR codes (1/3/4/9/2).
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}
