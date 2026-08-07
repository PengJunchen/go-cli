// Package tui implements the functional TUI layer. It provides a
// message-queue-driven render loop (BubbleteaApp), a set of content renderers
// and a theme system. The layer sits on top of the charmbracelet/bubbletea and
// lipgloss libraries; styling is delegated to lipgloss.Style and the theme
// presets express colors as truecolor (#RRGGBB) values.
//
// Design: the App consumes an event stream, dispatches
// each agent event to the renderer matching its content type via a
// thread-safe registry, and accumulates a view buffer exposed through View().
// The TUI layer itself does not emit tracing spans; render performance is
// recorded through slog.DebugContext and the consuming loop associates the
// event's TraceID when handling it.
package tui

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/charmbracelet/lipgloss"
)

// Style is an alias for lipgloss.Style so existing call sites
// (Foreground/Bold/Render/...) keep working with the charmbracelet stack.
type Style = lipgloss.Style

// NewStyle returns a lipgloss.Style with no attributes set.
func NewStyle() Style { return lipgloss.NewStyle() }

// Theme describes the set of named styles a renderer may use. The ten style
// accessors are the functional-equivalent of the lipgloss Style accessors that
// a Bubbletea theme would expose.
type Theme interface {
	Primary() Style
	Secondary() Style
	Success() Style
	Warning() Style
	Error() Style
	Bg() Style
	Fg() Style
	Faint() Style
	Bold() Style
	Italic() Style
}

// ThemePreset is a named collection of styles that can be hydrated into a
// Theme by a ThemeManager.
type ThemePreset struct {
	// Name is the stable identifier used to select the preset.
	Name string
	// Styles maps a theme slot name ("primary", "bg", ...) to its Style.
	Styles map[string]Style
}

// DarkTheme is the dark-background preset used on light text terminals. It uses
// an opencode-like purple accent over a muted slate foreground.
type DarkTheme struct{}

// compile-time assertions that the presets satisfy the Theme interface.
var (
	_ Theme = (*DarkTheme)(nil)
	_ Theme = (*LightTheme)(nil)
	_ Theme = (*MonokaiTheme)(nil)
	_ Theme = (*SolarizedTheme)(nil)
)

// Primary returns the accent style for dark mode (purple #7D56F4).
func (DarkTheme) Primary() Style { return NewStyle().Foreground(lipgloss.Color("#7D56F4")) }

// Secondary returns the muted accent style for dark mode (#6C7086).
func (DarkTheme) Secondary() Style { return NewStyle().Foreground(lipgloss.Color("#6C7086")) }

// Success returns the green success style for dark mode (#04E762).
func (DarkTheme) Success() Style { return NewStyle().Foreground(lipgloss.Color("#04E762")) }

// Warning returns the yellow warning style for dark mode (#FFC000).
func (DarkTheme) Warning() Style { return NewStyle().Foreground(lipgloss.Color("#FFC000")) }

// Error returns the red error style for dark mode (#FF5C5C).
func (DarkTheme) Error() Style { return NewStyle().Foreground(lipgloss.Color("#FF5C5C")) }

// Bg returns the default background style for dark mode (no background).
func (DarkTheme) Bg() Style { return NewStyle() }

// Fg returns the default foreground style for dark mode (#CDD6F4).
func (DarkTheme) Fg() Style { return NewStyle().Foreground(lipgloss.Color("#CDD6F4")) }

// Faint returns the faint/dim style for dark mode (#6C7086 + faint).
func (DarkTheme) Faint() Style {
	return NewStyle().Foreground(lipgloss.Color("#6C7086")).Faint(true)
}

// Bold returns the bold style for dark mode.
func (DarkTheme) Bold() Style { return NewStyle().Bold(true) }

// Italic returns the italic style for dark mode.
func (DarkTheme) Italic() Style { return NewStyle().Italic(true) }

// LightTheme is the light-background preset used on dark text terminals.
type LightTheme struct{}

// Primary returns the accent style for light mode (blue #1E66F5).
func (LightTheme) Primary() Style { return NewStyle().Foreground(lipgloss.Color("#1E66F5")) }

// Secondary returns the muted accent style for light mode (#6C6C6C).
func (LightTheme) Secondary() Style { return NewStyle().Foreground(lipgloss.Color("#6C6C6C")) }

// Success returns the green success style for light mode (#2E7D32).
func (LightTheme) Success() Style { return NewStyle().Foreground(lipgloss.Color("#2E7D32")) }

// Warning returns the yellow warning style for light mode (#F57C00).
func (LightTheme) Warning() Style { return NewStyle().Foreground(lipgloss.Color("#F57C00")) }

// Error returns the red error style for light mode (#C62828).
func (LightTheme) Error() Style { return NewStyle().Foreground(lipgloss.Color("#C62828")) }

// Bg returns the default background style for light mode (no background).
func (LightTheme) Bg() Style { return NewStyle() }

// Fg returns the default foreground style for light mode (#1A1A1A).
func (LightTheme) Fg() Style { return NewStyle().Foreground(lipgloss.Color("#1A1A1A")) }

// Faint returns the faint/dim style for light mode (#9E9E9E + faint).
func (LightTheme) Faint() Style {
	return NewStyle().Foreground(lipgloss.Color("#9E9E9E")).Faint(true)
}

// Bold returns the bold style for light mode.
func (LightTheme) Bold() Style { return NewStyle().Bold(true) }

// Italic returns the italic style for light mode.
func (LightTheme) Italic() Style { return NewStyle().Italic(true) }

// MonokaiTheme is a high-contrast preset inspired by the Monokai palette.
type MonokaiTheme struct{}

// Primary returns the pink accent style for Monokai (#F92672).
func (MonokaiTheme) Primary() Style { return NewStyle().Foreground(lipgloss.Color("#F92672")) }

// Secondary returns the purple accent style for Monokai (#AE81FF).
func (MonokaiTheme) Secondary() Style { return NewStyle().Foreground(lipgloss.Color("#AE81FF")) }

// Success returns the green success style for Monokai (#A6E22E).
func (MonokaiTheme) Success() Style { return NewStyle().Foreground(lipgloss.Color("#A6E22E")) }

// Warning returns the yellow warning style for Monokai (#FD971F).
func (MonokaiTheme) Warning() Style { return NewStyle().Foreground(lipgloss.Color("#FD971F")) }

// Error returns the red error style for Monokai (#F92672).
func (MonokaiTheme) Error() Style { return NewStyle().Foreground(lipgloss.Color("#F92672")) }

// Bg returns the dark background style for Monokai (#272822).
func (MonokaiTheme) Bg() Style { return NewStyle().Background(lipgloss.Color("#272822")) }

// Fg returns the light foreground style for Monokai (#F8F8F2).
func (MonokaiTheme) Fg() Style { return NewStyle().Foreground(lipgloss.Color("#F8F8F2")) }

// Faint returns the faint/dim style for Monokai (#75715E + faint).
func (MonokaiTheme) Faint() Style {
	return NewStyle().Foreground(lipgloss.Color("#75715E")).Faint(true)
}

// Bold returns the bold style for Monokai.
func (MonokaiTheme) Bold() Style { return NewStyle().Bold(true) }

// Italic returns the italic style for Monokai.
func (MonokaiTheme) Italic() Style { return NewStyle().Italic(true) }

// SolarizedTheme is a low-contrast preset inspired by the Solarized palette.
type SolarizedTheme struct{}

// Primary returns the blue accent style for Solarized (#268BD2).
func (SolarizedTheme) Primary() Style { return NewStyle().Foreground(lipgloss.Color("#268BD2")) }

// Secondary returns the cyan accent style for Solarized (#2AA198).
func (SolarizedTheme) Secondary() Style { return NewStyle().Foreground(lipgloss.Color("#2AA198")) }

// Success returns the green success style for Solarized (#859900).
func (SolarizedTheme) Success() Style { return NewStyle().Foreground(lipgloss.Color("#859900")) }

// Warning returns the yellow warning style for Solarized (#B58900).
func (SolarizedTheme) Warning() Style { return NewStyle().Foreground(lipgloss.Color("#B58900")) }

// Error returns the red error style for Solarized (#DC322F).
func (SolarizedTheme) Error() Style { return NewStyle().Foreground(lipgloss.Color("#DC322F")) }

// Bg returns the base background style for Solarized (#002B36).
func (SolarizedTheme) Bg() Style { return NewStyle().Background(lipgloss.Color("#002B36")) }

// Fg returns the base foreground style for Solarized (#93A1A1).
func (SolarizedTheme) Fg() Style { return NewStyle().Foreground(lipgloss.Color("#93A1A1")) }

// Faint returns the faint/dim style for Solarized (#586E75 + faint).
func (SolarizedTheme) Faint() Style {
	return NewStyle().Foreground(lipgloss.Color("#586E75")).Faint(true)
}

// Bold returns the bold style for Solarized.
func (SolarizedTheme) Bold() Style { return NewStyle().Bold(true) }

// Italic returns the italic style for Solarized.
func (SolarizedTheme) Italic() Style { return NewStyle().Italic(true) }

// defaultThemeRegistry builds the built-in theme presets keyed by name.
func defaultThemeRegistry() map[string]Theme {
	return map[string]Theme{
		"dark":      DarkTheme{},
		"light":     LightTheme{},
		"monokai":   MonokaiTheme{},
		"solarized": SolarizedTheme{},
	}
}

// ThemeManager owns the set of registered themes and the currently active one.
// It can switch themes by name and is safe for concurrent use.
type ThemeManager struct {
	mu      sync.RWMutex
	themes  map[string]Theme
	current Theme
}

// NewThemeManager returns a ThemeManager preloaded with the four built-in
// presets. The dark theme is active initially.
func NewThemeManager() *ThemeManager {
	manager := &ThemeManager{
		themes:  defaultThemeRegistry(),
		current: DarkTheme{},
	}
	slog.Debug("tui.theme.init", "themes", len(manager.themes))
	return manager
}

// Set switches the active theme to the named preset and reports whether the
// name was registered. It logs a debug event on success.
func (m *ThemeManager) Set(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	theme, ok := m.themes[name]
	if !ok {
		return fmt.Errorf("tui: unknown theme %q", name)
	}
	m.current = theme
	slog.Debug("tui.theme.set", "theme", name)
	return nil
}

// Get returns the currently active Theme.
func (m *ThemeManager) Get() Theme {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Register adds a theme under the given name and logs a debug event.
func (m *ThemeManager) Register(name string, theme Theme) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.themes[name] = theme
	slog.Debug("tui.theme.register", "theme", name)
}
