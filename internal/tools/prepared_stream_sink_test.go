package tools

import (
	"testing"

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
