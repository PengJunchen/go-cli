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

// eventSubscriberBufferSize bounds each live session subscription. A slow
// subscriber is detached with an explicit gap instead of blocking publishers.
const eventSubscriberBufferSize = 64

// EventEntry pairs an AgentEvent with its monotonically increasing sequence
// ID for Last-Event-ID reconnection support.
type EventEntry struct {
	ID    uint64
	Event core.AgentEvent
}

// EventGap reports events that an ACP session event stream could not provide
// contiguously. It describes transport projection loss, not a run-level
// EventGapCount or durable transcript gap.
type EventGap struct {
	Layer            string `json:"layer"`
	Reason           string `json:"reason"`
	Cursor           uint64 `json:"cursor"`
	MissingFrom      uint64 `json:"missing_from"`
	MissingThrough   uint64 `json:"missing_through"`
	MissingCount     uint64 `json:"missing_count"`
	FirstAvailableID uint64 `json:"first_available_id,omitempty"`
	LastEventID      uint64 `json:"last_event_id"`
}

// EventSubscription is a task/session-local replay snapshot plus a bounded
// live stream registered atomically with that snapshot. Call Close when the
// consumer is done; no per-subscription goroutine is created.
type EventSubscription struct {
	Replay     []EventEntry
	LastSeq    uint64
	HistoryGap *EventGap

	session      *Session
	subscriberID uint64
	subscriber   *eventSubscriber
}

// Events returns entries published after the subscription snapshot.
func (sub *EventSubscription) Events() <-chan EventEntry { return sub.subscriber.events }

// Done is closed when the session closes or the bounded live stream overflows.
func (sub *EventSubscription) Done() <-chan struct{} { return sub.subscriber.done }

// Close unregisters the subscriber and returns any live-overflow gap. Buffered
// entries remain readable from Events after Close, allowing consumers to drain
// the contiguous prefix before reporting the gap.
func (sub *EventSubscription) Close() *EventGap {
	if sub == nil || sub.session == nil || sub.subscriber == nil {
		return nil
	}

	s := sub.session
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.subscribers[sub.subscriberID] == sub.subscriber {
		delete(s.subscribers, sub.subscriberID)
	}
	s.closeSubscriberLocked(sub.subscriber)
	return cloneEventGap(sub.subscriber.gap)
}

type eventSubscriber struct {
	events       chan EventEntry
	done         chan struct{}
	gap          *EventGap
	doneSignaled bool
	eventsClosed bool
	closed       bool
}

// Session represents a connected ACP client's state. Each session tracks
// pending messages queued for delivery via /stream and a bounded event
// projection for /events. Use SubscribeAfter for replay/live streaming; Events
// remains as a legacy best-effort channel for existing in-process consumers.
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
	nextSubID    uint64
	subscribers  map[uint64]*eventSubscriber
}

// NewSession creates a Session with the given id (typically the client's
// SenderID).
func NewSession(id string) *Session {
	return &Session{
		id:          id,
		events:      make(chan core.AgentEvent, 128),
		subscribers: make(map[uint64]*eventSubscriber),
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
// Legacy and replay/live event consumers are unblocked.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.events)
	for id, subscriber := range s.subscribers {
		s.closeSubscriberLocked(subscriber)
		delete(s.subscribers, id)
	}
}

// closeSubscriberLocked freezes any overflow gap and closes the subscriber's
// channels. Caller must hold s.mu.
func (s *Session) closeSubscriberLocked(subscriber *eventSubscriber) {
	if subscriber.closed {
		return
	}
	if subscriber.gap != nil {
		subscriber.gap.MissingThrough = s.eventSeq
		if subscriber.gap.MissingThrough >= subscriber.gap.MissingFrom {
			subscriber.gap.MissingCount = subscriber.gap.MissingThrough - subscriber.gap.MissingFrom + 1
		}
		subscriber.gap.LastEventID = s.eventSeq
	}
	if !subscriber.eventsClosed {
		close(subscriber.events)
		subscriber.eventsClosed = true
	}
	if !subscriber.doneSignaled {
		close(subscriber.done)
		subscriber.doneSignaled = true
	}
	subscriber.closed = true
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

// PublishEvent records one monotonically-IDed entry and performs non-blocking
// delivery to legacy and replay/live consumers. A full legacy channel drops
// only that legacy delivery; a full replay/live subscriber is terminated with
// explicit gap evidence while the ring retains the event for reconnection.
func (s *Session) PublishEvent(event core.AgentEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.eventSeq++
	entry := EventEntry{ID: s.eventSeq, Event: event}
	s.appendHistory(entry)
	select {
	case s.events <- event:
	default:
		// Channel full, drop event from live stream. It remains in the
		// ring buffer for Last-Event-ID reconnection.
	}
	for _, subscriber := range s.subscribers {
		if subscriber.closed || subscriber.gap != nil {
			continue
		}
		select {
		case subscriber.events <- entry:
		default:
			subscriber.gap = &EventGap{
				Layer:          "acp_session_transport",
				Reason:         "subscriber_overflow",
				Cursor:         entry.ID - 1,
				MissingFrom:    entry.ID,
				MissingThrough: entry.ID,
				MissingCount:   1,
				LastEventID:    entry.ID,
			}
			if !subscriber.doneSignaled {
				close(subscriber.done)
				subscriber.doneSignaled = true
			}
		}
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

// EventHistorySince returns a snapshot of retained entries with ID greater
// than afterID and the current last sequence. It does not register a live
// consumer; callers that need replay-to-live continuity must use
// SubscribeAfter.
func (s *Session) EventHistorySince(afterID uint64) ([]EventEntry, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, _ := s.historySinceLocked(afterID)
	return entries, s.eventSeq
}

// SubscribeAfter atomically snapshots retained events and registers a
// bounded live subscriber under the same mutex used by PublishEvent. Each
// publication is therefore present in Replay or Events, unless the bounded
// live queue overflows, in which case Done closes and Close returns an
// explicit EventGap.
func (s *Session) SubscribeAfter(afterID uint64) *EventSubscription {
	s.mu.Lock()
	defer s.mu.Unlock()

	replay, historyGap := s.historySinceLocked(afterID)
	s.nextSubID++
	subscriber := &eventSubscriber{
		events: make(chan EventEntry, eventSubscriberBufferSize),
		done:   make(chan struct{}),
	}
	subscription := &EventSubscription{
		Replay:       replay,
		LastSeq:      s.eventSeq,
		HistoryGap:   historyGap,
		session:      s,
		subscriberID: s.nextSubID,
		subscriber:   subscriber,
	}
	if s.closed {
		s.closeSubscriberLocked(subscriber)
		return subscription
	}
	s.subscribers[s.nextSubID] = subscriber
	return subscription
}

// historySinceLocked returns a copy of the retained suffix and an explicit
// retention gap when afterID predates the oldest retained EventEntry. Caller
// must hold s.mu.
func (s *Session) historySinceLocked(afterID uint64) ([]EventEntry, *EventGap) {
	var entries []EventEntry
	for _, e := range s.eventHistory {
		if e.ID > afterID {
			entries = append(entries, e)
		}
	}
	var gap *EventGap
	if len(s.eventHistory) > 0 {
		firstAvailableID := s.eventHistory[0].ID
		if afterID < firstAvailableID-1 {
			missingFrom := afterID + 1
			missingThrough := firstAvailableID - 1
			gap = &EventGap{
				Layer:            "acp_session_transport",
				Reason:           "retention",
				Cursor:           missingThrough,
				MissingFrom:      missingFrom,
				MissingThrough:   missingThrough,
				MissingCount:     missingThrough - missingFrom + 1,
				FirstAvailableID: firstAvailableID,
				LastEventID:      s.eventSeq,
			}
		}
	}
	return entries, gap
}

func cloneEventGap(gap *EventGap) *EventGap {
	if gap == nil {
		return nil
	}
	copy := *gap
	return &copy
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
