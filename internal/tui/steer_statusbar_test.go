package tui

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyMsg is a tiny helper that builds a tea.KeyMsg from a key type and optional
// runes, keeping the steer/pause/quit tests readable.
func keyMsg(t tea.KeyType, runes ...rune) tea.KeyMsg {
	return tea.KeyMsg{Type: t, Runes: runes}
}

// runeKey builds a tea.KeyMsg carrying a single printable rune.
func runeKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// updateKey drives the model with a key message and discards the returned cmd.
func updateKey(m *teaModel, msg tea.KeyMsg) {
	_, _ = m.Update(msg)
}

// TestSteerInputModeEntryExit verifies that TAB enters steer mode and Esc
// exits it without submitting.
func TestSteerInputModeEntryExit(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.interactive = false // avoid TTY-only behavior

	// TAB enters steer mode.
	updateKey(m, keyMsg(tea.KeyTab))
	assert.True(t, m.steerInputMode)
	assert.Equal(t, "", m.steerInput)

	// Esc cancels steer mode (does not quit).
	cmd := m.handleKey(keyMsg(tea.KeyEsc))
	assert.Nil(t, cmd, "Esc in steer mode should exit steer, not quit")
	assert.False(t, m.steerInputMode)
}

// TestSteerInputTyping verifies that characters are inserted at the cursor
// position and backspace deletes the previous character.
func TestSteerInputTyping(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.interactive = false

	updateKey(m, keyMsg(tea.KeyTab))

	// Type "hello".
	for _, ch := range "hello" {
		updateKey(m, runeKey(ch))
	}
	assert.Equal(t, "hello", m.steerInput)
	assert.Equal(t, 5, m.steerCursor)

	// Backspace deletes 'o'.
	updateKey(m, keyMsg(tea.KeyBackspace))
	assert.Equal(t, "hell", m.steerInput)
	assert.Equal(t, 4, m.steerCursor)
}

// TestSteerInputCursorMovement verifies Ctrl+A moves cursor to start and
// Ctrl+E moves cursor to end.
func TestSteerInputCursorMovement(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.interactive = false

	updateKey(m, keyMsg(tea.KeyTab))
	for _, ch := range "hello" {
		updateKey(m, runeKey(ch))
	}
	assert.Equal(t, 5, m.steerCursor)

	// Ctrl+A moves cursor to start.
	updateKey(m, keyMsg(tea.KeyCtrlA))
	assert.Equal(t, 0, m.steerCursor)

	// Type 'X' - should insert at start.
	updateKey(m, runeKey('X'))
	assert.Equal(t, "Xhello", m.steerInput)
	assert.Equal(t, 1, m.steerCursor)

	// Ctrl+E moves cursor to end.
	updateKey(m, keyMsg(tea.KeyCtrlE))
	assert.Equal(t, 6, m.steerCursor)
}

// TestSteerInputDeleteWord verifies Ctrl+W deletes the word before the cursor.
func TestSteerInputDeleteWord(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.interactive = false

	updateKey(m, keyMsg(tea.KeyTab))
	for _, ch := range "hello world" {
		updateKey(m, runeKey(ch))
	}
	assert.Equal(t, "hello world", m.steerInput)

	// Ctrl+W deletes "world".
	updateKey(m, keyMsg(tea.KeyCtrlW))
	assert.Equal(t, "hello ", m.steerInput)
	assert.Equal(t, 6, m.steerCursor)
}

// TestSteerInputSubmit verifies that Enter submits the steer text via the
// callback and exits steer mode.
func TestSteerInputSubmit(t *testing.T) {
	var submitted atomic.Value // string
	app := NewBubbleteaApp(make(chan AgentEvent, 1),
		WithSteerCallback(func(input string) {
			submitted.Store(input)
		}),
	)
	m := app.model
	m.interactive = false

	updateKey(m, keyMsg(tea.KeyTab))
	for _, ch := range "go left" {
		updateKey(m, runeKey(ch))
	}

	// Enter submits.
	cmd := m.handleKey(keyMsg(tea.KeyEnter))
	assert.Nil(t, cmd, "submit should not quit the app")
	assert.False(t, m.steerInputMode, "should exit steer mode after submit")

	// Callback runs in a goroutine, wait for it.
	require.Eventually(t, func() bool {
		v := submitted.Load()
		s, ok := v.(string)
		return ok && s == "go left"
	}, time.Second, 5*time.Millisecond, "steer callback not invoked")
}

// isQuitCmd reports whether cmd is the tea.Quit command (by executing it once
// and inspecting the produced message). tea.Cmd is a func value so it cannot
// be compared directly with ==.
func isQuitCmd(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// TestCancelCallback verifies that pressing 'q' invokes the cancel callback and
// signals quit.
func TestCancelCallback(t *testing.T) {
	var called atomic.Bool
	app := NewBubbleteaApp(make(chan AgentEvent, 1),
		WithCancelCallback(func() {
			called.Store(true)
		}),
	)
	m := app.model
	m.interactive = false

	cmd := m.handleKey(runeKey('q'))
	assert.True(t, isQuitCmd(cmd), "q should quit the app")
	assert.True(t, m.quitting)
	assert.True(t, called.Load(), "cancel callback should be invoked")
}

// TestCtrlCQuit verifies Ctrl+C quits and invokes the cancel callback.
func TestCtrlCQuit(t *testing.T) {
	var called atomic.Bool
	app := NewBubbleteaApp(make(chan AgentEvent, 1),
		WithCancelCallback(func() {
			called.Store(true)
		}),
	)
	m := app.model
	m.interactive = false

	cmd := m.handleKey(keyMsg(tea.KeyCtrlC))
	assert.True(t, isQuitCmd(cmd), "Ctrl+C should quit the app")
	assert.True(t, called.Load(), "cancel callback should be invoked on Ctrl+C")
}

// TestPauseToggle verifies that Space toggles pause/resume callbacks.
func TestPauseToggle(t *testing.T) {
	var pauseCalled, resumeCalled atomic.Bool
	app := NewBubbleteaApp(make(chan AgentEvent, 1),
		WithPauseCallback(func() {
			pauseCalled.Store(true)
		}),
		WithResumeCallback(func() {
			resumeCalled.Store(true)
		}),
	)
	m := app.model
	m.interactive = false

	// First Space: pause.
	updateKey(m, keyMsg(tea.KeySpace))
	assert.True(t, pauseCalled.Load(), "pause callback should be invoked")
	assert.True(t, m.paused)

	// Second Space: resume.
	updateKey(m, keyMsg(tea.KeySpace))
	assert.True(t, resumeCalled.Load(), "resume callback should be invoked")
	assert.False(t, m.paused)
}

// TestTokenUsageStatusBar verifies that a token_usage event updates the status
// bar data.
func TestTokenUsageStatusBar(t *testing.T) {
	events := make(chan AgentEvent, 2)
	events <- AgentEvent{
		Type: "token_usage",
		TokenUsage: &TokenUsageData{
			InputTokens:  1000,
			OutputTokens: 500,
			MaxTokens:    8000,
			Cost:         0.025,
		},
	}
	close(events)

	app := NewBubbleteaApp(events)
	require.NoError(t, app.Run(context.Background()))

	m := app.model
	assert.Equal(t, 1000, m.tokenInput)
	assert.Equal(t, 500, m.tokenOutput)
	assert.Equal(t, 8000, m.tokenMax)
	assert.Equal(t, 0.025, m.tokenCost)
}

// TestStatusBarRendering verifies that the status bar renders the correct
// format and applies warning color when usage > 80%.
func TestStatusBarRendering(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model

	// Normal usage (under 80%).
	m.mu.Lock()
	m.tokenInput = 1000
	m.tokenOutput = 500
	m.tokenMax = 8000
	m.tokenCost = 0.025
	bar := m.renderStatusBarLocked()
	m.mu.Unlock()

	assert.Contains(t, bar, "Tokens: 1500/8000 (18%)")
	assert.Contains(t, bar, "Cost: $0.0250")
	assert.NotContains(t, bar, "\x1b[33m", "should not have warning color under 80%")

	// High usage (over 80%).
	m.mu.Lock()
	m.tokenInput = 7000
	m.tokenOutput = 1000
	m.tokenMax = 8000
	bar = m.renderStatusBarLocked()
	m.mu.Unlock()

	assert.Contains(t, bar, "\x1b[33m", "should have warning color over 80%")
	assert.Contains(t, bar, "100%")
}

// TestRenderViewWithSteerPrompt verifies that the steer prompt is rendered
// when in steer input mode.
func TestRenderViewWithSteerPrompt(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	m := app.model
	m.interactive = false

	updateKey(m, keyMsg(tea.KeyTab))
	for _, ch := range "hello" {
		updateKey(m, runeKey(ch))
	}

	view := app.View()
	assert.Contains(t, view, "> steer> ")
	assert.Contains(t, view, "hello")
}
