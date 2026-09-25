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
//
// CLICOLOR_FORCE=1 and COLORTERM=truecolor are set so that
// resolveColorProfile(os.Stderr) — called by NewBubbleteaApp — also resolves to
// TrueColor even though stderr is not a TTY in the test environment.
func TestMain(m *testing.M) {
	type envState struct {
		key    string
		value  string
		exists bool
	}
	vars := []envState{}
	for _, key := range []string{"NO_COLOR", "CLICOLOR_FORCE", "COLORTERM", "TERM"} {
		value, exists := os.LookupEnv(key)
		vars = append(vars, envState{key: key, value: value, exists: exists})
	}
	// ANSI-rendering contract tests run with a stable color profile regardless
	// of the host shell's NO_COLOR or TERM. Product NO_COLOR behavior is covered
	// by an explicit test that sets the variable itself.
	_ = os.Unsetenv("NO_COLOR")
	os.Setenv("CLICOLOR_FORCE", "1")
	os.Setenv("COLORTERM", "truecolor")
	os.Setenv("TERM", "xterm-256color")
	lipgloss.SetColorProfile(termenv.TrueColor)
	code := m.Run()
	for _, state := range vars {
		if state.exists {
			_ = os.Setenv(state.key, state.value)
		} else {
			_ = os.Unsetenv(state.key)
		}
	}
	os.Exit(code)
}
