package tools

import "context"

// ProcessExecutionIdentity identifies one locally started process group. The
// start token and host generation prevent a restored observer from treating a
// reused PID as the original child.
type ProcessExecutionIdentity struct {
	Platform               string
	HostGeneration         string
	LeaderPID              int
	ProcessStartToken      string
	ProcessGroupID         int
	OwnerPID               int
	OwnerProcessStartToken string
}

// ProcessExecutionIdentityObserver synchronously persists an exact process
// identity before a prepared Bash command is released to run user code.
type ProcessExecutionIdentityObserver func(context.Context, ProcessExecutionIdentity) error

type processExecutionIdentityObserverContextKey struct{}

// WithProcessExecutionIdentityObserver binds a prepared execution to a
// synchronous identity persistence callback. The callback is carried through
// tool wrappers by context and is used only by managed local Bash execution.
func WithProcessExecutionIdentityObserver(ctx context.Context, observer ProcessExecutionIdentityObserver) context.Context {
	if ctx == nil || observer == nil {
		return ctx
	}
	return context.WithValue(ctx, processExecutionIdentityObserverContextKey{}, observer)
}

func processExecutionIdentityObserverFromContext(ctx context.Context) ProcessExecutionIdentityObserver {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(processExecutionIdentityObserverContextKey{}).(ProcessExecutionIdentityObserver)
	return observer
}
