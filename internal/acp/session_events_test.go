package acp

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

func TestSessionSubscribeAfterAtomicReplayLiveCutover(t *testing.T) {
	const subscriberCount = 6
	const eventCount = 48

	session := NewSession("cutover")
	start := make(chan struct{})
	subscriptions := make([]*EventSubscription, subscriberCount)
	var workers sync.WaitGroup
	workers.Add(subscriberCount + 1)
	for i := range subscriptions {
		go func(index int) {
			defer workers.Done()
			<-start
			subscription, err := session.SubscribeAfter(0)
			if err != nil {
				t.Errorf("SubscribeAfter failed: %v", err)
				return
			}
			subscriptions[index] = subscription
		}(i)
	}
	go func() {
		defer workers.Done()
		<-start
		for i := 1; i <= eventCount; i++ {
			session.PublishEvent(core.AgentEvent{Kind: "message", Content: fmt.Sprint(i)})
		}
	}()
	close(start)
	workers.Wait()

	for _, subscription := range subscriptions {
		require.NotNil(t, subscription)
		require.Nil(t, subscription.HistoryGap)
		entries := append([]EventEntry(nil), subscription.Replay...)
		require.Equal(t, uint64(len(entries)), subscription.LastSeq)
		for range eventCount - len(entries) {
			select {
			case entry, ok := <-subscription.Events():
				require.True(t, ok, "live stream closed before replay/live cutover completed")
				entries = append(entries, entry)
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for an event across the replay/live cutover")
			}
		}
		require.Len(t, entries, eventCount)
		for index, entry := range entries {
			require.Equal(t, uint64(index+1), entry.ID, "replay plus live must be exactly once and contiguous")
		}
		require.Nil(t, subscription.Close())
	}
}

func TestSessionSubscribeAfterCutoverPublishesNextEntryOnlyLive(t *testing.T) {
	session := NewSession("cutover-boundary")
	session.PublishEvent(core.AgentEvent{Kind: "message", Content: "before"})

	subscription := mustSubscribeAfter(t, session, 0)
	defer subscription.Close()
	require.Equal(t, uint64(1), subscription.LastSeq)
	require.Len(t, subscription.Replay, 1)
	require.Equal(t, uint64(1), subscription.Replay[0].ID)

	session.PublishEvent(core.AgentEvent{Kind: "message", Content: "after"})
	select {
	case entry := <-subscription.Events():
		require.Equal(t, uint64(2), entry.ID)
		require.Equal(t, "after", entry.Event.Content)
	case <-time.After(time.Second):
		t.Fatal("event published immediately after the cutover was not live")
	}
	select {
	case entry := <-subscription.Events():
		t.Fatalf("cutover event was delivered more than once: %+v", entry)
	default:
	}
}

func TestSessionSubscribeAfterRejectsFutureCursorWithoutRegistering(t *testing.T) {
	session := NewSession("future-cursor")
	session.PublishEvent(core.AgentEvent{Kind: "message", Content: "current"})

	subscription, err := session.SubscribeAfter(2)
	require.Nil(t, subscription)
	require.ErrorIs(t, err, ErrEventCursorAhead)
	session.mu.Lock()
	subscriberCount := len(session.subscribers)
	session.mu.Unlock()
	require.Zero(t, subscriberCount, "rejected cursor must not register a subscriber")
}

func TestSessionSubscribeAfterFutureCursorCheckSharesPublishLock(t *testing.T) {
	for range 100 {
		session := NewSession("future-cursor-race")
		start := make(chan struct{})
		publishDone := make(chan struct{})
		go func() {
			defer close(publishDone)
			<-start
			session.PublishEvent(core.AgentEvent{Kind: "message", Content: "new"})
		}()

		close(start)
		subscription, err := session.SubscribeAfter(1)
		<-publishDone
		if err != nil {
			require.ErrorIs(t, err, ErrEventCursorAhead)
			require.Nil(t, subscription)
			continue
		}
		require.NotNil(t, subscription)
		require.GreaterOrEqual(t, subscription.LastSeq, uint64(1))
		require.Nil(t, subscription.HistoryGap)
		require.Empty(t, subscription.Replay, "cursor at the concurrently published event is already current")
		require.Nil(t, subscription.Close())
	}
}

func TestSessionSubscribeAfterReportsRetentionGapAndPreservesIDs(t *testing.T) {
	session := NewSession("retention")
	const priorEvents = eventHistorySize + 7
	for i := 1; i <= priorEvents; i++ {
		session.PublishEvent(core.AgentEvent{Kind: "message", Content: fmt.Sprint(i)})
	}

	subscription := mustSubscribeAfter(t, session, 0)
	defer subscription.Close()
	require.NotNil(t, subscription.HistoryGap)
	require.Equal(t, "acp_session_transport", subscription.HistoryGap.Layer)
	require.Equal(t, "retention", subscription.HistoryGap.Reason)
	require.Equal(t, uint64(1), subscription.HistoryGap.MissingFrom)
	require.Equal(t, uint64(7), subscription.HistoryGap.MissingThrough)
	require.Equal(t, uint64(7), subscription.HistoryGap.MissingCount)
	require.Equal(t, uint64(7), subscription.HistoryGap.Cursor)
	require.Equal(t, uint64(8), subscription.HistoryGap.FirstAvailableID)
	require.Equal(t, uint64(priorEvents), subscription.HistoryGap.LastEventID)
	require.Len(t, subscription.Replay, eventHistorySize)
	require.Equal(t, uint64(8), subscription.Replay[0].ID)
	require.Equal(t, uint64(priorEvents), subscription.Replay[len(subscription.Replay)-1].ID)

	session.PublishEvent(core.AgentEvent{Kind: "done", Content: "after snapshot"})
	select {
	case entry := <-subscription.Events():
		require.Equal(t, uint64(priorEvents+1), entry.ID)
		require.Equal(t, "after snapshot", entry.Event.Content)
	case <-time.After(time.Second):
		t.Fatal("post-snapshot event was not delivered live")
	}
}

func TestSessionSubscribersAreIsolatedAndSlowOverflowDoesNotBlockPublish(t *testing.T) {
	session := NewSession("subscribers")
	fast := mustSubscribeAfter(t, session, 0)
	slow := mustSubscribeAfter(t, session, 0)

	fastAck := make(chan struct{}, 1)
	publishDone := make(chan struct{})
	go func() {
		defer close(publishDone)
		for i := 1; i <= eventSubscriberBufferSize+32; i++ {
			session.PublishEvent(core.AgentEvent{Kind: "message", Content: fmt.Sprint(i)})
			<-fastAck
		}
	}()

	for i := 1; i <= eventSubscriberBufferSize+32; i++ {
		select {
		case entry := <-fast.Events():
			require.Equal(t, uint64(i), entry.ID, "fast subscriber should receive its own complete stream")
		case <-time.After(time.Second):
			t.Fatal("fast subscriber stalled while another subscriber overflowed")
		}
		fastAck <- struct{}{}
	}
	select {
	case <-publishDone:
	case <-time.After(time.Second):
		t.Fatal("slow subscriber blocked the event producer")
	}
	select {
	case <-slow.Done():
	default:
		t.Fatal("slow subscriber did not receive bounded-overflow notification")
	}

	gap := slow.Close()
	require.NotNil(t, gap)
	require.Equal(t, "subscriber_overflow", gap.Reason)
	require.Equal(t, uint64(eventSubscriberBufferSize+1), gap.MissingFrom)
	require.Equal(t, uint64(eventSubscriberBufferSize+32), gap.MissingThrough)
	require.Equal(t, uint64(32), gap.MissingCount)
	require.Equal(t, uint64(eventSubscriberBufferSize), gap.Cursor)
	require.Equal(t, uint64(eventSubscriberBufferSize+32), gap.LastEventID)

	var slowIDs []uint64
	for entry := range slow.Events() {
		slowIDs = append(slowIDs, entry.ID)
	}
	require.Len(t, slowIDs, eventSubscriberBufferSize)
	for index, id := range slowIDs {
		require.Equal(t, uint64(index+1), id)
	}

	// The fast subscriber has independent bounded capacity and is not marked
	// gapped just because another reader fell behind.
	fast.Close()
	select {
	case _, ok := <-fast.Events():
		require.False(t, ok, "Close must unregister and close the subscriber")
	case <-time.After(time.Second):
		t.Fatal("closed subscriber was not joinable")
	}
	session.PublishEvent(core.AgentEvent{Kind: "after-close"})
}

func TestSessionSubscriptionsDoNotCrossSessions(t *testing.T) {
	left := NewSession("left")
	right := NewSession("right")
	leftSub := mustSubscribeAfter(t, left, 0)
	rightSub := mustSubscribeAfter(t, right, 0)
	defer leftSub.Close()
	defer rightSub.Close()

	left.PublishEvent(core.AgentEvent{Kind: "message", Content: "left-only"})
	select {
	case entry := <-leftSub.Events():
		require.Equal(t, "left-only", entry.Event.Content)
	default:
		t.Fatal("left session event was not delivered to its subscriber")
	}
	select {
	case entry := <-rightSub.Events():
		t.Fatalf("left session event crossed into right session: %+v", entry)
	default:
	}

	right.PublishEvent(core.AgentEvent{Kind: "message", Content: "right-only"})
	select {
	case entry := <-rightSub.Events():
		require.Equal(t, "right-only", entry.Event.Content)
	default:
		t.Fatal("right session event was not delivered to its subscriber")
	}
}

func TestSessionSubscriptionCloseRacesWithPublish(t *testing.T) {
	for iteration := 0; iteration < 8; iteration++ {
		session := NewSession("cancel-race")
		subscription := mustSubscribeAfter(t, session, 0)
		publishDone := make(chan struct{})
		go func() {
			defer close(publishDone)
			for i := 0; i < eventSubscriberBufferSize+100; i++ {
				session.PublishEvent(core.AgentEvent{Kind: "message"})
			}
		}()

		select {
		case <-subscription.Done():
		case <-time.After(time.Second):
			t.Fatal("subscriber did not overflow before cancellation")
		}
		gap := subscription.Close()
		require.NotNil(t, gap)
		require.GreaterOrEqual(t, gap.MissingThrough, gap.MissingFrom)
		require.Equal(t, gap.MissingThrough-gap.MissingFrom+1, gap.MissingCount)
		require.LessOrEqual(t, gap.LastEventID, uint64(eventSubscriberBufferSize+100))
		select {
		case <-publishDone:
		case <-time.After(time.Second):
			t.Fatal("publisher did not finish after subscriber cancellation")
		}
		for range subscription.Events() {
		}
		require.Equal(t, gap, subscription.Close(), "closing a removed subscriber is idempotent")
	}
}

func mustSubscribeAfter(t *testing.T, session *Session, afterID uint64) *EventSubscription {
	t.Helper()
	subscription, err := session.SubscribeAfter(afterID)
	require.NoError(t, err)
	return subscription
}
