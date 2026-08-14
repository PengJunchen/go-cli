package core

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestSend_BlockTimeout_Expires verifies that when a BlockUntilConsumed
// stream has a block timeout configured and no consumer is reading, Send
// returns ErrSendTimeout instead of blocking forever.
func TestSend_BlockTimeout_Expires(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	stream := NewEventStream(0, WithEventBlockTimeout(100*time.Millisecond))

	start := time.Now()
	err := stream.Send(AgentEvent{Kind: "message", Content: "no-consumer"})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrSendTimeout, "Send should return ErrSendTimeout when no consumer reads within the timeout")
	assert.True(t, errors.Is(err, ErrSendTimeout), "err must wrap ErrSendTimeout")
	// Must expire within ~200ms of the configured 100ms timeout.
	assert.Less(t, elapsed, 200*time.Millisecond, "Send should return shortly after the timeout, got %v", elapsed)

	// Close must still work cleanly after a timed-out send.
	stream.Close()
}

// TestSend_BlockTimeout_NoTimeoutWhenConsumed verifies that when a consumer
// is actively reading, Send succeeds even though a block timeout is
// configured — the timeout only fires when the send would otherwise block.
func TestSend_BlockTimeout_NoTimeoutWhenConsumed(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	stream := NewEventStream(0, WithEventBlockTimeout(1*time.Second))

	got := make(chan AgentEvent, 1)
	go func() {
		ev, ok := <-stream.Events()
		if ok {
			got <- ev
		}
	}()

	require.NoError(t, stream.Send(AgentEvent{Kind: "status", Content: "consumed"}))
	stream.Close()

	select {
	case ev := <-got:
		assert.Equal(t, "status", ev.Kind)
		assert.Equal(t, "consumed", ev.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("consumer did not receive the event")
	}

	_, open := <-stream.Events()
	assert.False(t, open)
}
