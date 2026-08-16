package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowStubLoop sleeps briefly before returning a message event, widening the
// window for concurrent access contention during tests.
type slowStubLoop struct {
	delay time.Duration
}

func (s *slowStubLoop) Run(_ context.Context, _ Submission, _ ...EventStream) ([]AgentEvent, error) {
	time.Sleep(s.delay)
	return []AgentEvent{{Kind: "message", Content: "ok", Timestamp: time.Now()}}, nil
}

// TestAgentImpl_ConcurrentReadDuringRun verifies that read-only methods
// (Messages, Events, State) can be called safely while Run is executing
// concurrently. The -race detector will flag any data races.
func TestAgentImpl_ConcurrentReadDuringRun(t *testing.T) {
	agent := NewAgentImpl("rw", &slowStubLoop{delay: 5 * time.Millisecond})

	var wg sync.WaitGroup

	// Writers: concurrent Run calls.
	const writers = 5
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := agent.Run(context.Background(), Submission{Content: "q"})
			assert.NoError(t, err)
		}()
	}

	// Readers: call read-only methods while writers are active.
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_ = agent.Messages()
				_ = agent.Events()
				_ = agent.State()
			}
		}()
	}

	wg.Wait()

	// Each Run appends a user message and an assistant message (the loop
	// returns a "message" event, so lastAssistant is non-nil). With Lock
	// protecting writes, no appends are lost.
	msgs := agent.Messages()
	assert.Equal(t, writers*2, len(msgs))
}

// TestAgentImpl_ReadLockDoesNotBlockRead verifies that two goroutines can
// hold the read lock simultaneously. If the mutex were a plain Mutex (or
// if Lock were used instead of RLock), the second goroutine would block
// until the first releases, causing a timeout.
func TestAgentImpl_ReadLockDoesNotBlockRead(t *testing.T) {
	agent := NewAgentImpl("rw", &stubLoop{})
	_, _ = agent.Run(context.Background(), Submission{Content: "seed"})

	// Direct test: hold RLock and verify another goroutine can also RLock.
	agent.mu.RLock()

	done := make(chan struct{})
	go func() {
		agent.mu.RLock()
		agent.mu.RUnlock()
		close(done)
	}()

	select {
	case <-done:
		// Success: RLock allowed concurrent access.
	case <-time.After(2 * time.Second):
		t.Fatal("RLock blocked another RLock — possible deadlock")
	}
	agent.mu.RUnlock()

	// Behavioral test: two goroutines calling Messages() simultaneously
	// must complete without deadlocking.
	msgsDone := make(chan struct{})
	go func() {
		var inner sync.WaitGroup
		for range 2 {
			inner.Add(1)
			go func() {
				defer inner.Done()
				_ = agent.Messages()
			}()
		}
		inner.Wait()
		close(msgsDone)
	}()

	select {
	case <-msgsDone:
		// Success: concurrent reads completed.
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Messages() calls deadlocked")
	}
}

// TestAgentImpl_WriteStillExclusive verifies that concurrent Run calls are
// serialized: no history appends are lost and the final count matches exactly
// 2*N (one user + one assistant per Run).
func TestAgentImpl_WriteStillExclusive(t *testing.T) {
	agent := NewAgentImpl("rw", &slowStubLoop{delay: 3 * time.Millisecond})

	const n = 20
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := agent.Run(context.Background(), Submission{Content: "q"})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	msgs := agent.Messages()
	// Each Run appends exactly one user + one assistant message.
	assert.Equal(t, n*2, len(msgs))
}

// TestAgentImpl_CompactReadWriteLockMix verifies that Compact correctly uses
// RLock for the read-only history copy and Lock for the write-back, and that
// concurrent reads (Messages, Events, State) do not deadlock or race during
// compaction.
func TestAgentImpl_CompactReadWriteLockMix(t *testing.T) {
	hook := func(_ context.Context, msgs []AgentMessage) ([]AgentMessage, error) {
		// Simulate compaction: keep only the last message.
		if len(msgs) > 1 {
			return msgs[len(msgs)-1:], nil
		}
		return msgs, nil
	}
	agent := NewAgentImpl("rw", &stubLoop{}, WithCompactionHook(hook))

	// Seed history directly via SetHistory (which does not trigger the hook).
	agent.SetHistory([]AgentMessage{
		{Role: "user", Content: "m1"},
		{Role: "assistant", Content: "m2"},
		{Role: "user", Content: "m3"},
		{Role: "assistant", Content: "m4"},
		{Role: "user", Content: "m5"},
	})
	require.Len(t, agent.Messages(), 5)

	// Start readers that run concurrently with Compact.
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_ = agent.Messages()
				_ = agent.Events()
				_ = agent.State()
			}
		}()
	}

	err := agent.Compact(context.Background())
	require.NoError(t, err)
	wg.Wait()

	// After compaction the history is reduced to a single message.
	msgs := agent.Messages()
	assert.Len(t, msgs, 1)
	assert.Equal(t, "m5", msgs[0].Content)
}
