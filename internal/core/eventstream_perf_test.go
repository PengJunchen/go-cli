package core

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestEventStream_DefaultBlockTimeout verifies that a stream created without
// WithEventBlockTimeout gets the 30s default, and that the timeout mechanism
// actually fires (tested with a short timeout to keep the test fast).
func TestEventStream_DefaultBlockTimeout(t *testing.T) {
	// Part 1: the default field must be 30s.
	stream := NewEventStream(0)
	assert.Equal(t, defaultBlockTimeout, stream.blockTimeout,
		"NewEventStream without WithEventBlockTimeout should default to %v", defaultBlockTimeout)
	stream.Close()

	// Part 2: the timeout mechanism fires and returns ErrSendTimeout.
	// We use a short explicit timeout to test the mechanism quickly.
	defer verify.AssertNoGoroutineLeak(t)()

	timedStream := NewEventStream(0, WithEventBlockTimeout(100*time.Millisecond))

	start := time.Now()
	err := timedStream.Send(AgentEvent{Kind: "message", Content: "no-consumer"})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrSendTimeout)
	assert.True(t, errors.Is(err, ErrSendTimeout))
	assert.Less(t, elapsed, 200*time.Millisecond, "Send should return shortly after the timeout, got %v", elapsed)

	timedStream.Close()
}

// TestEventStream_SentCountAtomic verifies that concurrent Send and SentCount
// are race-free and that the final count is correct.
func TestEventStream_SentCountAtomic(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	const numSenders = 50
	const sendsPerGoroutine = 100
	const totalSends = numSenders * sendsPerGoroutine

	stream := NewEventStream(totalSends)

	var wg sync.WaitGroup
	wg.Add(numSenders)

	// Concurrent readers to keep the buffer drained (not strictly needed
	// since buffer == totalSends, but exercises the channel path).
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for range stream.Events() {
		}
	}()

	// Concurrent SentCount reader — must not race with Send.
	stopRead := make(chan struct{})
	var readWg sync.WaitGroup
	readWg.Add(1)
	go func() {
		defer readWg.Done()
		for {
			select {
			case <-stopRead:
				return
			default:
				_ = stream.SentCount()
			}
		}
	}()

	for range numSenders {
		go func() {
			defer wg.Done()
			for j := 0; j < sendsPerGoroutine; j++ {
				//nolint:errcheck,gosec // buffer is large enough; best-effort under test.
				stream.Send(AgentEvent{Kind: "message", Content: "x"})
			}
		}()
	}

	wg.Wait()
	close(stopRead)
	readWg.Wait()

	stream.Close()
	<-drainDone

	assert.Equal(t, totalSends, stream.SentCount(),
		"SentCount must equal total sends after concurrent completion")
}

// TestEventStream_ExplicitTimeoutOverridesDefault verifies that
// WithEventBlockTimeout(5s) overrides the 30s default.
func TestEventStream_ExplicitTimeoutOverridesDefault(t *testing.T) {
	stream := NewEventStream(0, WithEventBlockTimeout(5*time.Second))
	assert.Equal(t, 5*time.Second, stream.blockTimeout,
		"WithEventBlockTimeout(5s) should override the default 30s")
	assert.NotEqual(t, defaultBlockTimeout, stream.blockTimeout)
	stream.Close()
}

// TestEventStream_ZeroTimeoutMeansBlockForever verifies that
// WithEventBlockTimeout(0) means block forever (backward compatible).
// A Send on a full zero-capacity stream must not return on its own.
func TestEventStream_ZeroTimeoutMeansBlockForever(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	stream := NewEventStream(0, WithEventBlockTimeout(0))
	assert.Zero(t, stream.blockTimeout, "WithEventBlockTimeout(0) should set blockTimeout to 0")

	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		//nolint:errcheck,gosec // Send returns nil after Close; best-effort under test.
		stream.Send(AgentEvent{Kind: "message", Content: "blocked"})
	}()

	// The send must still be blocked after 50ms — it should block forever.
	select {
	case <-sendDone:
		t.Fatal("Send with zero timeout returned unexpectedly (should block forever)")
	case <-time.After(50 * time.Millisecond):
		// Expected: send is still blocked.
	}

	// Close unblocks the send via the done channel.
	stream.Close()

	select {
	case <-sendDone:
		// Send unblocked by Close.
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not unblock after Close")
	}
}
