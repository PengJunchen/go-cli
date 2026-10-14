package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

func TestFollowUpUpdateAndViewPreserveSpace(t *testing.T) {
	submitted := make(chan string, 1)
	app := NewBubbleteaApp(make(chan AgentEvent, 1), WithFollowUpCallback(func(input string) {
		submitted <- input
	}))
	model := app.model
	model.interactive = false

	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	model.Update(tea.KeyMsg{Type: tea.KeySpace})
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("world")})

	require.Contains(t, stripANSIPlain(app.View()), "> followup> hello world")
	model.Update(tea.KeyMsg{Type: tea.KeyEnter})

	select {
	case got := <-submitted:
		require.Equal(t, "hello world", got)
	case <-time.After(time.Second):
		t.Fatal("follow-up callback did not receive submitted input")
	}
}

func TestSteerUpdateAndViewPreserveSpace(t *testing.T) {
	submitted := make(chan string, 1)
	app := NewBubbleteaApp(make(chan AgentEvent, 1), WithSteerCallback(func(input string) {
		submitted <- input
	}))
	model := app.model
	model.interactive = false

	model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	model.Update(tea.KeyMsg{Type: tea.KeySpace})
	model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("world")})

	require.Contains(t, stripANSIPlain(app.View()), "> steer> hello world")
	model.Update(tea.KeyMsg{Type: tea.KeyEnter})

	select {
	case got := <-submitted:
		require.Equal(t, "hello world", got)
	case <-time.After(time.Second):
		t.Fatal("steer callback did not receive submitted input")
	}
}
