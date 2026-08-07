package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpinnerHasTenFrames(t *testing.T) {
	s := NewSpinnerRenderer()
	require.Len(t, s.frames, 10)
}

func TestSpinnerFramesAreDifferent(t *testing.T) {
	s := NewSpinnerRenderer()
	seen := make(map[string]bool, len(s.frames))
	for i, f := range s.frames {
		assert.False(t, seen[f], "frame %d is a duplicate of an earlier frame", i)
		seen[f] = true
	}
}

func TestSpinnerFramesAreBraille(t *testing.T) {
	s := NewSpinnerRenderer()
	expected := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	for i, want := range expected {
		assert.Equal(t, want, s.frames[i], "frame %d mismatch", i)
	}
}

func TestSpinnerRenderFrame(t *testing.T) {
	s := NewSpinnerRenderer()
	out := s.RenderFrame(0)
	// The output should contain the first braille glyph, wrapped in ANSI.
	assert.Contains(t, out, "⠋")
	assert.Contains(t, out, "\x1b[") // styled with primary color
}

func TestSpinnerFrameWrapping(t *testing.T) {
	s := NewSpinnerRenderer()
	// frame 10 wraps to frame 0, frame 11 wraps to frame 1, etc.
	for i := 0; i < len(s.frames); i++ {
		assert.Equal(t, s.RenderFrame(i), s.RenderFrame(i+len(s.frames)),
			"frame %d should equal frame %d", i, i+len(s.frames))
	}
	// Larger multiples also wrap correctly.
	assert.Equal(t, s.RenderFrame(0), s.RenderFrame(20))
	assert.Equal(t, s.RenderFrame(3), s.RenderFrame(23))
}

func TestSpinnerSetTheme(t *testing.T) {
	s := NewSpinnerRenderer()
	s.SetTheme(MockTheme{})
	out := s.RenderFrame(0)
	// MockTheme Primary is red+bold truecolor: 1;38;2;255;0;0
	assert.Contains(t, out, "1;38;2;255;0;0")
}

func TestSpinnerNotARenderer(t *testing.T) {
	// SpinnerRenderer must NOT implement the Renderer interface.
	s := NewSpinnerRenderer()
	_, ok := interface{}(s).(Renderer)
	assert.False(t, ok, "SpinnerRenderer must not implement Renderer")
}
