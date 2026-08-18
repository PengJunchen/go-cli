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

// newTestModel builds a non-interactive teaModel wired to an event channel for
// direct Update/View testing without spinning up a tea.Program.
func newTestModel(events <-chan AgentEvent) *teaModel {
	app := NewBubbleteaApp(events)
	app.model.interactive = false
	return app.model
}

// TestTeaModel_InitReturnsCmd verifies Init returns a non-nil command that
// arms the event, message and quit listeners.
func TestTeaModel_InitReturnsCmd(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	cmd := m.Init()
	require.NotNil(t, cmd, "Init must return a command")
	// Executing the batched command yields one of the listener messages (or
	// nil); it must not panic.
	_ = cmd()
}

// TestTeaModel_ViewEmpty verifies a fresh model renders an empty view.
func TestTeaModel_ViewEmpty(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	assert.Equal(t, "", m.View())
}

// TestTeaModel_HandlesAgentEvent verifies an agent event delivered via Update
// is rendered into the view.
func TestTeaModel_HandlesAgentEvent(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.Update(agentEventMsg{event: AgentEvent{
		ContentType: ContentTypeStatus, Content: "hello",
	}})
	assert.Contains(t, m.View(), "hello")
	assert.Equal(t, int64(1), m.eventsSeen.Load())
}

// TestTeaModel_HandlesIncremental verifies incremental streaming events
// accumulate into a single accordion entry.
func TestTeaModel_HandlesIncremental(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.Update(agentEventMsg{event: AgentEvent{
		ContentType: ContentTypeStreaming, Content: "Hello ", Incremental: true,
	}})
	m.Update(agentEventMsg{event: AgentEvent{
		ContentType: ContentTypeStreaming, Content: "world", Incremental: true,
	}})
	// Glamour renders streaming markdown with per-word ANSI styling, so the
	// contiguous "Hello world" is split across styled spans. Strip escapes to
	// assert the visible payload.
	assert.Contains(t, stripEscape(m.View()), "Hello world")
	require.Equal(t, 1, m.accordion.Len(), "incremental chunks accumulate into one entry")
}

// TestTeaModel_HandlesWindowSizeMsg verifies a resize updates the model dims.
func TestTeaModel_HandlesWindowSizeMsg(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.width, m.height = 0, 0
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	assert.Equal(t, 120, m.width)
	assert.Equal(t, 40, m.height)
}

// TestTeaModel_HandlesKeyMsgQuit verifies 'q' sets quitting and returns a
// tea.Quit command.
func TestTeaModel_HandlesKeyMsgQuit(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	cmd := m.handleKey(runeKey('q'))
	assert.True(t, isQuitCmd(cmd), "q should return a tea.Quit command")
	assert.True(t, m.quitting)
}

// TestTeaModel_HandlesKeyMsgSteer verifies Tab enters steer input mode.
func TestTeaModel_HandlesKeyMsgSteer(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	updateKey(m, keyMsg(tea.KeyTab))
	assert.True(t, m.steerInputMode)
	assert.Equal(t, "", m.steerInput)
}

// TestTeaModel_SteerInputAndSubmit verifies typing then Enter submits the steer
// text via the callback and exits steer mode.
func TestTeaModel_SteerInputAndSubmit(t *testing.T) {
	var submitted atomic.Value // string
	app := NewBubbleteaApp(make(chan AgentEvent, 1),
		WithSteerCallback(func(input string) { submitted.Store(input) }),
	)
	m := app.model
	m.interactive = false

	updateKey(m, keyMsg(tea.KeyTab))
	for _, ch := range "stop" {
		updateKey(m, runeKey(ch))
	}
	updateKey(m, keyMsg(tea.KeyEnter))

	assert.False(t, m.steerInputMode, "should exit steer mode after submit")
	require.Eventually(t, func() bool {
		v := submitted.Load()
		s, ok := v.(string)
		return ok && s == "stop"
	}, time.Second, 5*time.Millisecond, "steer callback not invoked with submitted text")
}

// TestTeaModel_HandlesPauseResume verifies Space toggles the paused flag and
// fires the matching callbacks.
func TestTeaModel_HandlesPauseResume(t *testing.T) {
	var paused, resumed atomic.Bool
	app := NewBubbleteaApp(make(chan AgentEvent, 1),
		WithPauseCallback(func() { paused.Store(true) }),
		WithResumeCallback(func() { resumed.Store(true) }),
	)
	m := app.model
	m.interactive = false

	updateKey(m, keyMsg(tea.KeySpace))
	assert.True(t, m.paused)
	assert.True(t, paused.Load())

	updateKey(m, keyMsg(tea.KeySpace))
	assert.False(t, m.paused)
	assert.True(t, resumed.Load())
}

// TestTeaModel_TokenUsageStatusBar verifies a token_usage event updates the
// status bar data and that the view renders the bar with warning color over
// 80% usage.
func TestTeaModel_TokenUsageStatusBar(t *testing.T) {
	m := newTestModel(make(chan AgentEvent, 1))
	m.Update(agentEventMsg{event: AgentEvent{
		Type: "token_usage",
		TokenUsage: &TokenUsageData{
			InputTokens: 7000, OutputTokens: 1000, MaxTokens: 8000, Cost: 0.05,
		},
	}})
	assert.Equal(t, 7000, m.tokenInput)
	assert.Equal(t, 8000, m.tokenMax)
	view := m.View()
	assert.Contains(t, view, "100%")
	assert.Contains(t, view, "\x1b[", "over 80% usage should be yellow")
}

// TestBubbleteaApp_RunCompletesOnChannelClose verifies Run returns nil when the
// event channel closes (waitForEvent returns tea.Quit).
func TestBubbleteaApp_RunCompletesOnChannelClose(t *testing.T) {
	events := make(chan AgentEvent, 2)
	events <- AgentEvent{ContentType: ContentTypeStatus, Content: "done"}
	close(events)

	app := NewBubbleteaApp(events)
	require.NoError(t, app.Run(context.Background()))
	assert.Equal(t, int64(1), app.EventsProcessed())
	assert.Contains(t, app.View(), "done")
}

// TestBubbleteaApp_QuitStopsLoop verifies Quit stops a running loop and that
// Run returns nil.
func TestBubbleteaApp_QuitStopsLoop(t *testing.T) {
	events := make(chan AgentEvent, 1)
	defer close(events)
	app := NewBubbleteaApp(events)
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(context.Background()) }() //nolint:errcheck

	app.Quit()
	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after Quit")
	}
}
