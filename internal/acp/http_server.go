package acp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// maxRequestBodySize caps the size of a request body accepted by the ACP
// HTTP server. This prevents a malicious client from exhausting server
// memory by sending an arbitrarily large request body (DoS mitigation).
const maxRequestBodySize = 1 * 1024 * 1024 // 1 MB

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
	authToken   string
	authSubject string
	tlsCertFile string
	tlsKeyFile  string

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

// SetEventBus is retained for source compatibility. /events uses a
// session-local atomic replay/live subscription; a process-wide bus cannot
// preserve session isolation or replay cutover semantics.
//
// Deprecated: wire event forwarding on CoreHandler if another observer needs
// the process-wide event bus.
func (s *HTTPServer) SetEventBus(_ core.EventBus) {}

// SetAuth enables bearer-token authentication. When set, all routes require
// an Authorization: Bearer <token> header and /stream, /events bind sender_id
// to the authenticated subject instead of an arbitrary query parameter.
func (s *HTTPServer) SetAuth(token, subject string) {
	s.authToken = token
	s.authSubject = subject
}

// SetTLS configures the server to serve HTTPS using the provided certificate
// and key file paths. Both must be non-empty for TLS to be enabled. This must
// be called before Start.
func (s *HTTPServer) SetTLS(certFile, keyFile string) {
	s.tlsCertFile = certFile
	s.tlsKeyFile = keyFile
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

// bodyLimitMiddleware wraps r.Body with an http.MaxBytesReader so that any
// handler reading the body is capped at maxRequestBodySize. Handlers detect
// the resulting *http.MaxBytesError from json.Decode and return 413.
func (s *HTTPServer) bodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		next.ServeHTTP(w, r)
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
	handler = s.bodyLimitMiddleware(handler)
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

	slog.Info("acp.server.start", "name", s.name, "addr", ln.Addr().String(), "tls", s.tlsCertFile != "")
	go func() {
		if s.tlsCertFile != "" && s.tlsKeyFile != "" {
			if err := srv.ServeTLS(ln, s.tlsCertFile, s.tlsKeyFile); err != nil && err != http.ErrServerClosed {
				slog.Error("acp.server.serve_tls_failed", "err", err)
			}
		} else {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				slog.Error("acp.server.serve_failed", "err", err)
			}
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
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
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
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
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
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
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
// connection that streams the session's events. Replay and bounded live
// delivery are registered atomically by Session.SubscribeAfter. A keep-alive
// comment is sent every 15 seconds to prevent intermediary proxies from
// closing idle connections.
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

	// Parse Last-Event-ID before registering so malformed cursors cannot
	// silently restart replay from the beginning of the retained ring.
	var afterID uint64
	if idStr := r.Header.Get("Last-Event-ID"); idStr != "" {
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid Last-Event-ID", http.StatusBadRequest)
			return
		}
		afterID = id
	}

	// Snapshot and register live delivery under the same Session lock used by
	// PublishEvent. An event racing this call is present in Replay or Events.
	subscription, err := sess.SubscribeAfter(afterID)
	if err != nil {
		if errors.Is(err, ErrEventCursorAhead) {
			http.Error(w, "Last-Event-ID is ahead of the current session event", http.StatusBadRequest)
			return
		}
		http.Error(w, "failed to subscribe to session events", http.StatusInternalServerError)
		return
	}
	defer subscription.Close()

	sse, ok := NewSSEWriter(w)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	if gap := subscription.HistoryGap; gap != nil {
		if err := sse.WriteEventWithID(gap.Cursor, "event_gap", gap); err != nil {
			return
		}
	}
	for _, e := range subscription.Replay {
		if err := sse.WriteEventWithID(e.ID, e.Event.Kind, e.Event); err != nil {
			return
		}
	}

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-subscription.Events():
			if !ok {
				_ = finishClosedEventSubscription(sse, subscription)
				return
			}
			if err := sse.WriteEventWithID(entry.ID, entry.Event.Kind, entry.Event); err != nil {
				return
			}
		case <-subscription.Done():
			_ = finishClosedEventSubscription(sse, subscription)
			return
		case <-ticker.C:
			sse.WriteComment("keep-alive")
		}
	}
}

// finishClosedEventSubscription detaches the subscriber, drains the complete
// buffered prefix, and reports its frozen gap when one exists. Both closed
// channel cases use this path because Session.Close closes Events and Done
// together, making select case choice nondeterministic.
func finishClosedEventSubscription(sse *SSEWriter, subscription *EventSubscription) error {
	gap := subscription.Close()
	for entry := range subscription.Events() {
		if err := sse.WriteEventWithID(entry.ID, entry.Event.Kind, entry.Event); err != nil {
			return err
		}
	}
	if gap != nil {
		return sse.WriteEventWithID(gap.Cursor, "event_gap", gap)
	}
	return nil
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

// IsLoopbackAddr reports whether addr binds to a loopback interface. An empty
// host (e.g. ":9090") binds to all interfaces and is not loopback.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if host == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	return net.ParseIP(host).IsLoopback()
}
