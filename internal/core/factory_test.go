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

func TestDefaultSubAgentNameFallsBackToSubagent(t *testing.T) {
	sub := NewDefaultSubAgent(SubAgentConfig{})
	assert.Equal(t, "subagent", sub.Name())
}

func TestDefaultSubAgentWithNilRunnerFactoryFallsBack(t *testing.T) {
	// Passing a nil option value must not leave the simulated runner unset.
	sub := NewDefaultSubAgent(
		SubAgentConfig{Name: "n", MaxTurns: 1},
		WithSubAgentRunner(nil),
	)
	subImpl, ok := sub.(*DefaultSubAgent)
	require.True(t, ok)
	assert.NotNil(t, subImpl.runnerFactory)
}

func TestSubAgentSimulatedRunnerDefaultMaxTurns(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// MaxTurns=0 falls back to the default of one turn (avoids division/zero).
	sub := NewDefaultSubAgent(SubAgentConfig{Name: "d", MaxTurns: 0})
	ch, err := sub.Run(context.Background(), "p")
	require.NoError(t, err)

	events := drainChan(ch)
	// user event + exactly one message event.
	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	res, err := sub.Wait(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "response-1", res.Content)
}

func TestSubAgentMaxTurnsRespected(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := NewDefaultSubAgent(SubAgentConfig{Name: "multi", MaxTurns: 3})
	ch, err := sub.Run(context.Background(), "p")
	require.NoError(t, err)

	events := drainChan(ch)
	messages := findEvents(events, "message")
	require.Len(t, messages, 3)
	res, err := sub.Wait(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "response-3", res.Content)
}

func TestSubAgentSendAfterTerminalIsRejected(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newTestSubAgent("early")
	_, err := sub.Run(context.Background(), "p")
	require.NoError(t, err)
	_, err = sub.Wait(context.Background())
	require.NoError(t, err)
	require.Equal(t, SubAgentCompleted, sub.State())

	// A send after the run ended must not be reported as accepted.
	require.ErrorIs(t, sub.Send(context.Background(), "late msg"), ErrSubAgentNotRunning)
	assert.Empty(t, sub.Received())
}

func TestSubAgentWaitContextCancellation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// A blocking runner that never completes lets us drive Wait's ctx branch.
	sub := NewDefaultSubAgent(SubAgentConfig{Name: "wait-ctx"}, WithSubAgentRunner(blockingFactory))
	_, err := sub.Run(context.Background(), "p")
	require.NoError(t, err)

	wctx, wcancel := context.WithCancel(context.Background())
	wcancel()
	_, err = sub.Wait(wctx)
	require.ErrorIs(t, err, context.Canceled)

	// Clean up the still-running sub-task so no goroutine leaks.
	require.NoError(t, sub.Interrupt(context.Background()))
	_, err = sub.Wait(context.Background())
	require.ErrorIs(t, err, context.Canceled)
}

func TestSubAgentSendDoesNotBlockOnFullInbox(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRunner := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRunner()
	runner := &heldInboxSubAgentRunner{started: started, release: release}
	sub := mustSubAgent(NewDefaultSubAgent(
		SubAgentConfig{Name: "inbox"},
		WithSubAgentRunner(func(SubAgentConfig) subAgentRunner { return runner }),
	))
	_, err := sub.Run(context.Background(), "p")
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sub-agent runner did not start")
	}

	// Blast far more messages than the inbox buffer; accepted messages are
	// recorded, while overflow is reported without blocking.
	accepted := 0
	for i := 0; i < 100; i++ {
		err := sub.Send(context.Background(), "msg")
		if err == nil {
			accepted++
		} else {
			require.ErrorIs(t, err, ErrSubAgentInboxFull)
		}
	}
	assert.Equal(t, inboxBufferSize, accepted)
	assert.Len(t, sub.Received(), accepted)
	releaseRunner()
	_, err = sub.Wait(context.Background())
	require.NoError(t, err)
	assert.Len(t, sub.Received(), inboxBufferSize)
}

func TestSubAgentPumpInboxClosedReturnsNil(t *testing.T) {
	inbox := make(chan string)
	close(inbox)
	var emitted []AgentEvent
	err := pumpInbox(context.Background(), inbox, func(ev AgentEvent) {
		emitted = append(emitted, ev)
	})
	assert.NoError(t, err)
	assert.Empty(t, emitted)
}

func TestSubAgentPumpInboxContextCancel(t *testing.T) {
	inbox := make(chan string)
	defer close(inbox)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := pumpInbox(ctx, inbox, func(AgentEvent) {})
	require.ErrorIs(t, err, context.Canceled)
}

func TestSubAgentPumpInboxEmptyDefaultReturnsNil(t *testing.T) {
	inbox := make(chan string)
	defer close(inbox)
	err := pumpInbox(context.Background(), inbox, func(AgentEvent) {})
	assert.NoError(t, err)
}

func TestSubAgentPumpInboxDrainsMessages(t *testing.T) {
	inbox := make(chan string, 2)
	inbox <- "a"
	inbox <- "b"
	close(inbox)

	var kinds []string
	var contents []string
	err := pumpInbox(context.Background(), inbox, func(ev AgentEvent) {
		kinds = append(kinds, ev.Kind)
		contents = append(contents, ev.Content)
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"user", "user"}, kinds)
	assert.Equal(t, []string{"a", "b"}, contents)
}

func TestSubAgentRunnerRunCtxCancelEmitsErrorEvent(t *testing.T) {
	runner := &simulatedSubAgentRunner{maxTurns: 5}
	ctx, cancel := context.WithCancel(context.Background())
	inbox := make(chan string)
	close(inbox)
	cancel() // pre-cancel so the runner's ctx check returns deterministically

	var events []AgentEvent
	msg, runErr := runner.Run(ctx, "p", inbox, func(ev AgentEvent) {
		events = append(events, ev)
	})

	require.ErrorIs(t, runErr, context.Canceled)
	assert.Equal(t, "assistant", msg.Role)
	// An error event is emitted when the ctx is canceled at the loop top.
	require.NotEmpty(t, findEvents(events, "error"))
}

func TestSubAgentFactoryNameFallsBackToConfigName(t *testing.T) {
	factory := NewSubAgentFactory()
	// When name is empty, the factory preserves the config name.
	sub, err := factory.Create(context.Background(), "", SubAgentConfig{Name: "from-config", Model: "m"})
	require.NoError(t, err)
	assert.Equal(t, "from-config", sub.Name())
}

func TestSubAgentFactoryNameOverridesConfig(t *testing.T) {
	factory := NewSubAgentFactory()
	// A supplied name takes precedence over the config's name.
	sub, err := factory.Create(context.Background(), "override", SubAgentConfig{Name: "config"})
	require.NoError(t, err)
	assert.Equal(t, "override", sub.Name())
}

func TestSubAgentRegistryDefaultLazyAndReset(t *testing.T) {
	original := GetSubAgentFactory()
	require.NotNil(t, original)

	// Register nil resets to a fresh default factory.
	RegisterSubAgentFactory(nil)
	def := GetSubAgentFactory()
	require.NotNil(t, def)

	// Restore the original default behavior for other tests.
	RegisterSubAgentFactory(nil)
}

func TestSubAgentFactoryRegistryNameResolution(t *testing.T) {
	assert.Equal(t, "default", factoryName(nil))
	// subFactoryStub does not implement Name(), so it resolves to "default".
	assert.Equal(t, "default", factoryName(subFactoryStub{}))

	named := &namedFactory{name: "custom"}
	assert.Equal(t, "custom", factoryName(named))
}

type namedFactory struct {
	name string
}

func (n *namedFactory) Name() string { return n.name }

func (n *namedFactory) Create(_ context.Context, _ string, _ SubAgentConfig) (SubAgent, error) {
	return newTestSubAgent("named"), nil
}

func TestSubAgentRegistryConcurrentAccess(t *testing.T) {
	// Production code uses a process-wide registry; concurrent Get/Register
	// must be race-free and always return a non-nil factory.
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := GetSubAgentFactory()
			require.NotNil(t, f)
			RegisterSubAgentFactory(subFactoryStub{})
			RegisterSubAgentFactory(nil)
		}()
	}
	wg.Wait()
	RegisterSubAgentFactory(nil)
}
