package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

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
	assert.NotContains(t, bar, "\x1b[", "should not have warning color under 80%")

	// High usage (over 80%).
	m.mu.Lock()
	m.tokenInput = 7000
	m.tokenOutput = 1000
	m.tokenMax = 8000
	bar = m.renderStatusBarLocked()
	m.mu.Unlock()

	assert.Contains(t, bar, "\x1b[", "should have warning color over 80%")
	assert.Contains(t, bar, "100%")
}

// TestStatusBar_TwoLines verifies the status bar renders as two lines when
// both model info and token usage are present.
func TestStatusBar_TwoLines(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.mu.Lock()
	m.modelName = "claude-sonnet-4"
	m.turnCount = 3
	m.sessionID = "abc12345"
	m.modeLabel = "chat"
	m.tokenInput = 1000
	m.tokenOutput = 500
	m.tokenMax = 8000
	m.tokenCost = 0.01
	bar := m.renderStatusBarLocked()
	m.mu.Unlock()

	// Two lines separated by \n.
	lines := splitLines(bar)
	assert.Len(t, lines, 2, "status bar should be two lines")
	// Line 1 contains model/turn/session/mode info.
	assert.Contains(t, lines[0], "claude-sonnet-4")
	assert.Contains(t, lines[0], "turn #3")
	assert.Contains(t, lines[0], "abc12345")
	assert.Contains(t, lines[0], "chat")
	// Line 2 contains tokens/cost.
	assert.Contains(t, lines[1], "Tokens:")
	assert.Contains(t, lines[1], "Cost:")
}

// TestStatusBar_ModelInfo verifies the model name appears on line 1.
func TestStatusBar_ModelInfo(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.mu.Lock()
	m.modelName = "gpt-4o"
	m.tokenInput = 100
	m.tokenMax = 8000
	bar := m.renderStatusBarLocked()
	m.mu.Unlock()

	assert.Contains(t, bar, "gpt-4o")
}

// TestStatusBar_TurnCount verifies the turn count appears on line 1.
func TestStatusBar_TurnCount(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.mu.Lock()
	m.turnCount = 5
	m.tokenInput = 100
	m.tokenMax = 8000
	bar := m.renderStatusBarLocked()
	m.mu.Unlock()

	assert.Contains(t, bar, "turn #5")
}

// TestStatusBar_TokenWarning verifies token usage >80% applies warning color.
func TestStatusBar_TokenWarning(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.mu.Lock()
	m.tokenInput = 8500
	m.tokenOutput = 0
	m.tokenMax = 10000
	m.tokenCost = 0.02
	bar := m.renderStatusBarLocked()
	m.mu.Unlock()

	// 8500/10000 = 85% > 80%, so the token line should have ANSI styling.
	assert.Contains(t, bar, "\x1b[", "over 80% should have warning ANSI styling")
	assert.Contains(t, bar, "85%")
}

// TestInteractive_TUIConfigWired verifies that TUIConfig options are correctly
// applied to the BubbleteaApp.
func TestInteractive_TUIConfigWired(t *testing.T) {
	app := NewBubbleteaApp(
		make(chan AgentEvent, 1),
		WithThemeConfig("monokai"),
		WithWordWrap(80),
		WithDiffStyle("split"),
		WithModelInfo("test-model"),
		WithSessionInfo("sess1234"),
		WithTurnCount(2),
		WithModeLabel("plan"),
	)
	m := app.model

	assert.Equal(t, "test-model", m.modelName)
	assert.Equal(t, 2, m.turnCount)
	assert.Equal(t, "sess1234", m.sessionID)
	assert.Equal(t, "plan", m.modeLabel)
	assert.Equal(t, 80, m.wordWrap)
	assert.Equal(t, "split", m.diffStyle)
	// Theme manager should have switched to monokai.
	theme := m.themeMgr.Get()
	assert.NotNil(t, theme)
	// Verify the theme name by checking it's not the default dark theme.
	_, isDark := theme.(DarkTheme)
	assert.False(t, isDark, "theme should not be the default dark theme")
}

// splitLines splits a string by newlines, trimming a single trailing empty
// string caused by a trailing newline.
func splitLines(s string) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
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

// TestSteerInputChineseBackspace verifies that Backspace on Chinese characters
// deletes a complete rune instead of slicing a multi-byte sequence in half.
func TestSteerInputChineseBackspace(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.interactive = false

	updateKey(m, keyMsg(tea.KeyTab))
	for _, ch := range "你好世界" {
		updateKey(m, runeKey(ch))
	}
	assert.Equal(t, "你好世界", m.steerInput)
	assert.Equal(t, 12, m.steerCursor) // 4 CJK chars × 3 bytes each

	// Backspace should delete "界" (one full rune).
	updateKey(m, keyMsg(tea.KeyBackspace))
	assert.True(t, utf8.ValidString(m.steerInput), "steerInput must remain valid UTF-8 after backspace")
	assert.Equal(t, "你好世", m.steerInput)
	assert.Equal(t, 9, m.steerCursor)

	// Another backspace deletes "世".
	updateKey(m, keyMsg(tea.KeyBackspace))
	assert.True(t, utf8.ValidString(m.steerInput))
	assert.Equal(t, "你好", m.steerInput)
	assert.Equal(t, 6, m.steerCursor)
}

// TestSteerInputChineseCtrlW verifies that Ctrl+W traverses by rune and does
// not split a multi-byte CJK character.
func TestSteerInputChineseCtrlW(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.interactive = false

	updateKey(m, keyMsg(tea.KeyTab))
	for _, ch := range "你好 世界" {
		updateKey(m, runeKey(ch))
	}
	assert.Equal(t, "你好 世界", m.steerInput)

	// Ctrl+W should delete "世界" (two CJK runes), leaving "你好 ".
	updateKey(m, keyMsg(tea.KeyCtrlW))
	assert.True(t, utf8.ValidString(m.steerInput), "steerInput must remain valid UTF-8 after Ctrl+W")
	assert.Equal(t, "你好 ", m.steerInput)
	assert.Equal(t, 7, m.steerCursor) // 2 CJK (6 bytes) + 1 space
}

// TestFollowUpInputChineseBackspace verifies that Backspace on Chinese
// characters in follow-up mode deletes a complete rune.
func TestFollowUpInputChineseBackspace(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.interactive = false

	// 'f' enters follow-up mode.
	updateKey(m, runeKey('f'))
	assert.True(t, m.followUpInputMode)

	for _, ch := range "你好" {
		updateKey(m, runeKey(ch))
	}
	assert.Equal(t, "你好", m.followUpInput)
	assert.Equal(t, 6, m.followUpCursor)

	// Backspace deletes "好".
	updateKey(m, keyMsg(tea.KeyBackspace))
	assert.True(t, utf8.ValidString(m.followUpInput))
	assert.Equal(t, "你", m.followUpInput)
	assert.Equal(t, 3, m.followUpCursor)
}

// TestFollowUpInputChineseCtrlW verifies Ctrl+W in follow-up mode traverses
// by rune with CJK characters.
func TestFollowUpInputChineseCtrlW(t *testing.T) {
	m := NewBubbleteaApp(make(chan AgentEvent, 1)).model
	m.interactive = false

	updateKey(m, runeKey('f'))
	for _, ch := range "你好 世界" {
		updateKey(m, runeKey(ch))
	}

	// Ctrl+W deletes "世界".
	updateKey(m, keyMsg(tea.KeyCtrlW))
	assert.True(t, utf8.ValidString(m.followUpInput))
	assert.Equal(t, "你好 ", m.followUpInput)
	assert.Equal(t, 7, m.followUpCursor)
}
