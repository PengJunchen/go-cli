package tui

import (
	"context"
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
// every accessor so tests can assert that a renderer applied some styling.
type MockTheme struct{}

// Primary returns a fixed style.
func (MockTheme) Primary() Style { return NewStyle().Foreground(colorRed).Bold(true) }

// Secondary returns a fixed style.
func (MockTheme) Secondary() Style { return NewStyle().Foreground(colorMagenta) }

// Success returns a fixed style.
func (MockTheme) Success() Style { return NewStyle().Foreground(colorGreen) }

// Warning returns a fixed style.
func (MockTheme) Warning() Style { return NewStyle().Foreground(colorYellow) }

// Error returns a fixed style.
func (MockTheme) Error() Style { return NewStyle().Foreground(colorRed) }

// Bg returns a fixed style.
func (MockTheme) Bg() Style { return NewStyle().Background(colorBlack) }

// Fg returns a fixed style.
func (MockTheme) Fg() Style { return NewStyle().Foreground(colorWhite) }

// Faint returns a fixed style.
func (MockTheme) Faint() Style { return NewStyle().Foreground(colorBrightBlack).Faint(true) }

// Bold returns a fixed style.
func (MockTheme) Bold() Style { return NewStyle().Bold(true) }

// Italic returns a fixed style.
func (MockTheme) Italic() Style { return NewStyle().Italic(true) }
