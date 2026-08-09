package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
)

// Session represents a connected ACP client's state. Each session tracks
// pending messages queued for delivery via the /stream endpoint.
type Session struct {
	id      string
	mu      sync.Mutex
	pending []ACPMessage
	closed  bool
}

// NewSession creates a Session with the given id (typically the client's
// SenderID).
func NewSession(id string) *Session {
	return &Session{id: id}
}

// ID returns the session identifier.
func (s *Session) ID() string { return s.id }

// Enqueue appends a message to the session's pending queue. Messages
// enqueued after Close are silently dropped.
func (s *Session) Enqueue(msg ACPMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.pending = append(s.pending, msg)
}

// Drain returns and clears all pending messages. Returns nil if the
// session is closed or has no pending messages.
func (s *Session) Drain() []ACPMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.pending) == 0 {
		return nil
	}
	out := s.pending
	s.pending = nil
	return out
}

// Close marks the session as closed. Subsequent Enqueue calls are no-ops.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// IsClosed reports whether the session has been closed.
func (s *Session) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// CoreHandler processes inbound ACP messages by dispatching them to a
// SubagentDispatcher (for TypeMessage) or RPCDispatcher (for TypeRPC).
// Responses are enqueued onto the sender's Session for later delivery via
// /stream. When no dispatcher is configured, TypeMessage messages receive
// an echo response (useful for testing and basic connectivity checks).
type CoreHandler struct {
	dispatcher core.SubagentDispatcher
	rpc        *RPCDispatcher
}

// NewCoreHandler creates a CoreHandler with the given dispatchers. Either
// or both may be nil; nil dispatchers cause the corresponding message types
// to receive an error response.
func NewCoreHandler(dispatcher core.SubagentDispatcher, rpc *RPCDispatcher) *CoreHandler {
	return &CoreHandler{
		dispatcher: dispatcher,
		rpc:        rpc,
	}
}

// ProcessMessage handles an inbound ACPMessage and enqueues any response
// onto the provided session. It dispatches TypeMessage to the
// SubagentDispatcher and TypeRPC to the RPCDispatcher. Other message types
// (connect, disconnect, ack) are ignored.
func (h *CoreHandler) ProcessMessage(ctx context.Context, msg ACPMessage, session *Session) {
	switch msg.Type {
	case TypeMessage:
		h.handleMessage(ctx, msg, session)
	case TypeRPC:
		h.handleRPC(ctx, msg, session)
	default:
		// Connect, disconnect, ack, etc. are session-lifecycle messages
		// that do not require agent dispatch.
	}
}

// handleMessage dispatches a TypeMessage to the SubagentDispatcher and
// enqueues the response (TypeResponse or TypeError) onto the session.
func (h *CoreHandler) handleMessage(ctx context.Context, msg ACPMessage, session *Session) {
	if h.dispatcher == nil {
		session.Enqueue(ACPMessage{
			Type:       TypeError,
			SenderID:   msg.ReceiverID,
			ReceiverID: msg.SenderID,
			Content:    "no sub-agent dispatcher configured",
			Timestamp:  time.Now(),
		})
		return
	}

	task := core.SubagentTask{
		ID:     fmt.Sprintf("acp-%s", msg.SenderID),
		Prompt: msg.Content,
	}
	if role, ok := msg.Metadata["role"]; ok {
		task.Role = role
	}

	result, err := h.dispatcher.Dispatch(ctx, task)

	content := result.Content
	msgType := TypeResponse
	if err != nil {
		content = err.Error()
		msgType = TypeError
	} else if result.Error != nil {
		content = result.Error.Error()
		msgType = TypeError
	}

	session.Enqueue(ACPMessage{
		Type:       msgType,
		SenderID:   msg.ReceiverID,
		ReceiverID: msg.SenderID,
		Content:    content,
		Timestamp:  time.Now(),
	})
}

// handleRPC parses a JSON-RPC 2.0 request from the message content,
// dispatches it to the RPCDispatcher, and enqueues the response onto the
// session. Notifications (ID == 0) do not receive a response.
func (h *CoreHandler) handleRPC(ctx context.Context, msg ACPMessage, session *Session) {
	var rpcMsg RPCMessage
	if err := json.Unmarshal([]byte(msg.Content), &rpcMsg); err != nil {
		slog.Warn("acp.server.rpc_parse_failed", "err", err)
		return
	}

	// Notifications (ID == 0) don't get a response.
	if rpcMsg.ID == 0 {
		if h.rpc != nil {
			_, _ = h.rpc.Dispatch(ctx, rpcMsg) //nolint:errcheck
		}
		return
	}

	var resp RPCResponse
	resp.JSONRPC = "2.0"
	resp.ID = rpcMsg.ID

	if h.rpc == nil {
		resp.Error = &RPCError{Code: RPCCodeInternalError, Message: "no RPC dispatcher configured"}
	} else {
		result, err := h.rpc.Dispatch(ctx, rpcMsg)
		if err != nil {
			if rpcErr, ok := err.(*RPCError); ok {
				resp.Error = rpcErr
			} else {
				resp.Error = &RPCError{Code: RPCCodeInternalError, Message: err.Error()}
			}
		} else {
			resp.Result = result
		}
	}

	respData, err := json.Marshal(resp)
	if err != nil {
		slog.Warn("acp.server.rpc_marshal_failed", "err", err)
		return
	}

	session.Enqueue(ACPMessage{
		Type:       TypeRPC,
		SenderID:   msg.ReceiverID,
		ReceiverID: msg.SenderID,
		Content:    string(respData),
		Timestamp:  time.Now(),
	})
}
