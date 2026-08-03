package core

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentHarnessErrorErrorString(t *testing.T) {
	e := &AgentHarnessError{
		Code:    ErrCodeTimeout,
		Message: "operation timed out",
		Cause:   errors.New("ctx deadline"),
	}
	assert.Contains(t, e.Error(), "timeout")
	assert.Contains(t, e.Error(), "operation timed out")
	assert.Contains(t, e.Error(), "ctx deadline")
}

func TestAgentHarnessErrorErrorNoCause(t *testing.T) {
	e := &AgentHarnessError{
		Code:    ErrCodeBusy,
		Message: "harness is busy",
	}
	assert.Equal(t, "busy: harness is busy", e.Error())
}

func TestAgentHarnessErrorUnwrap(t *testing.T) {
	cause := errors.New("root")
	e := &AgentHarnessError{Code: ErrCodeModelError, Message: "fail", Cause: cause}
	assert.Equal(t, cause, e.Unwrap())
	assert.True(t, errors.Is(e, cause))
}

func TestNormalizeErrorNil(t *testing.T) {
	assert.Nil(t, NormalizeError(nil))
}

func TestNormalizeErrorAlreadyNormalized(t *testing.T) {
	original := &AgentHarnessError{Code: ErrCodeBusy, Message: "busy"}
	got := NormalizeError(original)
	assert.Same(t, original, got)
}

func TestNormalizeErrorDeadlineExceeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Nanosecond)

	got := NormalizeError(ctx.Err())
	require.NotNil(t, got)
	assert.Equal(t, ErrCodeTimeout, got.Code)
}

func TestNormalizeErrorNetTimeout(t *testing.T) {
	// Simulate a network timeout error.
	ne := &netTimeoutError{}
	got := NormalizeError(ne)
	require.NotNil(t, got)
	assert.Equal(t, ErrCodeTimeout, got.Code)
}

func TestNormalizeErrorHookRejected(t *testing.T) {
	cases := []string{
		"hook rejected the run",
		"hook interrupted execution",
		"a hook halted the chain",
	}
	for _, msg := range cases {
		got := NormalizeError(errors.New(msg))
		require.NotNil(t, got, "msg: %s", msg)
		assert.Equal(t, ErrCodeHookRejected, got.Code, "msg: %s", msg)
	}
}

func TestNormalizeErrorBusy(t *testing.T) {
	cases := []string{
		"harness is busy",
		"already running a submission",
	}
	for _, msg := range cases {
		got := NormalizeError(errors.New(msg))
		require.NotNil(t, got, "msg: %s", msg)
		assert.Equal(t, ErrCodeBusy, got.Code, "msg: %s", msg)
	}
}

func TestNormalizeErrorToolError(t *testing.T) {
	cases := []string{
		"tool execution failed",
		"tool error: bash command",
	}
	for _, msg := range cases {
		got := NormalizeError(errors.New(msg))
		require.NotNil(t, got, "msg: %s", msg)
		assert.Equal(t, ErrCodeToolError, got.Code, "msg: %s", msg)
	}
}

func TestNormalizeErrorContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := NormalizeError(ctx.Err())
	require.NotNil(t, got)
	assert.Equal(t, ErrCodeModelError, got.Code)
}

func TestNormalizeErrorDefault(t *testing.T) {
	got := NormalizeError(errors.New("something went wrong"))
	require.NotNil(t, got)
	assert.Equal(t, ErrCodeModelError, got.Code)
	assert.Contains(t, got.Error(), "something went wrong")
}

func TestNormalizeErrorUnwrapChain(t *testing.T) {
	cause := errors.New("connection reset")
	wrapped := errors.Join(errors.New("layer1"), cause)
	got := NormalizeError(wrapped)
	require.NotNil(t, got)
	// The cause should be reachable via Unwrap.
	assert.True(t, errors.Is(got, wrapped) || errors.Is(got, cause))
}

func TestAgentHarnessErrorInterfaceCompliance(t *testing.T) {
	var _ error = (*AgentHarnessError)(nil)
}

// netTimeoutError is a minimal net.Error that reports Timeout() == true.
type netTimeoutError struct{}

func (e *netTimeoutError) Error() string   { return "i/o timeout" }
func (e *netTimeoutError) Timeout() bool   { return true }
func (e *netTimeoutError) Temporary() bool { return false }

// Compile-time assertion that netTimeoutError satisfies net.Error.
var _ net.Error = (*netTimeoutError)(nil)
