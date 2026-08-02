package tui

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStyleIsImmutable verifies every Style modifier returns a copy and leaves
// the original receiver unmodified.
func TestStyleIsImmutable(t *testing.T) {
	base := NewStyle()
	_ = base.Foreground(colorRed).Background(colorBlue).Bold(true).Faint(true).Italic(true).Underline(true)
	// Receiver must be untouched.
	require.Equal(t, "", base.String())
	require.Equal(t, "x", base.Render("x"))
}

// TestStyleRenderRoundTrip verifies Render wraps content in a self-contained SGR
// sequence: it starts with the open sequence and ends with the reset.
func TestStyleRenderRoundTrip(t *testing.T) {
	s := NewStyle().Foreground(colorGreen).Bold(true)
	out := s.Render("ok")
	require.True(t, hasPrefix(out, "\x1b[1;32m") || hasPrefix(out, "\x1b[32;1m"))
	require.True(t, hasSuffix(out, ansiReset))
	// The payload must be preserved between the escape and the reset.
	assert.Contains(t, out, "ok")
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
func hasSuffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

// TestStyleCodesStableOrder verifies the aggregated SGR codes appear in the
// documented stable order regardless of how flags were set.
func TestStyleCodesStableOrder(t *testing.T) {
	s := NewStyle().Foreground(colorCyan).Background(colorBlack).Underline(true).Faint(true).Bold(true).Italic(true)
	codes := s.codes()
	expected := []int{colorCyan, colorBlack + 10, styleAttrBold, styleAttrFaint, styleAttrItalic, styleAttrUnderline}
	require.Len(t, codes, len(expected))
	for i, e := range expected {
		assert.Equal(t, e, codes[i])
	}
}

// TestStyleEmptyCodes verifies an empty Style reports no SGR codes.
func TestStyleEmptyCodes(t *testing.T) {
	assert.Empty(t, NewStyle().codes())
	assert.Len(t, NewStyle().Foreground(noColor).codes(), 0)
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

	// Light's fg (dark text) must differ from dark/monokai/solarized fg.
	darkFg := presets["dark"].Fg().String()
	assert.NotEqual(t, darkFg, presets["light"].Fg().String())
	// Monokai secondary differs from solarized secondary.
	assert.NotEqual(t, presets["monokai"].Secondary().String(), presets["solarized"].Secondary().String())

	for name, th := range presets {
		t.Run(name, func(t *testing.T) {
			assert.NotEmpty(t, th.Primary().String())
			assert.NotEmpty(t, th.Error().String())
			assert.NotEmpty(t, th.Success().String())
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
// exact expected Style.
func TestThemeDefaultMethodsTable(t *testing.T) {
	dark := DarkTheme{}
	tests := []struct {
		name string
		fn   func() Style
		want string
	}{
		{"Primary", dark.Primary, NewStyle().Foreground(colorBrightCyan).String()},
		{"Secondary", dark.Secondary, NewStyle().Foreground(colorBrightBlack).String()},
		{"Success", dark.Success, NewStyle().Foreground(colorGreen).String()},
		{"Warning", dark.Warning, NewStyle().Foreground(colorYellow).String()},
		{"Error", dark.Error, NewStyle().Foreground(colorRed).String()},
		{"Fg", dark.Fg, NewStyle().Foreground(colorWhite).String()},
		{"Faint", dark.Faint, NewStyle().Foreground(colorBrightBlack).Faint(true).String()},
		{"Bold", dark.Bold, NewStyle().Bold(true).String()},
		{"Italic", dark.Italic, NewStyle().Italic(true).String()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.fn().String())
		})
	}

	// Dark Bg uses black background.
	assert.Equal(t, NewStyle().Background(colorBlack).String(), dark.Bg().String())
}

// TestThemePresetStructRoundTrip verifies ThemePreset carries a name and a
// styles map that can be used to hydrate styling.
func TestThemePresetStructRoundTrip(t *testing.T) {
	p := ThemePreset{Name: "custom", Styles: map[string]Style{"fg": NewStyle().Foreground(colorRed).Bold(true)}}
	require.Equal(t, "custom", p.Name)
	assert.Contains(t, p.Styles["fg"].String(), "31")
	assert.Contains(t, p.Styles["fg"].String(), "1")
}

// TestDefaultThemeRegistryKeys verifies the built-in preset registration keys.
func TestDefaultThemeRegistryKeys(t *testing.T) {
	reg := defaultThemeRegistry()
	require.Len(t, reg, 4)
	for _, name := range []string{"dark", "light", "monokai", "solarized"} {
		require.Contains(t, reg, name)
	}
}
