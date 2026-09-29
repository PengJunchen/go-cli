package tui

import (
	"context"
	"encoding"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"unicode/utf8"

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
		Type:           ev.Kind,
		Content:        ev.Content,
		ContentType:    KindToContentType(ev.Kind),
		Incremental:    ev.Incremental,
		ToolCallID:     ev.ToolCallID,
		Stream:         ev.Stream,
		IsError:        ev.IsError,
		RunID:          ev.RunID,
		ReceiptOutcome: ev.ReceiptOutcome,
		ToolCalls:      copyToolCalls(ev.ToolCalls),
	}
	if ev.CanonicalTranscriptIndex != nil {
		index := *ev.CanonicalTranscriptIndex
		ae.CanonicalTranscriptIndex = &index
	}
	if ev.AssistantTranscriptOrdinal != nil {
		ordinal := *ev.AssistantTranscriptOrdinal
		ae.AssistantTranscriptOrdinal = &ordinal
	}
	if ev.SourceTruncated != nil {
		truncated := *ev.SourceTruncated
		ae.SourceTruncated = &truncated
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

// copyToolCalls copies only JSON-compatible argument values into presentation-
// owned maps and slices. It does not invoke caller-provided encoder methods.
// Invalid or unsupported values remain visible as calls, with their details
// explicitly unavailable.
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

		budget := toolArgCloneBudget{
			remainingNodes: maxToolArgCloneNodes,
			remainingBytes: maxToolArgCloneJSONBytes,
		}
		args, ok := cloneToolArgValue(reflect.ValueOf(call.Args), 0, &budget)
		if !ok {
			copied[i].ArgsUnavailable = true
			continue
		}
		copied[i].Args = args
	}
	return copied
}

const (
	maxToolArgCloneDepth     = 128
	maxToolArgCloneNodes     = 10000
	maxToolArgCloneJSONBytes = 64 * 1024
)

type toolArgCloneBudget struct {
	remainingNodes int
	remainingBytes int
}

func (budget *toolArgCloneBudget) chargeBytes(size int) bool {
	if size < 0 || size > budget.remainingBytes {
		return false
	}
	budget.remainingBytes -= size
	return true
}

// cloneToolArgValue accepts a bounded JSON scalar/map/slice tree and returns a
// presentation-owned clone. Structs, pointers, cycles/deep trees, and custom
// encoders are rejected without invoking caller-provided methods.
func cloneToolArgValue(value reflect.Value, depth int, budget *toolArgCloneBudget) (any, bool) {
	if !value.IsValid() {
		if !budget.chargeBytes(len("null")) {
			return nil, false
		}
		return nil, true
	}
	if depth > maxToolArgCloneDepth || budget.remainingNodes <= 0 {
		return nil, false
	}
	budget.remainingNodes--

	typeOfValue := value.Type()
	marshalerType := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType := reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	if typeOfValue.Implements(marshalerType) || typeOfValue.Implements(textMarshalerType) {
		return nil, false
	}
	if typeOfValue.Kind() != reflect.Pointer {
		pointerType := reflect.PointerTo(typeOfValue)
		if pointerType.Implements(marshalerType) || pointerType.Implements(textMarshalerType) {
			return nil, false
		}
	}

	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			if !budget.chargeBytes(len("null")) {
				return nil, false
			}
			return nil, true
		}
		return cloneToolArgValue(value.Elem(), depth+1, budget)
	case reflect.Map:
		if value.IsNil() {
			if !budget.chargeBytes(len("null")) {
				return nil, false
			}
			return map[string]any(nil), true
		}
		if value.Type().Key().Kind() != reflect.String || value.Len() > budget.remainingNodes {
			return nil, false
		}
		if !chargeToolArgObjectFraming(value.Len(), budget) {
			return nil, false
		}
		// Check all key lengths before allocating the cloned map. String lengths
		// are checked against the remaining encoded-byte budget without copying
		// or JSON-encoding potentially oversized input.
		keyIter := value.MapRange()
		for keyIter.Next() {
			if !chargeToolArgJSONString(keyIter.Key().String(), budget) {
				return nil, false
			}
		}
		cloned := make(map[string]any, value.Len())
		iter := value.MapRange()
		for iter.Next() {
			child, ok := cloneToolArgValue(iter.Value(), depth+1, budget)
			if !ok {
				return nil, false
			}
			cloned[iter.Key().String()] = child
		}
		return cloned, true
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			if !budget.chargeBytes(len("null")) {
				return nil, false
			}
			return []any(nil), true
		}
		if value.Len() > budget.remainingNodes || !chargeToolArgArrayFraming(value.Len(), budget) {
			return nil, false
		}
		cloned := make([]any, value.Len())
		for i := 0; i < value.Len(); i++ {
			child, ok := cloneToolArgValue(value.Index(i), depth+1, budget)
			if !ok {
				return nil, false
			}
			cloned[i] = child
		}
		return cloned, true
	case reflect.Bool:
		encodedLen := len("false")
		if value.Bool() {
			encodedLen = len("true")
		}
		if !budget.chargeBytes(encodedLen) {
			return nil, false
		}
		return value.Interface(), true
	case reflect.String:
		if !chargeToolArgJSONString(value.String(), budget) {
			return nil, false
		}
		return value.Interface(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return cloneToolArgNumber(value, budget)
	case reflect.Float32:
		f := value.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return cloneToolArgNumber(value, budget)
	case reflect.Float64:
		f := value.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return cloneToolArgNumber(value, budget)
	default:
		return nil, false
	}
}

func cloneToolArgNumber(value reflect.Value, budget *toolArgCloneBudget) (any, bool) {
	encoded, err := json.Marshal(value.Interface())
	if err != nil || !budget.chargeBytes(len(encoded)) {
		return nil, false
	}
	return value.Interface(), true
}

func chargeToolArgObjectFraming(entries int, budget *toolArgCloneBudget) bool {
	framingBytes := 2 // opening and closing braces
	if entries > 0 {
		framingBytes += 2*entries - 1 // one colon per key and commas between entries
	}
	return budget.chargeBytes(framingBytes)
}

func chargeToolArgArrayFraming(elements int, budget *toolArgCloneBudget) bool {
	framingBytes := 2 // opening and closing brackets
	if elements > 0 {
		framingBytes += elements - 1 // commas between elements
	}
	return budget.chargeBytes(framingBytes)
}

// chargeToolArgJSONString charges the exact encoding/json string size with
// HTML escaping enabled, without allocating a quoted copy of the string.
func chargeToolArgJSONString(value string, budget *toolArgCloneBudget) bool {
	if budget.remainingBytes < 2 || len(value) > budget.remainingBytes-2 {
		return false
	}

	encodedBytes := 2 // surrounding quotes
	for i := 0; i < len(value); {
		current := value[i]
		if current < utf8.RuneSelf {
			switch {
			case current == '"' || current == '\\':
				encodedBytes += 2
			case current < 0x20:
				switch current {
				case '\b', '\f', '\n', '\r', '\t':
					encodedBytes += 2
				default:
					encodedBytes += 6
				}
			case current == '<' || current == '>' || current == '&':
				encodedBytes += 6
			default:
				encodedBytes++
			}
			i++
		} else {
			runeValue, size := utf8.DecodeRuneInString(value[i:])
			if runeValue == utf8.RuneError && size == 1 {
				encodedBytes += 6 // encoding/json emits invalid UTF-8 as \\ufffd
				i++
			} else if runeValue == '\u2028' || runeValue == '\u2029' {
				encodedBytes += 6
				i += size
			} else {
				encodedBytes += size
				i += size
			}
		}
		if encodedBytes > budget.remainingBytes {
			return false
		}
	}
	return budget.chargeBytes(encodedBytes)
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
		RunID:       ev.RunID,
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
