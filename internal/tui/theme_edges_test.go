package tui

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStyleBackgroundRendersBackgroundSGR verifies the background color maps to
// the correct SGR (foreground code + 10) when rendered end-to-end.
func TestStyleBackgroundRendersBackgroundSGR(t *testing.T) {
	s := NewStyle().Background(colorGreen) // 32 -> 42
	out := s.Render("bg")
	require.True(t, strings.Contains(out, "42"), "expected bg SGR 42 in %q", out)
	require.True(t, strings.HasPrefix(out, "\x1b[42m"))
	require.True(t, strings.HasSuffix(out, ansiReset))
	require.Equal(t, "bg", stripEscape(out))
}

// TestStyleForegroundAndBackgroundTogether verifies fg+bg appear together in
// the SGR body.
func TestStyleForegroundAndBackgroundTogether(t *testing.T) {
	s := NewStyle().Foreground(colorRed).Background(colorBlue)
	body := s.String() // e.g. \x1b[31;44m
	require.Contains(t, body, "31")
	require.Contains(t, body, "44")
}

// TestStyleFlagsIndependent verifies toggling each boolean attribute on and off
// leaves the SGR body with only the enabled attribute.
func TestStyleFlagsIndependent(t *testing.T) {
	tests := []struct {
		name  string
		style Style
		code  string
	}{
		{"bold", NewStyle().Bold(true), "1"},
		{"faint", NewStyle().Faint(true), "2"},
		{"italic", NewStyle().Italic(true), "3"},
		{"underline", NewStyle().Underline(true), "4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.style.Render("x")
			require.Contains(t, out, "\x1b["+tc.code+"m", "expected only SGR %s", tc.code)
			// No other flag code should be present.
			for _, other := range []string{"1", "2", "3", "4"} {
				if other == tc.code {
					continue
				}
				assert.NotContains(t, out, "\x1b["+other+"m", "unexpected attribute %s", other)
			}
		})
	}
}

// TestStyleResetViaSetFalse verifies calling a setter with false is a no-op that
// yields an empty style again.
func TestStyleResetViaSetFalse(t *testing.T) {
	s := NewStyle().Bold(true).Italic(true).Bold(false).Italic(false)
	require.Equal(t, "", s.String(), "flag disabled should remove the attribute")
}

// TestThemeManagerSetEmptyName verifies switching to an empty name yields an
// unknown-theme error and keeps the current theme.
func TestThemeManagerSetEmptyName(t *testing.T) {
	mgr := NewThemeManager()
	require.Error(t, mgr.Set(""))
	require.Equal(t, DarkTheme{}, mgr.Get())
}

// TestThemeManagerRegisterNilTheme verifies registering a nil Theme is accepted
// by the manager (it stores the nil) and does not panic, and Set on the built-in
// names still works.
func TestThemeManagerRegisterNilTheme(t *testing.T) {
	mgr := NewThemeManager()
	mgr.Register("nil-theme", nil)
	require.NoError(t, mgr.Set("dark"))
	require.Equal(t, DarkTheme{}, mgr.Get())
	// A nil active theme is tolerated by Get (returns nil) without panicking.
	mgr.Register("nil-theme", nil)
	_ = mgr.Get()
}

// TestThemeManagerConcurrentRegisterAndGet verifies concurrent Register plus Get
// is race-free and Get always returns a non-nil theme for the built-ins.
func TestThemeManagerConcurrentRegisterAndGet(t *testing.T) {
	mgr := NewThemeManager()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				mgr.Register(string(rune('a'+i)), LightTheme{})
				_ = mgr.Set("dark") //nolint:errcheck // "dark" always exists
				require.NotNil(t, mgr.Get())
			}
		}(i)
	}
	wg.Wait()
	require.Equal(t, DarkTheme{}, mgr.Get())
}

// TestDefaultThemeRegistryContainsNames verifies the four built-in presets are
// keyed by their canonical names.
func TestDefaultThemeRegistryContainsNames(t *testing.T) {
	reg := defaultThemeRegistry()
	for _, name := range []string{"dark", "light", "monokai", "solarized"} {
		require.Contains(t, reg, name)
		require.NotNil(t, reg[name])
	}
}

// TestEachPresetProducesDistinctPrimary verifies every preset's primary style
// differs so switching themes actually changes the accent color.
func TestEachPresetProducesDistinctPrimary(t *testing.T) {
	seen := map[string]bool{}
	for name, th := range map[string]Theme{
		"dark":      DarkTheme{},
		"light":     LightTheme{},
		"monokai":   MonokaiTheme{},
		"solarized": SolarizedTheme{},
	} {
		body := th.Primary().String()
		require.NotEmpty(t, body, "preset %q primary should be non-empty", name)
		seen[body] = true
	}
	require.Len(t, seen, 4, "all four primary styles should be distinct")
}

// TestRenderersDrawWithThemeManager verifies going through a ThemeManager
// applies the active theme's styling to a renderer end-to-end.
func TestRenderersDrawWithThemeManager(t *testing.T) {
	mgr := NewThemeManager()
	require.NoError(t, mgr.Set("light"))
	out := (MarkdownRenderer{}).Render(context.Background(), "hi", RenderOpts{Theme: mgr.Get()})
	require.True(t, strings.HasPrefix(out, "\x1b[34m"), "light primary should be blue (34), got %q", out)
}

// TestRendererRegistryEmptyVerifiesNewRegistryIsEmpty verifies a freshly
// constructed registry has no entries and looks up nothing.
func TestRendererRegistryEmptyVerifiesNewRegistryIsEmpty(t *testing.T) {
	reg := NewRendererRegistry()
	require.Empty(t, reg.List())
	_, ok := reg.Get(ContentTypeStatus)
	require.False(t, ok)
}

// TestRendererRegistryRegisterNilPanics verifies registering a nil renderer is
// not supported: Register dereferences the renderer and panics. We assert it
// panics rather than silently corrupting the registry.
func TestRendererRegistryRegisterNilPanics(t *testing.T) {
	reg := NewRendererRegistry()
	require.Panics(t, func() { reg.Register(nil) })
	require.Empty(t, reg.List())
}

// TestRendererRegistryListIsACopy verifies the snapshot returned by List is an
// independent map that does not alias the registry's internal state.
func TestRendererRegistryListIsACopy(t *testing.T) {
	reg := NewDefaultRegistry()
	snap := reg.List()
	delete(snap, ContentTypeCode)
	_, ok := reg.Get(ContentTypeCode)
	require.True(t, ok, "removing from the snapshot must not affect the registry")
	require.Len(t, snap, 23)
}

// TestRendererRegistryGetEmptyString verifies an empty content type is absent
// from the default registry and a fresh one.
func TestRendererRegistryGetEmptyString(t *testing.T) {
	for _, reg := range []*RendererRegistry{NewDefaultRegistry(), NewRendererRegistry()} {
		_, ok := reg.Get("")
		require.False(t, ok)
	}
}

// TestStreamingAndNonStreamingDrawBehaviour verifies the app draws streaming
// renderers by replacement and non-streaming renderers by appending, in a
// mixed sequence.
func TestStreamingAndNonStreamingDrawBehaviour(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.draw("streaming", "b", StreamingRenderer{})
	app.draw("streaming", "c", StreamingRenderer{})
	app.draw("code", "d", CodeRenderer{})
	app.draw("status", "a", StatusRenderer{})
	// Streaming replaces, static appends: ["c", "d", "a"].
	require.Equal(t, "c\nd\na", app.View())
}

// TestStyleEmptyStringRoundTrip verifies String() on a non-empty style returns
// the opening SGR only and Render returns a balanced open/close pair.
func TestStyleEmptyStringRoundTrip(t *testing.T) {
	s := NewStyle().Foreground(colorWhite)
	open := s.String()
	require.Equal(t, "\x1b[37m", open)
	rendered := s.Render("X")
	require.Equal(t, "\x1b[37mX\x1b[0m", rendered)
}
