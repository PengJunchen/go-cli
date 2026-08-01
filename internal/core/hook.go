package core

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// HookResult is returned by a hook chain's Before phase. Continue indicates
// whether the run should proceed; when false the chain halts the run.
type HookResult struct {
	// Continue reports whether the run may proceed after the hook chain.
	Continue bool
	// Output is an optional message the hook produced (e.g. an interruption
	// reason or a note for the user).
	Output string
}

// ContinueHookResult is a convenience value for a hook that lets the run
// proceed.
var ContinueHookResult = HookResult{Continue: true}

// InterruptHookResult is a convenience value for a hook that halts the run.
// Use NewInterruptHookResult to attach a reason message.
var InterruptHookResult = HookResult{Continue: false}

// NewInterruptHookResult builds an interrupting HookResult with a reason.
func NewInterruptHookResult(output string) HookResult {
	return HookResult{Continue: false, Output: output}
}

// Interrupted reports whether the HookResult halts the run.
func (r HookResult) Interrupted() bool { return !r.Continue }

// HookChain invokes a list of Hooks in order around a run. Before calls each
// hook's BeforeRun until one halts the chain; After calls each hook's AfterRun.
// Hooks are invoked sequentially (no internal locking needed).
type HookChain struct {
	hooks []Hook
}

// NewHookChain builds a HookChain over the given hooks, applied in order.
func NewHookChain(hooks ...Hook) *HookChain {
	return &HookChain{hooks: append([]Hook{}, hooks...)}
}

// Hooks returns a copy of the hooks in application order.
func (c *HookChain) Hooks() []Hook { return append([]Hook{}, c.hooks...) }

// Before runs each hook's BeforeRun in order. It returns Continue == false when
// a hook halts the run (Continue == false) or returns an error; the error is
// returned alongside so callers know exactly why the chain stopped. When every
// hook passes, it returns ContinueHookResult.
func (c *HookChain) Before(ctx context.Context, submission Submission) (HookResult, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "hook.before", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, nil)
	slog.Info("core.hookchain.before", "hooks", len(c.hooks), "type", submission.Type)

	for _, h := range c.hooks {
		if err := h.BeforeRun(spanCtx, submission); err != nil {
			slog.Info("core.hookchain.before.halt", "hook", h.Name(), "err", err)
			logger.Info("hook.before.halt", "hook", h.Name(), "err", err)
			return NewInterruptHookResult(err.Error()), err
		}
	}
	return ContinueHookResult, nil
}

// After runs each hook's AfterRun in application order after a run completes.
// A hook returning an error is logged but does not stop the remaining hooks, so
// every hook observes the run outcome. Its return is the first error observed,
// if any.
func (c *HookChain) After(ctx context.Context, submission Submission, result Result, err error) error {
	span, spanCtx := tracing.SpanFromContext(ctx, "hook.after", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, nil)
	slog.Info("core.hookchain.after", "hooks", len(c.hooks), "error", err != nil)

	var firstErr error
	for _, h := range c.hooks {
		if aerr := h.AfterRun(spanCtx, submission, result, err); aerr != nil {
			slog.Info("core.hookchain.after.error", "hook", h.Name(), "err", aerr)
			logger.Info("hook.after.error", "hook", h.Name(), "err", aerr)
			if firstErr == nil {
				firstErr = aerr
			}
		}
	}
	return firstErr
}
