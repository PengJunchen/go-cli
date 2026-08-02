package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStyleRenderEmpty(t *testing.T) {
	s := NewStyle()
	assert.Equal(t, "hello", s.Render("hello"))
	assert.Equal(t, "", s.String())
}

func TestStyleRenderWithColor(t *testing.T) {
	s := NewStyle().Foreground(colorRed)
	out := s.Render("err")
	assert.True(t, strings.HasPrefix(out, "\x1b[31m"), "expected red SGR prefix, got %q", out)
	assert.True(t, strings.HasSuffix(out, ansiReset), "expected reset suffix, got %q", out)

	st := s.String()
	require.Contains(t, st, "31")
	assert.False(t, strings.HasSuffix(st, ansiReset))
}

func TestStyleBoldFaintItalicUnderline(t *testing.T) {
	s := NewStyle().Foreground(colorGreen).Bold(true).Faint(true).Italic(true).Underline(true)
	st := s.String()
	for _, code := range []string{"32", "1", "2", "3", "4"} {
		require.Contains(t, st, code)
	}
}

func TestStyleBackgroundOffset(t *testing.T) {
	s := NewStyle().Background(colorBlue)
	// Background adds 10 above the foreground code (34 -> 44).
	require.Contains(t, s.String(), "44")
}

func TestThemePresetsImplementTheme(t *testing.T) {
	themes := []Theme{DarkTheme{}, LightTheme{}, MonokaiTheme{}, SolarizedTheme{}}
	for _, th := range themes {
		assert.NotEmpty(t, th.Primary().String())
		assert.NotEmpty(t, th.Secondary().String())
		assert.NotEmpty(t, th.Success().String())
		assert.NotEmpty(t, th.Warning().String())
		assert.NotEmpty(t, th.Error().String())
		assert.NotEmpty(t, th.Bg().String())
		assert.NotEmpty(t, th.Fg().String())
		assert.NotEmpty(t, th.Faint().String())
		assert.NotEmpty(t, th.Bold().String())
		assert.NotEmpty(t, th.Italic().String())
	}
}

func TestLightThemeFgDiffersFromDark(t *testing.T) {
	// Light themes use dark text; dark themes use light text.
	assert.Contains(t, DarkTheme{}.Fg().String(), "37")
	assert.Contains(t, LightTheme{}.Fg().String(), "30")
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

func TestThemePresetStruct(t *testing.T) {
	p := ThemePreset{
		Name:   "my",
		Styles: map[string]Style{"primary": NewStyle().Foreground(colorRed)},
	}
	assert.Equal(t, "my", p.Name)
	require.Contains(t, p.Styles["primary"].String(), "31")
}
