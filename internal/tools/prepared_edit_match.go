package tools

import (
	"context"
	"errors"
)

type preparedEditMatchKey struct{}
type preparedEditExecutingKey struct{}

type preparedEditMatchError struct {
	cause      *EditMatchError
	invocation *preparedInvocation
}

func (e *preparedEditMatchError) Error() string { return e.cause.Error() }
func (e *preparedEditMatchError) Unwrap() error { return e.cause }

func stockPreparedEdit(invocation PreparedInvocation) *preparedInvocation {
	p, ok := invocation.(*preparedInvocation)
	if !ok || p == nil || p.identity.RegistrationSource() != RegistrationSourceBuiltin ||
		p.identity.ToolName() != "edit" ||
		(p.completionProof != CompletionInlinePipelineJoined && p.completionProof != CompletionQueuedJobJoined) {
		return nil
	}
	return p
}

func withPreparedEditInvocation(ctx context.Context, invocation PreparedInvocation) context.Context {
	if p := stockPreparedEdit(invocation); p != nil && p.DispatchCommitted() {
		return context.WithValue(ctx, preparedEditMatchKey{}, p)
	}
	return ctx
}

func withPreparedEditExecution(ctx context.Context, definition ToolDefinition) context.Context {
	if _, ok := definition.(*EditFileTool); !ok {
		return ctx
	}
	if p, ok := ctx.Value(preparedEditMatchKey{}).(*preparedInvocation); ok && stockPreparedEdit(p) == p && p.DispatchCommitted() {
		return context.WithValue(ctx, preparedEditExecutingKey{}, p)
	}
	return ctx
}

func bindPreparedEditMatch(ctx context.Context, failure *EditMatchError) error {
	if ctx != nil {
		if p, ok := ctx.Value(preparedEditExecutingKey{}).(*preparedInvocation); ok && stockPreparedEdit(p) == p && p.DispatchCommitted() {
			return &preparedEditMatchError{cause: failure, invocation: p}
		}
	}
	return failure
}

// PreparedEditMatchKnown reports whether err is a mismatch from the exact
// completed stock edit invocation. It does not itself authorize a retry.
func PreparedEditMatchKnown(invocation PreparedInvocation, err error) bool {
	p := stockPreparedEdit(invocation)
	if p == nil || !p.DispatchCommitted() {
		return false
	}
	proof := p.CompletionSignal().Proof()
	if proof != CompletionInlinePipelineJoined && proof != CompletionQueuedJobJoined {
		return false
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if _, joined := current.(interface{ Unwrap() []error }); joined {
			return false
		}
		if unknown, ok := current.(interface{ OutcomeUnknown() bool }); ok && unknown.OutcomeUnknown() {
			return false
		}
		if failure, ok := current.(*preparedEditMatchError); ok {
			return failure != nil && failure.invocation == p && failure.cause != nil && failure.cause.MatchFailureKind() != ""
		}
	}
	return false
}
