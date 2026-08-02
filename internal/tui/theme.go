// Package tui implements the functional TUI layer. It provides a
// message-queue-driven render loop (BubbleteaApp), a set of content renderers
// and a theme system. In a Bubbletea-based design these would sit on top of
// the charmbracelet/bubbletea and lipgloss libraries; this repository is
// deliberately zero-dependency, so the layer reimplements the functional
// subset (ANSI-styled rendering, content-type dispatch, theme switching) using
// only the standard library.
//
// Design (task 4-16/4-17/4-18): the App consumes an event stream, dispatches
// each agent event to the renderer matching its content type via a
// thread-safe registry, and accumulates a view buffer exposed through View().
// The TUI layer itself does not emit tracing spans; render performance is
// recorded through slog.DebugContext and the consuming loop associates the
// event's TraceID when handling it.
package tui

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// ANSI 4-bit color codes (standard + bright variants). These back the Style
// struct and let presets express foreground/background colors as small ints.
const (
	colorBlack = iota + colorBlackBase
	colorRed
	colorGreen
	colorYellow
	colorBlue
	colorMagenta
	colorCyan
	colorWhite
	colorBrightBlack = iota + colorBrightBase
	colorBrightRed
	colorBrightGreen
	colorBrightYellow
	colorBrightBlue
	colorBrightMagenta
	colorBrightCyan
	colorBrightWhite
)

// Base offsets for the standard and bright ANSI color ranges.
const (
	colorBlackBase  = 30
	colorBrightBase = 90
)

const (
	// ansiReset clears all active SGR attributes.
	ansiReset = "\x1b[0m"
	// styleAttrBold enables bold intensity.
	styleAttrBold = 1
	// styleAttrFaint enables faint/dim intensity.
	styleAttrFaint = 2
	// styleAttrItalic enables italic text.
	styleAttrItalic = 3
	// styleAttrUnderline enables underlined text.
	styleAttrUnderline = 4
	// noColor is the sentinel used to mark an unset color slot.
	noColor = -1
)

// Style is a functional-equivalent of lipgloss.Style from the charmbracelet
// ecosystem. Instead of depending on lipgloss (which the repository does not
// vendor), it stores the ANSI SGR attributes (foreground/background color
// codes plus bold/italic/faint/underline flags) and renders a string by
// wrapping it in the corresponding escape sequences.
type Style struct {
	fg        int
	bg        int
	bold      bool
	faint     bool
	italic    bool
	underline bool
}

// NewStyle returns a Style with every attribute unset.
func NewStyle() Style {
	return Style{fg: noColor, bg: noColor}
}

// Foreground returns a copy of the Style with the given 4-bit ANSI color code
// (colorBlack..colorBrightWhite) set as the foreground.
func (s Style) Foreground(code int) Style {
	s.fg = code
	return s
}

// Background returns a copy of the Style with the given 4-bit ANSI color code
// set as the background.
func (s Style) Background(code int) Style {
	s.bg = code
	return s
}

// Bold returns a copy of the Style with the bold attribute set to the value.
func (s Style) Bold(v bool) Style {
	s.bold = v
	return s
}

// Faint returns a copy of the Style with the faint attribute set to the value.
func (s Style) Faint(v bool) Style {
	s.faint = v
	return s
}

// Italic returns a copy of the Style with the italic attribute set.
func (s Style) Italic(v bool) Style {
	s.italic = v
	return s
}

// Underline returns a copy of the Style with the underline attribute set.
func (s Style) Underline(v bool) Style {
	s.underline = v
	return s
}

// codes aggregates the enabled SGR attribute codes in stable order.
func (s Style) codes() []int {
	codes := []int{}
	if s.fg != noColor {
		codes = append(codes, s.fg)
	}
	if s.bg != noColor {
		// Background SGR codes live 10 above the foreground codes.
		codes = append(codes, s.bg+10)
	}
	if s.bold {
		codes = append(codes, styleAttrBold)
	}
	if s.faint {
		codes = append(codes, styleAttrFaint)
	}
	if s.italic {
		codes = append(codes, styleAttrItalic)
	}
	if s.underline {
		codes = append(codes, styleAttrUnderline)
	}
	return codes
}

// String returns the raw SGR escape sequence that Render applies, without any
// payload or reset. It returns an empty string when the Style is empty.
func (s Style) String() string {
	codes := s.codes()
	if len(codes) == 0 {
		return ""
	}
	return "\x1b[" + joinInts(codes) + "m"
}

// Render wraps text in the Style's ANSI escape sequence and a trailing reset,
// returning the original text unchanged when the Style is empty.
func (s Style) Render(text string) string {
	codes := s.codes()
	if len(codes) == 0 {
		return text
	}
	return "\x1b[" + joinInts(codes) + "m" + text + ansiReset
}

// joinInts renders an int slice as a semicolon-separated string.
func joinInts(codes []int) string {
	parts := make([]string, 0, len(codes))
	for _, c := range codes {
		parts = append(parts, fmt.Sprintf("%d", c))
	}
	return strings.Join(parts, ";")
}

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

// DarkTheme is the dark-background preset used on light text terminals.
type DarkTheme struct{}

// compile-time assertions that the presets satisfy the Theme interface.
var (
	_ Theme = (*DarkTheme)(nil)
	_ Theme = (*LightTheme)(nil)
	_ Theme = (*MonokaiTheme)(nil)
	_ Theme = (*SolarizedTheme)(nil)
)

// Primary returns the accent style for dark mode.
func (DarkTheme) Primary() Style { return NewStyle().Foreground(colorBrightCyan) }

// Secondary returns the muted accent style for dark mode.
func (DarkTheme) Secondary() Style { return NewStyle().Foreground(colorBrightBlack) }

// Success returns the green success style for dark mode.
func (DarkTheme) Success() Style { return NewStyle().Foreground(colorGreen) }

// Warning returns the yellow warning style for dark mode.
func (DarkTheme) Warning() Style { return NewStyle().Foreground(colorYellow) }

// Error returns the red error style for dark mode.
func (DarkTheme) Error() Style { return NewStyle().Foreground(colorRed) }

// Bg returns the default background style for dark mode.
func (DarkTheme) Bg() Style { return NewStyle().Background(colorBlack) }

// Fg returns the default foreground style for dark mode.
func (DarkTheme) Fg() Style { return NewStyle().Foreground(colorWhite) }

// Faint returns the faint/dim style for dark mode.
func (DarkTheme) Faint() Style { return NewStyle().Foreground(colorBrightBlack).Faint(true) }

// Bold returns the bold style for dark mode.
func (DarkTheme) Bold() Style { return NewStyle().Bold(true) }

// Italic returns the italic style for dark mode.
func (DarkTheme) Italic() Style { return NewStyle().Italic(true) }

// LightTheme is the light-background preset used on dark text terminals.
type LightTheme struct{}

// Primary returns the accent style for light mode.
func (LightTheme) Primary() Style { return NewStyle().Foreground(colorBlue) }

// Secondary returns the muted accent style for light mode.
func (LightTheme) Secondary() Style { return NewStyle().Foreground(colorBrightBlack) }

// Success returns the green success style for light mode.
func (LightTheme) Success() Style { return NewStyle().Foreground(colorGreen) }

// Warning returns the yellow warning style for light mode.
func (LightTheme) Warning() Style { return NewStyle().Foreground(colorYellow) }

// Error returns the red error style for light mode.
func (LightTheme) Error() Style { return NewStyle().Foreground(colorRed) }

// Bg returns the default background style for light mode.
func (LightTheme) Bg() Style { return NewStyle().Background(colorWhite) }

// Fg returns the default foreground style for light mode.
func (LightTheme) Fg() Style { return NewStyle().Foreground(colorBlack) }

// Faint returns the faint/dim style for light mode.
func (LightTheme) Faint() Style { return NewStyle().Foreground(colorBrightBlack).Faint(true) }

// Bold returns the bold style for light mode.
func (LightTheme) Bold() Style { return NewStyle().Bold(true) }

// Italic returns the italic style for light mode.
func (LightTheme) Italic() Style { return NewStyle().Italic(true) }

// MonokaiTheme is a high-contrast preset inspired by the Monokai palette.
type MonokaiTheme struct{}

// Primary returns the orange accent style for Monokai.
func (MonokaiTheme) Primary() Style { return NewStyle().Foreground(colorBrightYellow) }

// Secondary returns the purple accent style for Monokai.
func (MonokaiTheme) Secondary() Style { return NewStyle().Foreground(colorMagenta) }

// Success returns the green success style for Monokai.
func (MonokaiTheme) Success() Style { return NewStyle().Foreground(colorGreen) }

// Warning returns the yellow warning style for Monokai.
func (MonokaiTheme) Warning() Style { return NewStyle().Foreground(colorYellow) }

// Error returns the red error style for Monokai.
func (MonokaiTheme) Error() Style { return NewStyle().Foreground(colorRed) }

// Bg returns the dark background style for Monokai.
func (MonokaiTheme) Bg() Style { return NewStyle().Background(colorBlack) }

// Fg returns the light foreground style for Monokai.
func (MonokaiTheme) Fg() Style { return NewStyle().Foreground(colorBrightWhite) }

// Faint returns the faint/dim style for Monokai.
func (MonokaiTheme) Faint() Style { return NewStyle().Foreground(colorBrightBlack).Faint(true) }

// Bold returns the bold style for Monokai.
func (MonokaiTheme) Bold() Style { return NewStyle().Bold(true) }

// Italic returns the italic style for Monokai.
func (MonokaiTheme) Italic() Style { return NewStyle().Italic(true) }

// SolarizedTheme is a low-contrast preset inspired by the Solarized palette.
type SolarizedTheme struct{}

// Primary returns the blue accent style for Solarized.
func (SolarizedTheme) Primary() Style { return NewStyle().Foreground(colorBrightBlue) }

// Secondary returns the cyan accent style for Solarized.
func (SolarizedTheme) Secondary() Style { return NewStyle().Foreground(colorCyan) }

// Success returns the green success style for Solarized.
func (SolarizedTheme) Success() Style { return NewStyle().Foreground(colorGreen) }

// Warning returns the yellow warning style for Solarized.
func (SolarizedTheme) Warning() Style { return NewStyle().Foreground(colorYellow) }

// Error returns the red error style for Solarized.
func (SolarizedTheme) Error() Style { return NewStyle().Foreground(colorRed) }

// Bg returns the base background style for Solarized.
func (SolarizedTheme) Bg() Style { return NewStyle().Background(colorBrightBlack) }

// Fg returns the base foreground style for Solarized.
func (SolarizedTheme) Fg() Style { return NewStyle().Foreground(colorWhite) }

// Faint returns the faint/dim style for Solarized.
func (SolarizedTheme) Faint() Style { return NewStyle().Foreground(colorBrightBlack).Faint(true) }

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
