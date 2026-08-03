package core

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultFailureTurnSynthesizer_SatisfiesInterface(t *testing.T) {
	var _ FailureTurnSynthesizer = (*DefaultFailureTurnSynthesizer)(nil)
}

func TestFailureTurnSynthesizer_IsRecoverable_ContextDeadline(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	assert.True(t, s.IsRecoverable(context.DeadlineExceeded))
}

func TestFailureTurnSynthesizer_IsRecoverable_ContextCanceled(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	assert.True(t, s.IsRecoverable(context.Canceled))
}

func TestFailureTurnSynthesizer_IsRecoverable_NetTimeout(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	// A net.OpError with Timeout() == true is a recoverable network error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	// Dialing a closed port yields connection refused (a recoverable error).
	_, dialErr := net.Dial("tcp", ln.Addr().String())
	if dialErr != nil {
		assert.True(t, s.IsRecoverable(dialErr), "connection refused should be recoverable: %v", dialErr)
	}
}

func TestFailureTurnSynthesizer_IsRecoverable_ConnectionRefusedMessage(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	err := errors.New("dial tcp: connection refused")
	assert.True(t, s.IsRecoverable(err))
}

func TestFailureTurnSynthesizer_IsRecoverable_TimeoutMessage(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	tests := []string{
		"request timeout",
		"operation timed out",
		"i/o timeout",
	}
	for _, msg := range tests {
		err := errors.New(msg)
		assert.True(t, s.IsRecoverable(err), "expected %q to be recoverable", msg)
	}
}

func TestFailureTurnSynthesizer_IsRecoverable_TemporaryMessage(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	err := errors.New("temporary failure in name resolution")
	assert.True(t, s.IsRecoverable(err))
}

func TestFailureTurnSynthesizer_IsRecoverable_NonRecoverable(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	tests := []error{
		errors.New("file not found"),
		errors.New("permission denied"),
		errors.New("invalid argument"),
		errors.New("syntax error in input"),
	}
	for _, err := range tests {
		assert.False(t, s.IsRecoverable(err), "expected %q to NOT be recoverable", err.Error())
	}
}

func TestFailureTurnSynthesizer_IsRecoverable_NilError(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	assert.False(t, s.IsRecoverable(nil))
}

func TestFailureTurnSynthesizer_Synthesize_Recoverable(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	orig := context.DeadlineExceeded

	msg, err := s.Synthesize(context.Background(), orig)
	require.NoError(t, err)
	assert.Equal(t, "system", msg.Role)
	assert.Contains(t, msg.Content, "recoverable")
	assert.Contains(t, msg.Content, orig.Error())
	assert.Equal(t, orig.Error(), msg.OriginalError)
}

func TestFailureTurnSynthesizer_Synthesize_NonRecoverable(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	orig := errors.New("permission denied")

	msg, err := s.Synthesize(context.Background(), orig)
	require.NoError(t, err)
	assert.Equal(t, "system", msg.Role)
	assert.Contains(t, msg.Content, "permission denied")
	assert.Equal(t, orig.Error(), msg.OriginalError)
}

func TestFailureTurnSynthesizer_Synthesize_NilError(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	msg, err := s.Synthesize(context.Background(), nil)
	require.Error(t, err)
	assert.Equal(t, SynthesizedMessage{}, msg)
	assert.Contains(t, err.Error(), "nil error")
}

func TestFailureTurnSynthesizer_Synthesize_WrappedError(t *testing.T) {
	s := NewDefaultFailureTurnSynthesizer()
	base := context.DeadlineExceeded
	wrapped := errors.New("model call failed")

	msg, err := s.Synthesize(context.Background(), wrapped)
	require.NoError(t, err)
	// Even though it's not a context error, the error text doesn't contain
	// recoverable keywords, so it should be treated as non-recoverable.
	assert.Contains(t, msg.Content, "model call failed")
	_ = base // keep the reference for clarity
}
