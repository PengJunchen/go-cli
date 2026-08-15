package tui

import (
	"os"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withEnvVars sets the given env vars, runs the test body, and restores the
// originals via t.Cleanup. An empty value unsets the variable.
func withEnvVars(t *testing.T, vars map[string]string) {
	t.Helper()
	for k, v := range vars {
		old, had := os.LookupEnv(k)
		if v == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, v)
		}
		key := k
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}

func TestResolveColorProfile_NoColor(t *testing.T) {
	withEnvVars(t, map[string]string{
		"NO_COLOR":       "1",
		"CLICOLOR":       "",
		"CLICOLOR_FORCE": "",
		"COLORTERM":      "",
	})
	got := resolveColorProfile(os.Stderr)
	assert.Equal(t, termenv.Ascii, got, "NO_COLOR=1 should produce Ascii profile")
}

func TestResolveColorProfile_NoColorAnyValue(t *testing.T) {
	withEnvVars(t, map[string]string{
		"NO_COLOR":       "anything",
		"CLICOLOR_FORCE": "1",
		"COLORTERM":      "truecolor",
	})
	got := resolveColorProfile(os.Stderr)
	assert.Equal(t, termenv.Ascii, got,
		"NO_COLOR set to any value should produce Ascii, even with CLICOLOR_FORCE")
}

func TestResolveColorProfile_ClicolorZero(t *testing.T) {
	withEnvVars(t, map[string]string{
		"NO_COLOR":       "",
		"CLICOLOR":       "0",
		"CLICOLOR_FORCE": "",
		"COLORTERM":      "",
	})
	got := resolveColorProfile(os.Stderr)
	assert.Equal(t, termenv.Ascii, got, "CLICOLOR=0 should produce Ascii profile")
}

func TestResolveColorProfile_ClicolorForce(t *testing.T) {
	withEnvVars(t, map[string]string{
		"NO_COLOR":       "",
		"CLICOLOR":       "",
		"CLICOLOR_FORCE": "1",
		"COLORTERM":      "truecolor",
	})
	got := resolveColorProfile(os.Stderr)
	assert.Equal(t, termenv.TrueColor, got,
		"CLICOLOR_FORCE=1 with COLORTERM=truecolor should produce TrueColor")
}

func TestResolveColorProfile_ClicolorZeroForced(t *testing.T) {
	withEnvVars(t, map[string]string{
		"NO_COLOR":       "",
		"CLICOLOR":       "0",
		"CLICOLOR_FORCE": "1",
		"COLORTERM":      "truecolor",
	})
	got := resolveColorProfile(os.Stderr)
	assert.Equal(t, termenv.TrueColor, got,
		"CLICOLOR_FORCE should override CLICOLOR=0")
}

func TestResolveColorProfile_NoEnvVarsNonTTY(t *testing.T) {
	withEnvVars(t, map[string]string{
		"NO_COLOR":       "",
		"CLICOLOR":       "",
		"CLICOLOR_FORCE": "",
		"COLORTERM":      "",
	})
	got := resolveColorProfile(os.Stderr)
	assert.Equal(t, termenv.Ascii, got,
		"no env vars on a non-TTY stream should produce Ascii")
}

// TestNoColorSuppressesANSI verifies the end-to-end behavior: when NO_COLOR=1
// is set, lipgloss rendering produces no ANSI escape sequences.
func TestNoColorSuppressesANSI(t *testing.T) {
	withEnvVars(t, map[string]string{
		"NO_COLOR":       "1",
		"CLICOLOR":       "",
		"CLICOLOR_FORCE": "",
		"COLORTERM":      "",
	})

	profile := resolveColorProfile(os.Stderr)
	require.Equal(t, termenv.Ascii, profile)

	lipgloss.SetColorProfile(profile)
	t.Cleanup(func() {
		// Restore the TrueColor profile expected by other tests.
		lipgloss.SetColorProfile(termenv.TrueColor)
	})

	styled := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#FF0000")).
		Bold(true).
		Render("hello")

	assert.NotContains(t, styled, "\x1b[",
		"NO_COLOR=1 should suppress all ANSI escape sequences")
	assert.Equal(t, "hello", styled,
		"NO_COLOR=1 should produce plain text with no styling")
}

// TestClicolorForceProducesANSI verifies that CLICOLOR_FORCE=1 with
// COLORTERM=truecolor produces colored output even on a non-TTY stream.
func TestClicolorForceProducesANSI(t *testing.T) {
	withEnvVars(t, map[string]string{
		"NO_COLOR":       "",
		"CLICOLOR":       "",
		"CLICOLOR_FORCE": "1",
		"COLORTERM":      "truecolor",
	})

	profile := resolveColorProfile(os.Stderr)
	require.Equal(t, termenv.TrueColor, profile)

	lipgloss.SetColorProfile(profile)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(termenv.TrueColor)
	})

	styled := lipgloss.NewStyle().
		Foreground(lipgloss.Color("#FF0000")).
		Render("hello")

	assert.Contains(t, styled, "\x1b[",
		"CLICOLOR_FORCE=1 should produce ANSI escape sequences")
	assert.Contains(t, styled, "hello",
		"styled output should contain the original text")
}
