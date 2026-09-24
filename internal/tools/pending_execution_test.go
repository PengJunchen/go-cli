package tools

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pendingExecutionErrorStub struct {
	cause   error
	done    <-chan struct{}
	unknown bool
}

var _ PendingExecutionError = (*pendingExecutionErrorStub)(nil)

func (e *pendingExecutionErrorStub) Error() string { return "pending execution: " + e.cause.Error() }

func (e *pendingExecutionErrorStub) Unwrap() error { return e.cause }

func (e *pendingExecutionErrorStub) OutcomeUnknown() bool { return e.unknown }

func (e *pendingExecutionErrorStub) ExecutionDone() <-chan struct{} { return e.done }

func TestPendingExecutionErrorErrorsAsContract(t *testing.T) {
	cause := errors.New("command canceled")
	done := make(chan struct{})
	close(done)
	want := &pendingExecutionErrorStub{
		cause:   cause,
		done:    done,
		unknown: true,
	}

	wrapped := fmt.Errorf("tool wrapper: %w", want)
	var pending PendingExecutionError
	require.ErrorAs(t, wrapped, &pending)
	assert.True(t, pending.OutcomeUnknown())
	select {
	case <-pending.ExecutionDone():
	default:
		t.Fatal("execution completion receipt should be observable")
	}
	require.ErrorIs(t, wrapped, cause)
}
