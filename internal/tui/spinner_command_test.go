//go:build !headless

package tui

import (
	"sync"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/stretchr/testify/require"
)

func TestToolCallSpinnerCommandCapturesScheduledTick(t *testing.T) {
	app := NewBubbleteaApp(nil)
	model := app.model
	command := model.handleEvent(AgentEvent{Type: "tool_call", ContentType: ContentTypeToolCall, Content: "read", ToolCallID: "call"})
	require.NotNil(t, command)
	expected, ok := model.spinner.Tick().(spinner.TickMsg)
	require.True(t, ok)
	// Bubble Tea may update the model before running a previously queued command.
	// That command must retain the scheduled tick's tag, not read the new state.
	_, _ = model.Update(expected)
	actual, ok := command().(spinner.TickMsg)
	require.True(t, ok)
	expected.Time = actual.Time
	require.Equal(t, expected, actual)
}

func TestToolCallSpinnerCommandConcurrentUpdate(t *testing.T) {
	app := NewBubbleteaApp(nil)
	model := app.model
	command := model.handleEvent(AgentEvent{Type: "tool_call", ContentType: ContentTypeToolCall, Content: "read", ToolCallID: "call"})
	require.NotNil(t, command)
	tick := spinner.TickMsg{ID: model.spinner.ID()}
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		for i := 0; i < 1000; i++ {
			_ = command()
		}
	}()
	close(start)
	for i := 0; i < 1000; i++ {
		_, _ = model.Update(tick)
	}
	workers.Wait()
}
