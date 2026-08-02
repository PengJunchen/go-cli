package tui

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/core"
)

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
	case "error":
		return ContentTypeError
	case "done":
		return ContentTypeStatus
	default:
		return ContentTypeStatus
	}
}

// CoreEventToAgentEvent converts a core.AgentEvent into a tui.AgentEvent
// suitable for consumption by the TUI render loop. The mapping is lossless:
// every core field is preserved and the ContentType is derived from Kind.
func CoreEventToAgentEvent(ev core.AgentEvent) AgentEvent {
	return AgentEvent{
		Type:        ev.Kind,
		Content:     ev.Content,
		ContentType: KindToContentType(ev.Kind),
	}
}

// BridgeEvents drains a core.EventStream and forwards each event as a
// tui.AgentEvent on the returned channel. The channel closes when the stream
// closes or the context is canceled. BridgeEvents is the single integration
// point between the core runtime and the TUI layer.
func BridgeEvents(ctx context.Context, stream core.EventStream) <-chan AgentEvent {
	ch := make(chan AgentEvent, 16)
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
				default:
					slog.Debug("tui.bridge.dropped", "kind", ev.Kind)
				}
			}
		}
	}()
	return ch
}
