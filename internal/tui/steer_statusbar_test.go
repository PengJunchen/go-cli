package tui

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSteerInputModeEntryExit verifies that TAB enters steer mode and Esc
// exits it without submitting.
func TestSteerInputModeEntryExit(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.interactive = false // avoid raw mode

	// TAB enters steer mode.
	app.handleMsg(keySteerEnter)
	assert.True(t, app.steerInputMode.Load())
	assert.Equal(t, "", app.steerInput)

	// Esc cancels steer mode.
	quit := app.handleMsg(keySteerCancel)
	assert.False(t, quit)
	assert.False(t, app.steerInputMode.Load())
}

// TestSteerInputTyping verifies that characters are inserted at the cursor
// position and backspace deletes the previous character.
func TestSteerInputTyping(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.interactive = false

	// Enter steer mode.
	app.handleMsg(keySteerEnter)

	// Type "hello".
	for _, ch := range []byte("hello") {
		app.handleMsg(steerCharMsg{char: ch})
	}
	assert.Equal(t, "hello", app.steerInput)
	assert.Equal(t, 5, app.steerCursor)

	// Backspace deletes 'o'.
	app.handleMsg(keySteerBackspace)
	assert.Equal(t, "hell", app.steerInput)
	assert.Equal(t, 4, app.steerCursor)
}

// TestSteerInputCursorMovement verifies Ctrl+A moves cursor to start and
// Ctrl+E moves cursor to end.
func TestSteerInputCursorMovement(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.interactive = false

	app.handleMsg(keySteerEnter)
	for _, ch := range []byte("hello") {
		app.handleMsg(steerCharMsg{char: ch})
	}
	assert.Equal(t, 5, app.steerCursor)

	// Ctrl+A moves cursor to start.
	app.handleMsg(keySteerCursorStart)
	assert.Equal(t, 0, app.steerCursor)

	// Type 'X' - should insert at start.
	app.handleMsg(steerCharMsg{char: 'X'})
	assert.Equal(t, "Xhello", app.steerInput)
	assert.Equal(t, 1, app.steerCursor)

	// Ctrl+E moves cursor to end.
	app.handleMsg(keySteerCursorEnd)
	assert.Equal(t, 6, app.steerCursor)
}

// TestSteerInputDeleteWord verifies Ctrl+W deletes the word before the cursor.
func TestSteerInputDeleteWord(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.interactive = false

	app.handleMsg(keySteerEnter)
	for _, ch := range []byte("hello world") {
		app.handleMsg(steerCharMsg{char: ch})
	}
	assert.Equal(t, "hello world", app.steerInput)

	// Ctrl+W deletes "world".
	app.handleMsg(keySteerDeleteWord)
	assert.Equal(t, "hello ", app.steerInput)
	assert.Equal(t, 6, app.steerCursor)
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
	app.interactive = false

	app.handleMsg(keySteerEnter)
	for _, ch := range []byte("go left") {
		app.handleMsg(steerCharMsg{char: ch})
	}

	// Enter submits.
	quit := app.handleMsg(keySteerSubmit)
	assert.False(t, quit, "submit should not quit the app")
	assert.False(t, app.steerInputMode.Load(), "should exit steer mode after submit")

	// Callback runs in a goroutine, wait for it.
	require.Eventually(t, func() bool {
		v := submitted.Load()
		s, ok := v.(string)
		return ok && s == "go left"
	}, time.Second, 5*time.Millisecond, "steer callback not invoked")
}

// TestCancelCallback verifies that pressing 'q' invokes the cancel callback.
func TestCancelCallback(t *testing.T) {
	var called atomic.Bool
	app := NewBubbleteaApp(make(chan AgentEvent, 1),
		WithCancelCallback(func() {
			called.Store(true)
		}),
	)
	app.interactive = false

	quit := app.handleMsg(keyQuit)
	assert.True(t, quit, "q should quit the app")
	assert.True(t, called.Load(), "cancel callback should be invoked")
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
	app.interactive = false

	// First Space: pause.
	app.handleMsg(keyPause)
	assert.True(t, pauseCalled.Load(), "pause callback should be invoked")
	assert.True(t, app.paused)

	// Second Space: resume.
	app.handleMsg(keyPause)
	assert.True(t, resumeCalled.Load(), "resume callback should be invoked")
	assert.False(t, app.paused)
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

	assert.Equal(t, 1000, app.tokenInput)
	assert.Equal(t, 500, app.tokenOutput)
	assert.Equal(t, 8000, app.tokenMax)
	assert.Equal(t, 0.025, app.tokenCost)
}

// TestStatusBarRendering verifies that the status bar renders the correct
// format and applies warning color when usage > 80%.
func TestStatusBarRendering(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))

	// Normal usage (under 80%).
	app.mu.Lock()
	app.tokenInput = 1000
	app.tokenOutput = 500
	app.tokenMax = 8000
	app.tokenCost = 0.025
	bar := app.renderStatusBar()
	app.mu.Unlock()

	assert.Contains(t, bar, "Tokens: 1500/8000 (18%)")
	assert.Contains(t, bar, "Cost: $0.0250")
	assert.NotContains(t, bar, "\x1b[33m", "should not have warning color under 80%")

	// High usage (over 80%).
	app.mu.Lock()
	app.tokenInput = 7000
	app.tokenOutput = 1000
	app.tokenMax = 8000
	bar = app.renderStatusBar()
	app.mu.Unlock()

	assert.Contains(t, bar, "\x1b[33m", "should have warning color over 80%")
	assert.Contains(t, bar, "100%")
}

// TestRenderViewWithSteerPrompt verifies that the steer prompt is rendered
// when in steer input mode.
func TestRenderViewWithSteerPrompt(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.interactive = false

	app.handleMsg(keySteerEnter)
	for _, ch := range []byte("hello") {
		app.handleMsg(steerCharMsg{char: ch})
	}

	view := app.View()
	assert.Contains(t, view, "> steer> ")
	assert.Contains(t, view, "hello")
}
