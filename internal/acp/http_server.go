package acp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
)

// contextKey is an unexported type used for context value keys in this package.
type contextKey int

const authSubjectKey contextKey = iota

// HTTPServer implements ACPServer, serving the ACP JSON-over-HTTP protocol.
// It exposes four routes that mirror the gRPCAdapter client contract:
//
//   - POST /connect     — establish a session
//   - POST /send        — deliver an ACP message for processing
//   - POST /disconnect  — tear down a session
//   - GET  /stream      — drain pending response messages (NDJSON)
//
// Sessions are tracked by SenderID (used as an internal routing key). The
// /stream endpoint requires a ?sender_id= query parameter for per-session
// streaming; global drain (all sessions without sender_id) is not allowed
// to prevent cross-session data leakage.
//
// When auth is enabled via SetAuth, all routes require a Bearer token and
// the /stream, /events endpoints bind sender_id to the authenticated subject
// instead of an arbitrary query parameter. Session IDs are always generated
// server-side using crypto/rand to prevent session fixation attacks.
type HTTPServer struct {
	name        string
	addr        string
	handler     *CoreHandler
	eventBus    core.EventBus
	authToken   string
	authSubject string

	mu       sync.Mutex
	sessions map[string]*Session
	server   *http.Server
	ln       net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	running  bool

	// maxConcurrency limits the number of concurrent in-flight requests.
	// Default: 64. Set before calling Start.
	maxConcurrency int
	sem            chan struct{}

	// sessionTTL is the maximum idle duration before a session is cleaned up.
	// Default: 30 minutes.
	sessionTTL time.Duration
	// cleanupInterval is how often the cleanup goroutine checks for idle
	// sessions. Default: 5 minutes.
	cleanupInterval time.Duration
	// lastActivity tracks the last activity time for each session (by senderID).
	lastActivity map[string]time.Time
}

var _ ACPServer = (*HTTPServer)(nil)

// NewHTTPServer creates an HTTPServer listening on addr. The CoreHandler
// processes inbound messages; it may be nil to run in echo-only mode (no
// agent dispatch, useful for connectivity testing).
func NewHTTPServer(name, addr string, handler *CoreHandler) *HTTPServer {
	return &HTTPServer{
		name:            name,
		addr:            addr,
		handler:         handler,
		sessions:        make(map[string]*Session),
		maxConcurrency:  64,
		sessionTTL:      30 * time.Minute,
		cleanupInterval: 5 * time.Minute,
		lastActivity:    make(map[string]time.Time),
	}
}

// SetEventBus wires an EventBus for SSE fan-out. When set, the /events
// endpoint subscribes to the bus for live event streaming (each SSE
// connection gets its own independent channel). When nil (or not called),
// the /events endpoint falls back to the per-session events channel.
func (s *HTTPServer) SetEventBus(bus core.EventBus) {
	s.eventBus = bus
}

// SetAuth enables bearer-token authentication. When set, all routes require
// an Authorization: Bearer <token> header and /stream, /events bind sender_id
// to the authenticated subject instead of an arbitrary query parameter.
func (s *HTTPServer) SetAuth(token, subject string) {
	s.authToken = token
	s.authSubject = subject
}

// authMiddleware wraps next with bearer-token authentication. Requests
// without a valid Bearer token receive 401 with a WWW-Authenticate header.
func (s *HTTPServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), authSubjectKey, s.authSubject)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authSubjectFromContext returns the authenticated subject from the request
// context, or "" if auth is not enabled.
func authSubjectFromContext(ctx context.Context) string {
	v, _ := ctx.Value(authSubjectKey).(string)
	return v
}

// rateLimitMiddleware wraps next with a semaphore-based concurrency limiter.
// When the number of in-flight requests reaches maxConcurrency, additional
// requests receive 503 Service Unavailable.
func (s *HTTPServer) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "too many concurrent requests", http.StatusServiceUnavailable)
			return
		}
	})
}

// Start brings the HTTP server up and begins serving. It binds the listener
// synchronously so that bind errors (e.g. port in use) are returned to the
// caller rather than silently logged.
func (s *HTTPServer) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = true
	if s.maxConcurrency <= 0 {
		s.maxConcurrency = 64
	}
	s.sem = make(chan struct{}, s.maxConcurrency)
	s.mu.Unlock()

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		return fmt.Errorf("acp: listen %s: %w", s.addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/connect", s.handleConnect)
	mux.HandleFunc("/send", s.handleSend)
	mux.HandleFunc("/disconnect", s.handleDisconnect)
	mux.HandleFunc("/stream", s.handleStream)
	mux.HandleFunc("/events", s.handleEvents)

	handler := http.Handler(mux)
	if s.authToken != "" {
		handler = s.authMiddleware(handler)
	}
	handler = s.rateLimitMiddleware(handler)

	srv := &http.Server{
		Addr:    s.addr,
		Handler: handler,
	}

	s.mu.Lock()
	s.server = srv
	s.ln = ln
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.mu.Unlock()

	slog.Info("acp.server.start", "name", s.name, "addr", ln.Addr().String())
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("acp.server.serve_failed", "err", err)
		}
	}()

	// Start session cleanup goroutine for idle session reclamation.
	go s.sessionCleanupLoop()

	return nil
}

// Stop shuts the server down and releases all sessions. It cancels the
// server-level context to abort any in-flight dispatch goroutines, then
// gracefully shuts down the HTTP server.
func (s *HTTPServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return nil
	}
	s.running = false
	server := s.server
	s.server = nil
	s.ln = nil
	cancel := s.cancel
	s.cancel = nil

	// Close all sessions.
	for id, sess := range s.sessions {
		sess.Close()
		delete(s.sessions, id)
	}
	s.lastActivity = make(map[string]time.Time)
	s.mu.Unlock()

	// Cancel server context to abort in-flight dispatch goroutines.
	if cancel != nil {
		cancel()
	}

	slog.Info("acp.server.stop", "name", s.name)
	if server != nil {
		return server.Shutdown(ctx)
	}
	return nil
}

// Name returns the server identity.
func (s *HTTPServer) Name() string { return s.name }

// Addr returns the actual listening address. If the server was started with
// ":0", this returns the OS-assigned address. Returns "" before Start or
// after Stop.
func (s *HTTPServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Running reports whether the server has been started and not yet stopped.
func (s *HTTPServer) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// generateSessionID generates a cryptographically random 32-byte session ID,
// returned as a 64-character hex-encoded string. The ID is generated
// server-side to prevent session fixation attacks — client-provided SenderIDs
// are never used as session identifiers.
func generateSessionID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("acp: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// getOrCreateSession returns the session for senderID, creating one if it
// does not exist. New sessions are assigned a server-generated random ID
// (not the client-provided senderID) to prevent session fixation.
func (s *HTTPServer) getOrCreateSession(senderID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[senderID]
	if !ok {
		sess = NewSession(generateSessionID())
		s.sessions[senderID] = sess
	}
	s.lastActivity[senderID] = time.Now()
	return sess
}

// getSession returns the session for senderID, or nil if not found.
func (s *HTTPServer) getSession(senderID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[senderID]
	if sess != nil {
		s.lastActivity[senderID] = time.Now()
	}
	return sess
}

// removeSession closes and removes the session for senderID.
func (s *HTTPServer) removeSession(senderID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[senderID]
	if !ok {
		return nil
	}
	delete(s.sessions, senderID)
	delete(s.lastActivity, senderID)
	return sess
}

// handleConnect processes POST /connect requests. It creates a session for
// the client's SenderID and returns the server-generated session ID in the
// response body. The client-provided SenderID is used as an internal routing
// key but is never exposed as the session ID, preventing session fixation.
func (s *HTTPServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var msg ACPMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	senderID := msg.SenderID
	if senderID == "" {
		senderID = "anonymous"
	}

	sess := s.getOrCreateSession(senderID)

	slog.Info("acp.server.connect", "sender", senderID, "session_id", sess.ID())

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"session_id": sess.ID(),
		"status":     "connected",
	})
}

// handleSend processes POST /send requests. It decodes the ACPMessage,
// dispatches it through the CoreHandler, and returns HTTP 200.
func (s *HTTPServer) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var msg ACPMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}

	senderID := msg.SenderID
	if senderID == "" {
		senderID = "anonymous"
	}

	sess := s.getOrCreateSession(senderID)

	// Process the message asynchronously to avoid blocking the HTTP
	// response. The response (if any) is enqueued onto the session and
	// will be delivered via /stream. We use the server-level context
	// (not r.Context()) because the request context is canceled as soon
	// as this handler returns.
	if s.handler != nil {
		s.mu.Lock()
		srvCtx := s.ctx
		s.mu.Unlock()
		if srvCtx != nil {
			go s.handler.ProcessMessage(srvCtx, msg, sess)
		}
	} else {
		// Echo mode: respond with an ack.
		sess.Enqueue(ACPMessage{
			Type:       TypeAck,
			SenderID:   s.name,
			ReceiverID: senderID,
			Content:    "message received",
			Timestamp:  time.Now(),
		})
	}

	w.WriteHeader(http.StatusOK)
}

// handleDisconnect processes POST /disconnect requests. It closes and
// removes the session for the client's SenderID.
func (s *HTTPServer) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var msg ACPMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		// Allow empty body disconnect.
		msg.SenderID = ""
	}

	senderID := msg.SenderID
	if senderID == "" {
		// Try to get from query parameter.
		senderID = r.URL.Query().Get("sender_id")
	}

	sess := s.removeSession(senderID)
	if sess != nil {
		sess.Close()
	}

	slog.Info("acp.server.disconnect", "sender", senderID)
	w.WriteHeader(http.StatusOK)
}

// handleStream processes GET /stream requests. It drains pending messages
// for the requested session (via ?sender_id= query parameter) and writes
// them as newline-delimited JSON. A sender_id query parameter is always
// required — draining all sessions (global drain) is not allowed, to
// prevent unauthorized access to other clients' messages. When auth is
// enabled, sender_id is bound to the authenticated subject.
func (s *HTTPServer) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	senderID := r.URL.Query().Get("sender_id")

	// When auth is enabled, bind sender_id to the authenticated subject.
	if subject := authSubjectFromContext(r.Context()); subject != "" {
		senderID = subject
	}

	// Require sender_id — global drain (all sessions) is not allowed
	// to prevent cross-session data leakage.
	if senderID == "" {
		http.Error(w, "sender_id query parameter is required", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	// Drain messages for a specific session.
	sess := s.getSession(senderID)
	if sess == nil {
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	s.writeMessages(w, sess.Drain())

	if flusher != nil {
		flusher.Flush()
	}
}

// writeMessages writes messages as newline-delimited JSON to w.
func (s *HTTPServer) writeMessages(w http.ResponseWriter, msgs []ACPMessage) {
	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "%s\n", data) //nolint:errcheck // best-effort stream write
	}
}

// handleEvents processes GET /events requests. It opens an SSE long-lived
// connection that streams core.AgentEvents. The connection stays open until
// the client disconnects, the session is closed, or the server shuts down.
// A keep-alive comment is sent every 15 seconds to prevent intermediary
// proxies from closing idle connections.
//
// Last-Event-ID reconnection: when the client sends a Last-Event-ID request
// header, the server replays missed events from the session's ring buffer
// (entries whose ID exceeds the header value) before streaming live events.
//
// EventBus fan-out: when an EventBus is wired (via SetEventBus), live events
// are read from a per-connection subscription channel. When no bus is
// configured, live events are read from the per-session events channel
// (backward compatible).
func (s *HTTPServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	senderID := r.URL.Query().Get("sender_id")

	// When auth is enabled, bind sender_id to the authenticated subject.
	if subject := authSubjectFromContext(r.Context()); subject != "" {
		senderID = subject
	}

	if senderID == "" {
		http.Error(w, "sender_id query parameter is required", http.StatusBadRequest)
		return
	}

	sess := s.getSession(senderID)
	if sess == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	sse, ok := NewSSEWriter(w)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// --- Last-Event-ID reconnection ---
	// Parse the Last-Event-ID header (if present) and replay missed events
	// from the session's ring buffer. EventHistorySince returns the missed
	// entries and the current last sequence ID atomically, so we can safely
	// assign IDs to subsequent live-stream reads starting from lastSeq+1.
	var afterID uint64
	if idStr := r.Header.Get("Last-Event-ID"); idStr != "" {
		if id, err := strconv.ParseUint(idStr, 10, 64); err == nil {
			afterID = id
		}
	}
	entries, lastSeq := sess.EventHistorySince(afterID)
	nextID := lastSeq + 1
	for _, e := range entries {
		if err := sse.WriteEventWithID(e.ID, e.Event.Kind, e.Event); err != nil {
			return
		}
	}

	// --- Live event source ---
	// When an EventBus is wired, subscribe for fan-out (each connection gets
	// its own independent channel). Otherwise, fall back to the per-session
	// events channel.
	var eventCh <-chan core.AgentEvent
	if s.eventBus != nil {
		eventCh = s.eventBus.Subscribe(r.Context())
	} else {
		eventCh = sess.Events()
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-eventCh:
			if !ok {
				// Channel closed (session closed or bus shut down).
				return
			}
			if err := sse.WriteEventWithID(nextID, ev.Kind, ev); err != nil {
				return
			}
			nextID++
		case <-ticker.C:
			sse.WriteComment("keep-alive")
		}
	}
}

// ActiveSessions returns the number of currently active sessions. This is
// primarily used for testing and monitoring.
func (s *HTTPServer) ActiveSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// sessionCleanupLoop periodically removes sessions that have been idle longer
// than sessionTTL. The loop exits when the server context is canceled.
func (s *HTTPServer) sessionCleanupLoop() {
	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.cleanupIdleSessions()
		}
	}
}

// cleanupIdleSessions removes sessions whose last activity exceeds sessionTTL.
func (s *HTTPServer) cleanupIdleSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for id, last := range s.lastActivity {
		if now.Sub(last) > s.sessionTTL {
			if sess, ok := s.sessions[id]; ok {
				sess.Close()
			}
			delete(s.sessions, id)
			delete(s.lastActivity, id)
		}
	}
}

// ensureServerName returns a non-empty server name, defaulting to "acp-server".
func ensureServerName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "acp-server"
	}
	return name
}
