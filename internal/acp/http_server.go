package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HTTPServer implements ACPServer, serving the ACP JSON-over-HTTP protocol.
// It exposes four routes that mirror the gRPCAdapter client contract:
//
//   - POST /connect     — establish a session
//   - POST /send        — deliver an ACP message for processing
//   - POST /disconnect  — tear down a session
//   - GET  /stream      — drain pending response messages (NDJSON)
//
// Sessions are tracked by SenderID. The /stream endpoint accepts an optional
// ?sender_id= query parameter for per-session streaming; when omitted, all
// pending messages across sessions are drained (backward compatible with the
// existing gRPCAdapter client which does not send sender_id in GET requests).
type HTTPServer struct {
	name    string
	addr    string
	handler *CoreHandler

	mu       sync.Mutex
	sessions map[string]*Session
	server   *http.Server
	ln       net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	running  bool
}

var _ ACPServer = (*HTTPServer)(nil)

// NewHTTPServer creates an HTTPServer listening on addr. The CoreHandler
// processes inbound messages; it may be nil to run in echo-only mode (no
// agent dispatch, useful for connectivity testing).
func NewHTTPServer(name, addr string, handler *CoreHandler) *HTTPServer {
	return &HTTPServer{
		name:     name,
		addr:     addr,
		handler:  handler,
		sessions: make(map[string]*Session),
	}
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

	srv := &http.Server{
		Addr:    s.addr,
		Handler: mux,
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

// getOrCreateSession returns the session for senderID, creating one if it
// does not exist.
func (s *HTTPServer) getOrCreateSession(senderID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[senderID]
	if !ok {
		sess = NewSession(senderID)
		s.sessions[senderID] = sess
	}
	return sess
}

// getSession returns the session for senderID, or nil if not found.
func (s *HTTPServer) getSession(senderID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[senderID]
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
	return sess
}

// handleConnect processes POST /connect requests. It creates a session for
// the client's SenderID and returns the session ID in the response body.
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

	s.getOrCreateSession(senderID)

	slog.Info("acp.server.connect", "sender", senderID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"session_id": senderID,
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
// them as newline-delimited JSON. If no sender_id is provided, all pending
// messages across all sessions are drained.
func (s *HTTPServer) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	senderID := r.URL.Query().Get("sender_id")

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	if senderID != "" {
		// Drain messages for a specific session.
		sess := s.getSession(senderID)
		if sess == nil {
			return
		}
		s.writeMessages(w, sess.Drain())
	} else {
		// Drain all sessions (backward compatible with gRPCAdapter
		// which does not send sender_id in GET /stream).
		s.mu.Lock()
		sessions := make([]*Session, 0, len(s.sessions))
		for _, sess := range s.sessions {
			sessions = append(sessions, sess)
		}
		s.mu.Unlock()

		for _, sess := range sessions {
			s.writeMessages(w, sess.Drain())
		}
	}

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

// ActiveSessions returns the number of currently active sessions. This is
// primarily used for testing and monitoring.
func (s *HTTPServer) ActiveSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// ensureServerName returns a non-empty server name, defaulting to "acp-server".
func ensureServerName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "acp-server"
	}
	return name
}
