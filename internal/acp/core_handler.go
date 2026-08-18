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

// eventHistorySize is the maximum number of events retained in the Session
// ring buffer for Last-Event-ID reconnection. When this limit is exceeded,
// the oldest entries are evicted.
const eventHistorySize = 256

// EventEntry pairs an AgentEvent with its monotonically increasing sequence
// ID for Last-Event-ID reconnection support.
type EventEntry struct {
	ID    uint64
	Event core.AgentEvent
}

// Session represents a connected ACP client's state. Each session tracks
// pending messages queued for delivery via the /stream endpoint and an events
// channel for streaming core.AgentEvents to SSE clients via /events.
type Session struct {
	id      string
	mu      sync.Mutex
	pending []ACPMessage
	closed  bool
	events  chan core.AgentEvent

	// eventSeq is a monotonically increasing counter assigned to each event
	// via PublishEvent. eventHistory is a ring buffer of the last
	// eventHistorySize entries, used for Last-Event-ID reconnection: when a
	// client reconnects with a Last-Event-ID header, the server replays
	// entries whose ID exceeds that value.
	eventSeq     uint64
	eventHistory []EventEntry
}

// NewSession creates a Session with the given id (typically the client's
// SenderID).
func NewSession(id string) *Session {
	return &Session{
		id:     id,
		events: make(chan core.AgentEvent, 128),
	}
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
// The events channel is closed so SSE consumers unblock.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.events)
}

// IsClosed reports whether the session has been closed.
func (s *Session) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Events returns the channel for streaming core.AgentEvents to SSE clients.
func (s *Session) Events() <-chan core.AgentEvent {
	return s.events
}

// PublishEvent sends an event to the events channel. Non-blocking: if the
// channel is full, the event is dropped. The event is always recorded in the
// ring buffer with a monotonically increasing ID so that Last-Event-ID
// reconnection can replay it even if the channel delivery was dropped.
func (s *Session) PublishEvent(event core.AgentEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.eventSeq++
	s.appendHistory(EventEntry{ID: s.eventSeq, Event: event})
	select {
	case s.events <- event:
	default:
		// Channel full, drop event from live stream. It remains in the
		// ring buffer for Last-Event-ID reconnection.
	}
}

// appendHistory adds an entry to the ring buffer, evicting the oldest entry
// when the buffer is full. Caller must hold s.mu.
func (s *Session) appendHistory(entry EventEntry) {
	if len(s.eventHistory) < eventHistorySize {
		s.eventHistory = append(s.eventHistory, entry)
		return
	}
	// Ring buffer: shift left and append at the end.
	copy(s.eventHistory, s.eventHistory[1:])
	s.eventHistory[eventHistorySize-1] = entry
}

// EventHistorySince returns all entries with ID greater than afterID, plus
// the current last event sequence number. Both reads are atomic with respect
// to PublishEvent (protected by the same mutex), so callers can safely use
// the returned lastSeq to assign IDs to subsequent live-stream reads without
// gaps or duplicates.
func (s *Session) EventHistorySince(afterID uint64) ([]EventEntry, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var entries []EventEntry
	for _, e := range s.eventHistory {
		if e.ID > afterID {
			entries = append(entries, e)
		}
	}
	return entries, s.eventSeq
}

// CoreHandler processes inbound ACP messages by dispatching them to a
// SubagentDispatcher (for TypeMessage) or RPCDispatcher (for TypeRPC).
// Responses are enqueued onto the sender's Session for later delivery via
// /stream. When no dispatcher is configured, TypeMessage messages receive
// an echo response (useful for testing and basic connectivity checks).
type CoreHandler struct {
	dispatcher core.SubagentDispatcher
	rpc        *RPCDispatcher
	eventBus   core.EventBus

	// bridgeOnce ensures the event forwarder is installed at most once.
	bridgeOnce sync.Once
	// sessions maps taskID -> *Session for event routing during dispatch.
	sessions sync.Map
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

// SetEventBus wires an EventBus that receives a copy of every agent event
// bridged from the dispatcher. When nil (or not called), no bus publishing
// occurs. This is the integration seam for SSE fan-out via /events.
func (h *CoreHandler) SetEventBus(bus core.EventBus) {
	h.eventBus = bus
}

// ensureBridge installs the event forwarder on the dispatcher at most once.
// The forwarder routes sub-agent events to the originating session via
// PublishEvent (SSE /events path) and, when an EventBus is wired, publishes
// a copy for fan-out. When the dispatcher is not a
// *core.DefaultSubagentDispatcher the bridge is a no-op (graceful
// degradation).
func (h *CoreHandler) ensureBridge() {
	h.bridgeOnce.Do(func() {
		d, ok := h.dispatcher.(*core.DefaultSubagentDispatcher)
		if !ok {
			return
		}
		d.SetEventForwarder(func(taskID string, ev core.AgentEvent) {
			if val, ok := h.sessions.Load(taskID); ok {
				val.(*Session).PublishEvent(ev) //nolint:errcheck // best-effort event forwarding
			}
			if h.eventBus != nil {
				h.eventBus.Publish(ev)
			}
		})
	})
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
// When the dispatcher supports event forwarding, sub-agent events are
// bridged to the session via PublishEvent (SSE /events path) and, when
// an EventBus is wired, published for fan-out.
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

	// Register the session so the event forwarder can route events by
	// taskID, then ensure the forwarder is installed. The bridge is
	// idempotent (sync.Once) and nil-safe.
	h.sessions.Store(task.ID, session)
	defer h.sessions.Delete(task.ID)
	h.ensureBridge()

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
