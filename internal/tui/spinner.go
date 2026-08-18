package tui

// SpinnerRenderer is a standalone component (NOT a Renderer interface
// implementation) because animation frames are driven by a timer, not by
// events. Callers advance the frame index and invoke RenderFrame to produce the
// current spinner glyph styled with the theme primary color.
type SpinnerRenderer struct {
	frames []string
	theme  Theme
}

// NewSpinnerRenderer returns a SpinnerRenderer with the standard 10-frame
// braille animation. The default theme is DarkTheme; use SetTheme to change it.
func NewSpinnerRenderer() *SpinnerRenderer {
	return &SpinnerRenderer{
		frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		theme:  DarkTheme{},
	}
}

// SetTheme sets the theme used to style spinner frames, allowing callers (e.g.
// app.go) to sync the spinner with the active theme.
func (r *SpinnerRenderer) SetTheme(theme Theme) {
	r.theme = theme
}

// RenderFrame returns the spinner glyph for the given frame index, wrapping
// modulo the frame count. The glyph is styled with the theme primary color.
func (r *SpinnerRenderer) RenderFrame(frame int) string {
	glyph := r.frames[frame%len(r.frames)]
	return r.theme.Primary().Render(glyph)
}
