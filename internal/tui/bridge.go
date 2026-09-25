package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
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
// suitable for consumption by the TUI render loop. It preserves the event
// fields consumed by the TUI, derives ContentType from Kind, and forwards
// token usage plus safe tool-call details when present.
func CoreEventToAgentEvent(ev core.AgentEvent) AgentEvent {
	ae := AgentEvent{
		Type:        ev.Kind,
		Content:     ev.Content,
		ContentType: KindToContentType(ev.Kind),
		Incremental: ev.Incremental,
		ToolCallID:  ev.ToolCallID,
		Stream:      ev.Stream,
		IsError:     ev.IsError,
		ToolCalls:   copyToolCalls(ev.ToolCalls),
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

// copyToolCalls validates assistant tool-call arguments as JSON payloads and
// then uses core's canonical history clone to prevent aliases to nested
// maps/slices from crossing the core-to-TUI boundary. Invalid or unsupported
// values remain visible as calls, with their details explicitly unavailable.
func copyToolCalls(calls []llm.ToolCall) []ToolCallData {
	if calls == nil {
		return nil
	}

	copied := make([]ToolCallData, len(calls))
	for i, call := range calls {
		copied[i] = ToolCallData{ID: call.ID, Name: call.Name}
		if call.Args == nil {
			continue
		}

		if _, err := json.Marshal(call.Args); err != nil || !isToolArgsJSONValue(reflect.ValueOf(call.Args)) {
			copied[i].ArgsUnavailable = true
			continue
		}
		messages := core.CloneAgentMessages([]core.AgentMessage{{ToolCalls: []llm.ToolCall{call}}})
		copied[i].Args = messages[0].ToolCalls[0].Args
	}
	return copied
}

// isToolArgsJSONValue restricts cloning to the map/slice/scalar graphs used by
// decoded JSON payloads. Structs and pointers may hide mutable aliases behind
// custom encoders, so they are reported unavailable rather than shallow-copied.
// json.Marshal is run first to reject cycles and unsupported values.
func isToolArgsJSONValue(value reflect.Value) bool {
	if !value.IsValid() {
		return true
	}
	typeOfValue := value.Type()
	marshalerType := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	if typeOfValue.Implements(marshalerType) ||
		(typeOfValue.Kind() != reflect.Pointer && reflect.PointerTo(typeOfValue).Implements(marshalerType)) {
		return false
	}

	switch value.Kind() {
	case reflect.Interface:
		return value.IsNil() || isToolArgsJSONValue(value.Elem())
	case reflect.Map:
		if value.IsNil() {
			return true
		}
		if value.Type().Key().Kind() != reflect.String {
			return false
		}
		iter := value.MapRange()
		for iter.Next() {
			if !isToolArgsJSONValue(iter.Value()) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return true
		}
		for i := 0; i < value.Len(); i++ {
			if !isToolArgsJSONValue(value.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
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
