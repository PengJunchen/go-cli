//go:build mock

package mock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

// Compile-time assertions that the sub-agent mocks satisfy the core contracts.
var (
	_ core.SubAgent        = (*MockSubAgent)(nil)
	_ core.SubAgentFactory = (*MockSubAgentFactory)(nil)
)

func TestMockSubAgentRecordsCalls(t *testing.T) {
	sub := NewMockSubAgent("worker")
	sub.SetResult(core.AgentMessage{Role: "assistant", Content: "ok"}, nil)
	sub.SetEvents([]core.AgentEvent{{Kind: "message", Content: "start"}})

	ch, err := sub.Run(context.Background(), "do the thing")
	require.NoError(t, err)
	var got []core.AgentEvent
	for ev := range ch {
		got = append(got, ev)
	}
	require.Len(t, got, 1)

	require.NoError(t, sub.Send(context.Background(), "more"))
	require.NoError(t, sub.Interrupt(context.Background()))

	msg, err := sub.Wait(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "ok", msg.Content)

	assert.Len(t, sub.RunCalls(), 1)
	assert.Equal(t, "do the thing", sub.RunCalls()[0].Prompt)
	assert.Equal(t, []string{"more"}, sub.Sent())
	assert.Equal(t, 1, sub.InterruptCount())
	assert.Equal(t, 1, sub.WaitCount())
}

func TestMockSubAgentFactoryRecords(t *testing.T) {
	sub := NewMockSubAgent("worker")
	factory := NewMockSubAgentFactory(sub)

	created, err := factory.Create(context.Background(), "task-a", core.SubAgentConfig{Model: "mock"})
	require.NoError(t, err)
	// The factory returns the pre-configured mock sub-agent.
	assert.Equal(t, sub, created)

	calls := factory.CreateCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "task-a", calls[0].Name)
	assert.Equal(t, "mock", calls[0].Model)
}
