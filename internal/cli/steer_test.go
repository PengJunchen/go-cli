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

// TestInterruptHandler_SendSteerWritesToChannel verifies that SendSteer writes
// the steering message to the steerCh so the REPL's select on SteerChannel()
// receives it and can forward it to the TurnRunner.
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

// TestInterruptHandler_SendSteerNonBlocking verifies that SendSteer is
// non-blocking when the channel buffer is full (cap 1), so the REPL loop
// is never stalled by a steer write.
func TestInterruptHandler_SendSteerNonBlocking(t *testing.T) {
	cancel := func() {}
	h := NewInterruptHandler(cancel)
	h.Start(nil)
	defer h.Stop()

	// Fill the buffer (cap 1).
	require.NoError(t, h.SendSteer("first"))
	// Second send should not block — it drops silently.
	require.NoError(t, h.SendSteer("second"))

	// Drain — should get at least one message.
	select {
	case <-h.SteerChannel():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for steer message")
	}
}
