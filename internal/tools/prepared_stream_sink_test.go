package tools

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type nonComparablePreparedStreamSink struct {
	values []string
}

func (s nonComparablePreparedStreamSink) Send(content, toolCallID, stream string) error {
	return nil
}

func TestAttestSynchronousStreamSinkRejectsNilAndTypedNil(t *testing.T) {
	_, err := AttestSynchronousStreamSink(nil)
	require.ErrorIs(t, err, ErrPreparedStreamSinkUnavailable)

	var sink *preparedCaptureSink
	_, err = AttestSynchronousStreamSink(sink)
	require.ErrorIs(t, err, ErrPreparedStreamSinkUnavailable)
}

func TestAttestSynchronousStreamSinkForwardsToExactSink(t *testing.T) {
	sink := &preparedCaptureSink{}
	attested, err := AttestSynchronousStreamSink(sink)
	require.NoError(t, err)
	require.NoError(t, attested.Send("line", "call-1", "stdout"))
	assert.Equal(t, "line", sink.content)
	assert.Equal(t, "call-1", sink.callID)
	assert.Equal(t, "stdout", sink.stream)
}

func TestAttestedSinkIdentityCheckDoesNotCompareArbitraryInterfaces(t *testing.T) {
	captured, err := AttestSynchronousStreamSink(&preparedCaptureSink{})
	require.NoError(t, err)
	capturedConcrete := captured.(*synchronousStreamSink)

	var nonComparable StreamSink = nonComparablePreparedStreamSink{values: []string{"uncomparable"}}
	assert.False(t, isCapturedSynchronousStreamSink(capturedConcrete, nonComparable))
}

type preparedConcurrentProbeSink struct {
	active          atomic.Int32
	overlap         atomic.Bool
	firstEntry      chan struct{}
	secondEntry     chan struct{}
	releaseFirst    chan struct{}
	firstEntryOnce  sync.Once
	secondEntryOnce sync.Once
}

func (s *preparedConcurrentProbeSink) Send(string, string, string) error {
	if active := s.active.Add(1); active > 1 {
		s.overlap.Store(true)
		s.secondEntryOnce.Do(func() { close(s.secondEntry) })
	}
	defer s.active.Add(-1)
	s.firstEntryOnce.Do(func() {
		close(s.firstEntry)
		<-s.releaseFirst
	})
	return nil
}

func TestAttestedSynchronousStreamSinkSerializesConcurrentSend(t *testing.T) {
	inner := &preparedConcurrentProbeSink{
		firstEntry:   make(chan struct{}),
		secondEntry:  make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(inner.releaseFirst) }) }
	t.Cleanup(releaseFirst)

	attested, err := AttestSynchronousStreamSink(inner)
	require.NoError(t, err)
	start := make(chan struct{})
	attempted := make(chan struct{}, 2)
	results := make(chan error, 2)
	var ready sync.WaitGroup
	for range 2 {
		ready.Add(1)
		go func() {
			ready.Done()
			<-start
			attempted <- struct{}{}
			results <- attested.Send("line", "call-1", "stdout")
		}()
	}
	ready.Wait()
	close(start)
	select {
	case <-inner.firstEntry:
	case <-time.After(time.Second):
		t.Fatal("the attested sink did not enter Send")
	}
	<-attempted
	<-attempted

	concurrentEntry := false
	select {
	case <-inner.secondEntry:
		concurrentEntry = true
	case <-time.After(25 * time.Millisecond):
	}
	releaseFirst()
	for range 2 {
		select {
		case err := <-results:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("serialized sink calls did not return after release")
		}
	}
	assert.False(t, concurrentEntry, "the wrapper must not enter the underlying sink concurrently")
	assert.False(t, inner.overlap.Load(), "the host assertion does not imply underlying sink concurrency safety")
}
