package tui

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/core"
)

// ContentTypeSubagent is the content type for events forwarded from a
// sub-agent. Sub-agent content is rendered with indentation and a prefix so
// the user can visually distinguish parent and child agent activity.
const ContentTypeSubagent = "subagent"

// subagentIndent is the prefix applied to each line of sub-agent event
// content so it is visually nested under the parent's dispatch_subagent call.
const subagentIndent = "  "

// KindToContentType maps a core.AgentEvent.Kind to the TUI content type used
// for renderer dispatch. Unknown kinds fall back to ContentTypeStatus.
func KindToContentType(kind string) string {
	switch kind {
	case "message":
		return ContentTypeAssistant
	case "tool_call":
		return ContentTypeToolCall
	case "tool_result":
		return ContentTypeToolResult
	case "tool_output":
		return ContentTypeToolOutput
	case "error":
		return ContentTypeError
	case "done":
		return ContentTypeStatus
	case "thinking":
		return ContentTypeThinking
	case "token_usage":
		return ContentTypeStatus
	default:
		return ContentTypeStatus
	}
}

// CoreEventToAgentEvent converts a core.AgentEvent into a tui.AgentEvent
// suitable for consumption by the TUI render loop. The mapping is lossless:
// every core field is preserved, the ContentType is derived from Kind, and
// token usage data is forwarded when present.
func CoreEventToAgentEvent(ev core.AgentEvent) AgentEvent {
	ae := AgentEvent{
		Type:        ev.Kind,
		Content:     ev.Content,
		ContentType: KindToContentType(ev.Kind),
		Incremental: ev.Incremental,
		ToolCallID:  ev.ToolCallID,
		Stream:      ev.Stream,
		IsError:     ev.IsError,
	}
	if ev.TokenUsage != nil {
		ae.TokenUsage = &TokenUsageData{
			InputTokens:  ev.TokenUsage.InputTokens,
			OutputTokens: ev.TokenUsage.OutputTokens,
			MaxTokens:    ev.TokenUsage.MaxTokens,
			Cost:         ev.TokenUsage.Cost,
		}
	}
	return ae
}

// SubagentEventToAgentEvent converts a core.AgentEvent emitted by a sub-agent
// into a tui.AgentEvent with subagent content type. The taskID is prepended to
// the content and each line is indented so the sub-agent's activity is
// visually nested under the parent's dispatch_subagent tool call.
func SubagentEventToAgentEvent(taskID string, ev core.AgentEvent) AgentEvent {
	prefix := fmt.Sprintf("[subagent:%s] ", taskID)
	indented := indentLines(ev.Content, subagentIndent)
	return AgentEvent{
		Type:        ev.Kind,
		Content:     prefix + indented,
		ContentType: ContentTypeSubagent,
		Incremental: ev.Incremental,
	}
}

// indentLines prefixes every line of s with indent. This ensures multi-line
// sub-agent output (e.g. code blocks) is visually nested.
func indentLines(s, indent string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}

// BridgeEvents drains a core.EventStream and forwards each event as a
// tui.AgentEvent on the returned channel. The channel closes when the stream
// closes or the context is canceled. BridgeEvents is the single integration
// point between the core runtime and the TUI layer.
func BridgeEvents(ctx context.Context, stream core.EventStream) <-chan AgentEvent {
	ch := make(chan AgentEvent, 64)
	go func() {
		defer close(ch)
		for {
			select {
			case <-ctx.Done():
				slog.Debug("tui.bridge.canceled", "err", ctx.Err())
				return
			case ev, ok := <-stream.Events():
				if !ok {
					slog.Debug("tui.bridge.stream_closed")
					return
				}
				select {
				case ch <- CoreEventToAgentEvent(ev):
				case <-ctx.Done():
					slog.Debug("tui.bridge.canceled_send", "err", ctx.Err())
					return
				}
			}
		}
	}()
	return ch
}
