package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestDispatchForwardsEventsViaCallback(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newFakeSubAgent("worker", AgentMessage{Role: "assistant", Content: "done"})
	sub.events = []AgentEvent{
		{Kind: "status", Content: "starting", Timestamp: time.Now()},
		{Kind: "message", Content: "working", Timestamp: time.Now()},
	}
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}

	d := NewDefaultSubagentDispatcher(factory)

	var mu sync.Mutex
	var forwarded []AgentEvent
	d.SetEventForwarder(func(taskID string, ev AgentEvent) {
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, "t1", taskID)
		forwarded = append(forwarded, ev)
	})

	_, err := d.Dispatch(context.Background(), SubagentTask{ID: "t1", Prompt: "go"})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, forwarded, 2, "both events should be forwarded")
	assert.Equal(t, "starting", forwarded[0].Content)
	assert.Equal(t, "working", forwarded[1].Content)
}

func TestDispatchNoForwarderDrainsSilently(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newFakeSubAgent("worker", AgentMessage{Role: "assistant", Content: "done"})
	sub.events = []AgentEvent{
		{Kind: "status", Content: "starting", Timestamp: time.Now()},
	}
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}

	d := NewDefaultSubagentDispatcher(factory)
	// No event forwarder set - should not panic and should drain silently.
	_, err := d.Dispatch(context.Background(), SubagentTask{ID: "t1", Prompt: "go"})
	require.NoError(t, err)
}

func TestDispatchAppliesRoleToolWhitelist(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newFakeSubAgent("worker", AgentMessage{Role: "assistant", Content: "done"})
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}

	d := NewDefaultSubagentDispatcher(factory)
	_, err := d.Dispatch(context.Background(), SubagentTask{
		ID:     "t1",
		Prompt: "go",
		Role:   "researcher",
	})
	require.NoError(t, err)

	require.Len(t, factory.configs, 1)
	assert.Equal(t, []string{"read", "grep", "find", "ls", "web_fetch"}, factory.configs[0].Tools,
		"role whitelist should be applied when tools are not specified")
}

func TestDispatchExplicitToolsOverrideRoleWhitelist(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newFakeSubAgent("worker", AgentMessage{Role: "assistant", Content: "done"})
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}

	d := NewDefaultSubagentDispatcher(factory)
	_, err := d.Dispatch(context.Background(), SubagentTask{
		ID:     "t1",
		Prompt: "go",
		Role:   "researcher",
		Tools:  []string{"bash", "write"},
	})
	require.NoError(t, err)

	require.Len(t, factory.configs, 1)
	assert.Equal(t, []string{"bash", "write"}, factory.configs[0].Tools,
		"explicit tools should override the role whitelist")
}
