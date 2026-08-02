package acp

import (
	"context"
	"log/slog"
	"time"

	"github.com/pengjunchen/go-cli/internal/extension"
)

// ACPMiddleware bridges ACP messages into the extension middleware model. When
// an AgentInput carries an ACPMessage in its Data field, WrapAgent converts the
// inbound ACP message into an agent event by invoking the wrapped AgentFunc
// with the message content and then relaying the produced response back to the
// peer. Messages that carry no ACP data pass through the AgentFunc unchanged
// while still being recorded as ACP handling.
type ACPMiddleware struct {
	name   string
	client ACPClient
}

var _ extension.Middleware = (*ACPMiddleware)(nil)

// NewACPMiddleware returns an ACPMiddleware that shuttles ACP messages to and
// from the given client. client may be nil to make the middleware a pure
// pass-through that only records ACP handling.
func NewACPMiddleware(name string, client ACPClient) *ACPMiddleware {
	return &ACPMiddleware{name: name, client: client}
}

// Name returns the middleware identity.
func (m *ACPMiddleware) Name() string { return m.name }

// WrapAgent returns an AgentFunc that converts an ACP message carried in
// AgentInput.Data into an agent event (invoking next), then relays the
// response back to the peer via ACPClient.SendMessage.
func (m *ACPMiddleware) WrapAgent(next extension.AgentFunc) extension.AgentFunc {
	return func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		msg, ok := input.Data.(ACPMessage)
		if !ok || msg.Type != TypeMessage {
			slog.DebugContext(ctx, "acp.middleware.passthrough",
				"name", m.name,
				"message", input.Message,
			)
			return next(ctx, input)
		}

		converted := extension.AgentInput{
			Message: msg.Content,
			Data:    msg.Metadata,
		}
		out, err := next(ctx, converted)
		if err != nil {
			return out, err
		}

		if m.client != nil && m.client.ReceiveMessages() != nil {
			reply := ACPMessage{
				Type:       TypeResponse,
				SenderID:   msg.ReceiverID,
				ReceiverID: msg.SenderID,
				Content:    out.Text,
				Timestamp:  time.Now(),
			}
			if sendErr := m.client.SendMessage(ctx, reply); sendErr != nil {
				return out, sendErr
			}
		}

		slog.DebugContext(ctx, "acp.middleware.converted",
			"name", m.name,
			"message_type", msg.Type,
			"sender_id", msg.SenderID,
			"receiver_id", msg.ReceiverID,
		)
		return out, nil
	}
}
