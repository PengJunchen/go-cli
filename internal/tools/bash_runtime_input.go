package tools

import (
	"context"
	"strings"
	"time"
)

// ProcessStartGuard atomically authorizes release of a managed Bash process.
// Implementations must invoke start at most once, synchronously.
type ProcessStartGuard func(context.Context, func() error) error

type bashRuntimeInputKey struct{}
type bashRuntimeInput struct {
	stdin               string
	guard               ProcessStartGuard
	executionTimeout    time.Duration
	hasExecutionTimeout bool
}

// WithBashRuntimeInput attaches immutable host-supplied stdin and a process
// release guard to a single runtime-owned Bash invocation. Neither is accepted
// from tool arguments or inherited by subsequent tool calls.
func WithBashRuntimeInput(ctx context.Context, stdin string, guard ProcessStartGuard) context.Context {
	return context.WithValue(ctx, bashRuntimeInputKey{}, bashRuntimeInput{stdin: stdin, guard: guard})
}

func bashRuntimeInputFromContext(ctx context.Context) (bashRuntimeInput, bool) {
	input, ok := ctx.Value(bashRuntimeInputKey{}).(bashRuntimeInput)
	return input, ok
}

func runtimeBashStdin(ctx context.Context) *strings.Reader {
	input, _ := bashRuntimeInputFromContext(ctx)
	return strings.NewReader(input.stdin)
}

// WithBashRuntimeExecutionTimeout bounds only the managed Bash execution. It
// leaves approval and preflight on the caller's context, and never increases
// the tool's own timeout. The value remains host-only runtime input.
func WithBashRuntimeExecutionTimeout(ctx context.Context, timeout time.Duration) context.Context {
	input, _ := bashRuntimeInputFromContext(ctx)
	input.executionTimeout = timeout
	input.hasExecutionTimeout = true
	return context.WithValue(ctx, bashRuntimeInputKey{}, input)
}

func runtimeBashTimeout(ctx context.Context, configured time.Duration) time.Duration {
	if input, present := bashRuntimeInputFromContext(ctx); present && input.hasExecutionTimeout && input.executionTimeout < configured {
		return input.executionTimeout
	}
	return configured
}
