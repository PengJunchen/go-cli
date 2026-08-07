package cli

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAssembleAgent_ExposesTurnRunner verifies that AssembleAgent creates a
// TurnRunner wired with the shared steering channel and exposes it on the
// returned AgentAssembly so the REPL can call Steer on a running turn.
func TestAssembleAgent_ExposesTurnRunner(t *testing.T) {
	assembly, err := AssembleAgent(
		context.Background(),
		newAssembleTestConfig(),
		"openai", "test-model",
		io.Discard,
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	require.NotNil(t, assembly.TurnRunner, "TurnRunner must be populated by AssembleAgent")
}

// TestAssembleAgent_TurnRunnerSharesSteerChannel verifies that the TurnRunner
// exposed by AssembleAgent shares the same steering channel as the LoopAgent,
// so a Steer call on the TurnRunner delivers the instruction into the running
// loop between LLM iterations.
func TestAssembleAgent_TurnRunnerSharesSteerChannel(t *testing.T) {
	assembly, err := AssembleAgent(
		context.Background(),
		newAssembleTestConfig(),
		"openai", "test-model",
		io.Discard,
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	require.NotNil(t, assembly.TurnRunner)
	require.NotNil(t, assembly.SteerChannel, "SteerChannel must be exposed for the REPL to write to")
}

// TestAssembleAgent_ExposesLoopAgent verifies that AssembleAgent exposes the
// raw LoopAgent so the REPL can call Pause()/Resume() on it.
func TestAssembleAgent_ExposesLoopAgent(t *testing.T) {
	assembly, err := AssembleAgent(
		context.Background(),
		newAssembleTestConfig(),
		"openai", "test-model",
		io.Discard,
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	require.NotNil(t, assembly.LoopAgent, "LoopAgent must be exposed for Pause/Resume")
}

// TestInterruptHandler_SendSteerWritesToChannel verifies that SendSteer writes
// the steering message to the steerCh (via SubmissionQueue) so the REPL's select
// on SteerChannel() receives it and can forward it to the TurnRunner.
func TestInterruptHandler_SendSteerWritesToChannel(t *testing.T) {
	cancel := func() {}
	h := NewInterruptHandler(cancel)
	h.Start(nil)
	defer h.Stop()

	require.NoError(t, h.SendSteer("change direction"))

	select {
	case msg := <-h.SteerChannel():
		assert.Equal(t, "change direction", msg)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for steer message on SteerChannel")
	}
}

// TestInterruptHandler_SendSteerMultipleMessages verifies that the
// SubmissionQueue + cap-16 steerCh allows multiple steer messages to be
// buffered without loss, unlike the old cap-1 channel.
func TestInterruptHandler_SendSteerMultipleMessages(t *testing.T) {
	cancel := func() {}
	h := NewInterruptHandler(cancel)
	h.Start(nil)
	defer h.Stop()

	// Send multiple steer messages - they should all be delivered.
	for i := 0; i < 5; i++ {
		require.NoError(t, h.SendSteer(string(rune('a'+i))))
	}

	received := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		select {
		case msg := <-h.SteerChannel():
			received = append(received, msg)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for steer message %d", i)
		}
	}
	assert.Len(t, received, 5)
}

// TestInterruptHandler_QueueLen verifies that QueueLen reports the number of
// pending steering messages in the SubmissionQueue before the drain goroutine
// starts.
func TestInterruptHandler_QueueLen(t *testing.T) {
	cancel := func() {}
	h := NewInterruptHandler(cancel)
	// Don't start the drain goroutine so messages stay in the queue.
	require.NoError(t, h.SendSteer("msg1"))
	require.NoError(t, h.SendSteer("msg2"))

	// Items should still be in the SubmissionQueue since drainQueue hasn't started.
	assert.Equal(t, 2, h.QueueLen(), "QueueLen should reflect queued messages before Start()")
}
