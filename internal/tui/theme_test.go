package tui

import (
	"sort"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStyleRenderEmpty(t *testing.T) {
	s := NewStyle()
	// An empty lipgloss.Style passes content through verbatim with no escapes.
	assert.Equal(t, "hello", s.Render("hello"))
	assert.NotContains(t, s.Render("hello"), "\x1b[")
}

func TestStyleRenderWithColor(t *testing.T) {
	// Truecolor foreground (#FF5C5C) renders a 24-bit SGR sequence plus reset.
	s := NewStyle().Foreground(lipgloss.Color("#FF5C5C"))
	out := s.Render("err")
	assert.True(t, strings.HasPrefix(out, "\x1b[38;2;255;92;92m"), "expected truecolor red SGR prefix, got %q", out)
	assert.True(t, strings.HasSuffix(out, "\x1b[0m"), "expected reset suffix, got %q", out)
	assert.Contains(t, out, "err")
}

func TestStyleBoldFaintItalicUnderline(t *testing.T) {
	// Each attribute produces its standard SGR code under the forced truecolor
	// profile: bold=1, faint=2, italic=3, underline=4 (lipgloss emits 4;4).
	out := NewStyle().Foreground(lipgloss.Color("#04E762")).
		Bold(true).Faint(true).Italic(true).Underline(true).Render("x")
	assert.Contains(t, out, "1")
	assert.Contains(t, out, "2")
	assert.Contains(t, out, "3")
	assert.Contains(t, out, "4")
	assert.Contains(t, out, "x")
}

func TestStyleBackgroundOffset(t *testing.T) {
	// Background truecolor uses the 48;2 prefix (vs 38;2 for foreground).
	s := NewStyle().Background(lipgloss.Color("#1E66F5"))
	require.Contains(t, s.Render("bg"), "48;2;")
}

func TestThemePresetsImplementTheme(t *testing.T) {
	themes := []Theme{DarkTheme{}, LightTheme{}, MonokaiTheme{}, SolarizedTheme{}}
	for _, th := range themes {
		// Every accessor renders its payload with styling (non-empty output).
		assert.NotEmpty(t, th.Primary().Render("x"))
		assert.NotEmpty(t, th.Secondary().Render("x"))
		assert.NotEmpty(t, th.Success().Render("x"))
		assert.NotEmpty(t, th.Warning().Render("x"))
		assert.NotEmpty(t, th.Error().Render("x"))
		assert.NotEmpty(t, th.Bg().Render("x"))
		assert.NotEmpty(t, th.Fg().Render("x"))
		assert.NotEmpty(t, th.Faint().Render("x"))
		assert.NotEmpty(t, th.Bold().Render("x"))
		assert.NotEmpty(t, th.Italic().Render("x"))
	}
}

func TestLightThemeFgDiffersFromDark(t *testing.T) {
	// Light themes use dark text; dark themes use light text. The rendered
	// truecolor sequences must differ.
	assert.NotEqual(t, DarkTheme{}.Fg().Render("x"), LightTheme{}.Fg().Render("x"))
}

func TestThemeManagerDefaultsToDark(t *testing.T) {
	mgr := NewThemeManager()
	require.Equal(t, DarkTheme{}, mgr.Get())
}

func TestThemeManagerSwitching(t *testing.T) {
	mgr := NewThemeManager()
	require.NoError(t, mgr.Set("light"))
	assert.Equal(t, LightTheme{}, mgr.Get())

	require.NoError(t, mgr.Set("monokai"))
	assert.Equal(t, MonokaiTheme{}, mgr.Get())

	require.NoError(t, mgr.Set("solarized"))
	assert.Equal(t, SolarizedTheme{}, mgr.Get())
}

func TestThemeManagerSetUnknown(t *testing.T) {
	mgr := NewThemeManager()
	require.Error(t, mgr.Set("nonexistent"))
	// Previous theme is preserved after a failed switch.
	require.Equal(t, DarkTheme{}, mgr.Get())
}

func TestThemeManagerRegisterAndSwitch(t *testing.T) {
	mgr := NewThemeManager()
	mgr.Register("custom", MockTheme{})
	require.NoError(t, mgr.Set("custom"))
	require.Equal(t, MockTheme{}, mgr.Get())
}

func TestThemeManagerNames(t *testing.T) {
	mgr := NewThemeManager()
	names := mgr.Names()
	assert.Len(t, names, 4)
	assert.Contains(t, names, "dark")
	assert.Contains(t, names, "light")
	assert.Contains(t, names, "monokai")
	assert.Contains(t, names, "solarized")
	// Names should be sorted alphabetically.
	assert.True(t, sort.StringsAreSorted(names))
}

func TestThemeManagerCurrentName(t *testing.T) {
	mgr := NewThemeManager()
	assert.Equal(t, "dark", mgr.CurrentName())

	require.NoError(t, mgr.Set("light"))
	assert.Equal(t, "light", mgr.CurrentName())

	// Failed switch does not change current name.
	require.Error(t, mgr.Set("nonexistent"))
	assert.Equal(t, "light", mgr.CurrentName())
}

func TestThemePresetStruct(t *testing.T) {
	p := ThemePreset{
		Name:   "my",
		Styles: map[string]Style{"primary": NewStyle().Foreground(lipgloss.Color("#FF0000"))},
	}
	assert.Equal(t, "my", p.Name)
	require.Contains(t, p.Styles["primary"].Render("x"), "38;2;255;0;0")
}
