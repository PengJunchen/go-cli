package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/extension"
)

// This file bridges the extension package's Hook and Middleware types into the
// core runtime's Hook and Middleware types. The two systems use different
// contracts (extension.Hook uses a single Handle(HookEvent) method while
// core.Hook uses BeforeRun/AfterRun; extension.Middleware wraps an AgentFunc
// while core.Middleware wraps an AgentLoop), so adapters translate between
// them. The adapters live in the cli package because core already imports
// extension, so the extension package cannot depend on core without creating
// an import cycle.

// extensionHookAdapter bridges an extension.Hook (event-style Handle) into a
// core.Hook (BeforeRun/AfterRun). The extension hook's Handle is invoked with a
// synthesized HookEvent for each lifecycle phase. A non-pass HookResult halts
// the run by surfacing as an error.
type extensionHookAdapter struct {
	extHook extension.Hook
}

var _ core.Hook = (*extensionHookAdapter)(nil)

// newExtensionHookAdapter wraps an extension.Hook as a core.Hook.
func newExtensionHookAdapter(h extension.Hook) *extensionHookAdapter {
	return &extensionHookAdapter{extHook: h}
}

// Name delegates to the underlying extension hook.
func (a *extensionHookAdapter) Name() string { return a.extHook.Name() }

// BeforeRun forwards an agent.before_run event to the extension hook. A non-pass
// HookResult halts the run by returning an error carrying the hook name and
// reason.
func (a *extensionHookAdapter) BeforeRun(ctx context.Context, submission core.Submission) error {
	event := extension.HookEvent{
		Name:      "agent.before_run",
		Data:      submission,
		Source:    "extension-bridge",
		Timestamp: time.Now(),
	}
	result := a.extHook.Handle(ctx, event)
	if result.Action != extension.HookActionPass {
		return fmt.Errorf("extension hook %s halted before-run: %s", a.extHook.Name(), result.Reason)
	}
	return nil
}

// AfterRun forwards an agent.after_run event to the extension hook so it can
// observe the completed run. A non-pass HookResult surfaces as an error.
func (a *extensionHookAdapter) AfterRun(ctx context.Context, submission core.Submission, result core.Result, runErr error) error {
	event := extension.HookEvent{
		Name: "agent.after_run",
		Data: map[string]any{
			"result":   result,
			"error":    runErr,
			"submission": submission,
		},
		Source:    "extension-bridge",
		Timestamp: time.Now(),
	}
	hResult := a.extHook.Handle(ctx, event)
	if hResult.Action != extension.HookActionPass {
		return fmt.Errorf("extension hook %s halted after-run: %s", a.extHook.Name(), hResult.Reason)
	}
	return nil
}

// extensionMiddlewareAdapter bridges an extension.Middleware (which wraps an
// AgentFunc) into a core.Middleware (which wraps an AgentLoop). The adapter
// translates between the Submission/AgentEvent and AgentInput/AgentOutput
// models so extension middleware participates in the runtime onion chain.
type extensionMiddlewareAdapter struct {
	extMW extension.Middleware
}

var _ core.Middleware = (*extensionMiddlewareAdapter)(nil)

// newExtensionMiddlewareAdapter wraps an extension.Middleware as a core.Middleware.
func newExtensionMiddlewareAdapter(m extension.Middleware) *extensionMiddlewareAdapter {
	return &extensionMiddlewareAdapter{extMW: m}
}

// Name delegates to the underlying extension middleware.
func (a *extensionMiddlewareAdapter) Name() string { return a.extMW.Name() }

// Wrap composes the extension middleware over the given core.AgentLoop. The
// loop is adapted into an extension.AgentFunc, wrapped by the extension
// middleware, and re-exposed as an AgentLoop.
func (a *extensionMiddlewareAdapter) Wrap(next core.AgentLoop) core.AgentLoop {
	// agentFunc adapts the core.AgentLoop into an extension.AgentFunc.
	agentFunc := func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		events, err := next.Run(ctx, core.Submission{
			Type:     core.SubmissionUserMessage,
			Content:  input.Message,
			Metadata: toMetadata(input.Data),
		})
		if err != nil {
			return extension.AgentOutput{}, err
		}
		return extension.AgentOutput{Text: lastBridgeMessage(events), Data: input.Data}, nil
	}

	wrapped := a.extMW.WrapAgent(agentFunc)
	return &extensionMiddlewareLoop{fn: wrapped}
}

// extensionMiddlewareLoop is the core.AgentLoop produced by
// extensionMiddlewareAdapter.Wrap. It translates a Submission into an
// AgentInput, invokes the wrapped extension.AgentFunc, and converts the
// AgentOutput back into agent events.
type extensionMiddlewareLoop struct {
	fn extension.AgentFunc
}

func (l *extensionMiddlewareLoop) Run(ctx context.Context, submission core.Submission, _ ...core.EventStream) ([]core.AgentEvent, error) {
	input := extension.AgentInput{
		Message: submission.Content,
		Data:    submission.Metadata,
	}
	out, err := l.fn(ctx, input)
	if err != nil {
		return nil, err
	}
	var events []core.AgentEvent
	if out.Text != "" {
		events = append(events, core.AgentEvent{Kind: "message", Content: out.Text, Timestamp: time.Now()})
	}
	return events, nil
}

// toMetadata coerces an opaque extension.AgentInput.Data value into the
// map[string]any expected by core.Submission.Metadata. Non-map values yield nil.
func toMetadata(data any) map[string]any {
	if m, ok := data.(map[string]any); ok {
		return m
	}
	return nil
}

// lastBridgeMessage returns the content of the final non-empty "message" event,
// or the empty string if none exists.
func lastBridgeMessage(events []core.AgentEvent) string {
	final := ""
	for _, ev := range events {
		if ev.Kind == "message" && ev.Content != "" {
			final = ev.Content
		}
	}
	return final
}
