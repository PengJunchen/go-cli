package tui

import (
	"context"

	"github.com/charmbracelet/lipgloss"
)

// MockRenderer is a test double that returns a fixed result for every render
// call. It is defined in a test file only, so it never ships in production
// binaries. No build tag is required because there is no import cycle; the mock
// types live in the same package as the code under test.
type MockRenderer struct {
	fixedName string
	fixedType string
	fixedOut  string
}

// NewMockRenderer returns a MockRenderer that Supports the given content type
// and always renders fixedOut.
func NewMockRenderer(name, contentType, fixedOut string) MockRenderer {
	return MockRenderer{fixedName: name, fixedType: contentType, fixedOut: fixedOut}
}

// Render returns the fixed output.
func (m MockRenderer) Render(context.Context, string, RenderOpts) string { return m.fixedOut }

// Name returns the configured renderer name.
func (m MockRenderer) Name() string { return m.fixedName }

// Supports reports whether the content type matches the configured one.
func (m MockRenderer) Supports(ct string) bool { return ct == m.fixedType }

// MockTheme is a test double that returns a fixed style (force red, bold) for
// every accessor so tests can assert that a renderer applied some styling. It
// uses truecolor hex values via lipgloss.Color, mirroring the production
// presets.
type MockTheme struct{}

// Primary returns a fixed style (red + bold).
func (MockTheme) Primary() Style {
	return NewStyle().Foreground(lipgloss.Color("#FF0000")).Bold(true)
}

// Secondary returns a fixed style (magenta).
func (MockTheme) Secondary() Style { return NewStyle().Foreground(lipgloss.Color("#FF00FF")) }

// Success returns a fixed style (green).
func (MockTheme) Success() Style { return NewStyle().Foreground(lipgloss.Color("#00FF00")) }

// Warning returns a fixed style (yellow).
func (MockTheme) Warning() Style { return NewStyle().Foreground(lipgloss.Color("#FFFF00")) }

// Error returns a fixed style (red).
func (MockTheme) Error() Style { return NewStyle().Foreground(lipgloss.Color("#FF0000")) }

// Bg returns a fixed style (black background).
func (MockTheme) Bg() Style { return NewStyle().Background(lipgloss.Color("#000000")) }

// Fg returns a fixed style (white).
func (MockTheme) Fg() Style { return NewStyle().Foreground(lipgloss.Color("#FFFFFF")) }

// Faint returns a fixed style (bright black + faint).
func (MockTheme) Faint() Style {
	return NewStyle().Foreground(lipgloss.Color("#808080")).Faint(true)
}

// Bold returns a fixed style (bold).
func (MockTheme) Bold() Style { return NewStyle().Bold(true) }

// Italic returns a fixed style (italic).
func (MockTheme) Italic() Style { return NewStyle().Italic(true) }
