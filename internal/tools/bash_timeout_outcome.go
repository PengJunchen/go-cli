package tools

import (
	"context"
	"errors"
)

// preparedBashTimeoutError is an interrupted, failed local command, not a
// rollback guarantee. Only an ordinary model/provider stock Bash invocation may use
// it for recovery, and only after that current sealed invocation's actual process proof.
// Runtime-owned host actions are excluded even though they use the same tool.
type preparedBashTimeoutError struct {
	cause      error
	invocation *preparedInvocation
}

func (e *preparedBashTimeoutError) Error() string      { return e.cause.Error() }
func (e *preparedBashTimeoutError) Unwrap() error      { return e.cause }
func (*preparedBashTimeoutError) OutcomeUnknown() bool { return false }

type preparedBashInvocationKey struct{}

func stockPreparedBash(invocation PreparedInvocation) *preparedInvocation {
	var p *preparedInvocation
	switch value := invocation.(type) {
	case *preparedInvocation:
		p = value
	case *preparedStreamingInvocation:
		if value != nil {
			p = value.preparedInvocation
		}
	}
	if p == nil || p.identity.RegistrationSource() != RegistrationSourceBuiltin || p.identity.ToolName() != "bash" || p.completionProof != completionLocalProcessOrPrestartNoProcessJoined {
		return nil
	}
	return p
}

func withPreparedBashInvocation(ctx context.Context, invocation PreparedInvocation) context.Context {
	if p := stockPreparedBash(invocation); p != nil && p.DispatchCommitted() {
		return context.WithValue(ctx, preparedBashInvocationKey{}, p)
	}
	return ctx
}

func bashLifecycleError(parent, own context.Context, cause error, result managedLocalResult) error {
	// Host-owned runtime inputs (including configured post-tool hooks) retain
	// their existing stop policy. Only ordinary model/provider stock Bash
	// actions may recover from their own execution window; call IDs confer no authority.
	runtimeOwned := false
	if parent != nil {
		_, runtimeOwned = bashRuntimeInputFromContext(parent)
	}
	if !runtimeOwned && parent != nil && own != nil && parent.Err() == nil && errors.Is(own.Err(), context.DeadlineExceeded) && result.containmentProven && result.quiescent && result.readersJoined && !result.forcedPipeClose && result.cleanupErr == nil && result.readerErr == nil {
		if invocation, ok := parent.Value(preparedBashInvocationKey{}).(*preparedInvocation); ok && stockPreparedBash(invocation) == invocation && invocation.DispatchCommitted() {
			return &preparedBashTimeoutError{cause: cause, invocation: invocation}
		}
	}
	return localLifecycleError(cause, result)
}

// IsPreparedBashOwnTimeout only recognizes the private error class. It is NOT
// recovery authority: legacy callers must keep it unknown, and durable callers
// must validate the current sealed invocation with PreparedBashOwnTimeoutKnown.
func IsPreparedBashOwnTimeout(err error) bool {
	var timeout *preparedBashTimeoutError
	return errors.As(err, &timeout)
}

// PreparedBashOwnTimeoutKnown recognizes only ordinary model/provider stock
// Bash actions; runtime-owned host inputs/timeouts never mint this class.
// It binds the failure to this consumed invocation and actual closed proof. A genuine error
// borrowed from another action, a custom registration, an unknown/joined error,
// or revoked/missing completion proof cannot grant recovery.
func PreparedBashOwnTimeoutKnown(invocation PreparedInvocation, err error) bool {
	p := stockPreparedBash(invocation)
	if p == nil || !p.DispatchCommitted() || p.CompletionSignal().Proof() != CompletionOwnedProcessGroupJoined {
		return false
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if _, joined := current.(interface{ Unwrap() []error }); joined {
			return false
		}
		if unknown, ok := current.(interface{ OutcomeUnknown() bool }); ok && unknown.OutcomeUnknown() {
			return false
		}
		if timeout, ok := current.(*preparedBashTimeoutError); ok {
			return timeout != nil && timeout.invocation == p
		}
	}
	return false
}
