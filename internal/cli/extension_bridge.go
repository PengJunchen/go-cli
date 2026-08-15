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
			"result":     result,
			"error":      runErr,
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

// bridgeStreamCtxKey is the context key used to thread the core.EventStream
// through the extension AgentFunc boundary. The extension package cannot
// import core, so the stream is carried via context rather than as a field
// on AgentInput.
type bridgeStreamCtxKey struct{}

// Wrap composes the extension middleware over the given core.AgentLoop. The
// loop is adapted into an extension.AgentFunc, wrapped by the extension
// middleware, and re-exposed as an AgentLoop.
func (a *extensionMiddlewareAdapter) Wrap(next core.AgentLoop) core.AgentLoop {
	// agentFunc adapts the core.AgentLoop into an extension.AgentFunc. It
	// recovers the EventStream from context (set by extensionMiddlewareLoop.Run)
	// and forwards it to the inner loop so real-time streaming is preserved.
	agentFunc := func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		var es core.EventStream
		if s, ok := ctx.Value(bridgeStreamCtxKey{}).(core.EventStream); ok {
			es = s
		}

		var history []core.AgentMessage
		if h, ok := input.History.([]core.AgentMessage); ok {
			history = h
		}

		var events []core.AgentEvent
		var err error
		if es != nil {
			events, err = next.Run(ctx, core.Submission{
				Type:     submissionTypeFromString(input.Type),
				Content:  input.Message,
				Metadata: toMetadata(input.Data),
				History:  history,
			}, es)
		} else {
			events, err = next.Run(ctx, core.Submission{
				Type:     submissionTypeFromString(input.Type),
				Content:  input.Message,
				Metadata: toMetadata(input.Data),
				History:  history,
			})
		}
		if err != nil {
			return extension.AgentOutput{}, err
		}
		return extension.AgentOutput{
			Text:   lastBridgeMessage(events),
			Data:   input.Data,
			Events: events,
		}, nil
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

func (l *extensionMiddlewareLoop) Run(ctx context.Context, submission core.Submission, stream ...core.EventStream) ([]core.AgentEvent, error) {
	// Thread the EventStream through context so the inner agentFunc can
	// forward it to the wrapped core.AgentLoop, preserving real-time
	// streaming.
	if len(stream) > 0 && stream[0] != nil {
		ctx = context.WithValue(ctx, bridgeStreamCtxKey{}, stream[0])
	}
	input := extension.AgentInput{
		Message: submission.Content,
		Data:    submission.Metadata,
		History: submission.History,
		Type:    submission.Type.String(),
	}
	out, err := l.fn(ctx, input)
	if err != nil {
		return nil, err
	}
	return bridgeOutputToEvents(out), nil
}

// bridgeOutputToEvents converts an extension.AgentOutput back into a slice of
// core.AgentEvent. When the middleware chain preserved the inner loop's events
// (via AgentOutput.Events), those are returned directly so intermediate
// streaming fragments, tool calls, and other event kinds are not lost. If the
// middleware modified the final text, the last "message" event is updated to
// reflect the change. When no events survived (e.g. the middleware built a
// fresh AgentOutput with only Text), a single message event is synthesized.
func bridgeOutputToEvents(out extension.AgentOutput) []core.AgentEvent {
	if events, ok := out.Events.([]core.AgentEvent); ok && len(events) > 0 {
		if out.Text != "" {
			for i := len(events) - 1; i >= 0; i-- {
				if events[i].Kind == "message" {
					events[i].Content = out.Text
					break
				}
			}
		}
		return events
	}
	var events []core.AgentEvent
	if out.Text != "" {
		events = append(events, core.AgentEvent{Kind: "message", Content: out.Text, Timestamp: time.Now()})
	}
	return events
}

// submissionTypeFromString converts the string representation of a submission
// type back to core.SubmissionType. Unknown values default to
// SubmissionUserMessage.
func submissionTypeFromString(s string) core.SubmissionType {
	switch s {
	case "steering":
		return core.SubmissionSteering
	case "followup":
		return core.SubmissionFollowUp
	default:
		return core.SubmissionUserMessage
	}
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
