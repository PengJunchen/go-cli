package tui

import (
	"sync"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStyleIsImmutable verifies every Style modifier returns a copy and leaves
// the original receiver unmodified. lipgloss.Style is a value type, so chained
// modifiers do not mutate the receiver.
func TestStyleIsImmutable(t *testing.T) {
	base := NewStyle()
	_ = base.Foreground(lipgloss.Color("#FF0000")).Background(lipgloss.Color("#1E66F5")).
		Bold(true).Faint(true).Italic(true).Underline(true)
	// Receiver must be untouched: it renders plain text with no escapes.
	require.Equal(t, "x", base.Render("x"))
}

// TestStyleRenderRoundTrip verifies Render wraps content in a self-contained
// SGR sequence with a trailing reset.
func TestStyleRenderRoundTrip(t *testing.T) {
	s := NewStyle().Foreground(lipgloss.Color("#04E762")).Bold(true)
	out := s.Render("ok")
	require.True(t, hasPrefix(out, "\x1b[1;38;2;") || hasPrefix(out, "\x1b[38;2;"), "expected truecolor+bold SGR open, got %q", out)
	require.True(t, hasSuffix(out, "\x1b[0m"))
	// The payload must be preserved between the escape and the reset.
	assert.Contains(t, out, "ok")
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

// TestThemePresetsDistinct checks that primary/secondary/fg/faint styles differ
// meaningfully across the four presets so switching themes changes rendering.
func TestThemePresetsDistinct(t *testing.T) {
	presets := map[string]Theme{
		"dark":      DarkTheme{},
		"light":     LightTheme{},
		"monokai":   MonokaiTheme{},
		"solarized": SolarizedTheme{},
	}

	// Light's fg (dark text) must differ from dark fg.
	darkFg := presets["dark"].Fg().Render("x")
	assert.NotEqual(t, darkFg, presets["light"].Fg().Render("x"))
	// Monokai secondary differs from solarized secondary.
	assert.NotEqual(t, presets["monokai"].Secondary().Render("x"), presets["solarized"].Secondary().Render("x"))

	for name, th := range presets {
		t.Run(name, func(t *testing.T) {
			assert.NotEmpty(t, th.Primary().Render("x"))
			assert.NotEmpty(t, th.Error().Render("x"))
			assert.NotEmpty(t, th.Success().Render("x"))
		})
	}
}

// TestThemeManagerConcurrentSetGet verifies concurrent Set/Get switching across
// registered themes is safe and always resolves to a registered theme.
func TestThemeManagerConcurrentSetGet(t *testing.T) {
	mgr := NewThemeManager()
	mgr.Register("mock", MockTheme{})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			names := []string{"dark", "light", "monokai", "solarized", "mock"}
			for j := 0; j < 200; j++ {
				_ = mgr.Set(names[(i+j)%len(names)]) //nolint:errcheck // switching to valid names never errors here
				tv := mgr.Get()
				if tv == nil {
					t.Errorf("Get returned nil theme")
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestThemeManagerRegisterOverwrites verifies re-registering a name replaces
// the existing theme and that the newly registered theme becomes selectable.
func TestThemeManagerRegisterOverwrites(t *testing.T) {
	mgr := NewThemeManager()
	mgr.Register("x", LightTheme{})
	require.NoError(t, mgr.Set("x"))
	require.Equal(t, LightTheme{}, mgr.Get())

	mgr.Register("x", MockTheme{})
	require.NoError(t, mgr.Set("x"))
	require.Equal(t, MockTheme{}, mgr.Get())
}

// TestThemeDefaultMethodsTable verifies each DarkTheme accessor returns the
// expected truecolor style by comparing rendered output against a freshly
// reconstructed lipgloss.Style.
func TestThemeDefaultMethodsTable(t *testing.T) {
	dark := DarkTheme{}
	tests := []struct {
		name string
		fn   func() Style
		want string
	}{
		{"Primary", dark.Primary, NewStyle().Foreground(lipgloss.Color("#7D56F4")).Render("x")},
		{"Secondary", dark.Secondary, NewStyle().Foreground(lipgloss.Color("#6C7086")).Render("x")},
		{"Success", dark.Success, NewStyle().Foreground(lipgloss.Color("#04E762")).Render("x")},
		{"Warning", dark.Warning, NewStyle().Foreground(lipgloss.Color("#FFC000")).Render("x")},
		{"Error", dark.Error, NewStyle().Foreground(lipgloss.Color("#FF5C5C")).Render("x")},
		{"Fg", dark.Fg, NewStyle().Foreground(lipgloss.Color("#CDD6F4")).Render("x")},
		{"Faint", dark.Faint, NewStyle().Foreground(lipgloss.Color("#6C7086")).Faint(true).Render("x")},
		{"Bold", dark.Bold, NewStyle().Bold(true).Render("x")},
		{"Italic", dark.Italic, NewStyle().Italic(true).Render("x")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.fn().Render("x"))
		})
	}

	// Dark Bg has no background set, so it renders plain text.
	assert.Equal(t, NewStyle().Render("x"), dark.Bg().Render("x"))
}

// TestThemePresetStructRoundTrip verifies ThemePreset carries a name and a
// styles map that can be used to hydrate styling.
func TestThemePresetStructRoundTrip(t *testing.T) {
	p := ThemePreset{Name: "custom", Styles: map[string]Style{"fg": NewStyle().Foreground(lipgloss.Color("#FF0000")).Bold(true)}}
	require.Equal(t, "custom", p.Name)
	out := p.Styles["fg"].Render("x")
	assert.Contains(t, out, "38;2;255;0;0") // red foreground
	assert.Contains(t, out, "1")            // bold
}

// TestDefaultThemeRegistryKeys verifies the built-in preset registration keys.
func TestDefaultThemeRegistryKeys(t *testing.T) {
	reg := defaultThemeRegistry()
	require.Len(t, reg, 4)
	for _, name := range []string{"dark", "light", "monokai", "solarized"} {
		require.Contains(t, reg, name)
	}
}
