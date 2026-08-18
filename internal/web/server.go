// Package web provides the web server for the go-cli dashboard.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML embed.FS

// maxChatBodySize limits the JSON request body for /api/chat to 1 MiB.
const maxChatBodySize = 1 << 20

// maxSSEConnections limits the number of concurrent SSE connections to
// prevent resource exhaustion.
const maxSSEConnections = 64

// WebServer is a self-contained HTTP server that provides a browser-based
// Web UI for interacting with the go-cli. It exposes a simple chat interface
// backed by JSON HTTP endpoints and an SSE streaming placeholder.
//
// The server has no dependency on the CLI assembly; chat responses are
// placeholder echoes. Real CLI integration will be wired later.
type WebServer struct {
	addr      string
	authToken string
	handler   http.Handler

	mu     sync.Mutex
	server *http.Server
	ln     net.Listener
	sseSem chan struct{} // concurrency limiter for SSE
}

// WebServerOption configures a WebServer.
type WebServerOption func(*WebServer)

// WithAuthToken sets a bearer token that clients must supply in the
// Authorization header. When set, all API endpoints require
// "Authorization: Bearer <token>". When empty (the default), no
// authentication is enforced.
func WithAuthToken(token string) WebServerOption {
	return func(s *WebServer) { s.authToken = token }
}

// NewWebServer creates a WebServer listening on addr.
func NewWebServer(addr string, opts ...WebServerOption) *WebServer {
	s := &WebServer{
		addr:   addr,
		sseSem: make(chan struct{}, maxSSEConnections),
	}
	for _, opt := range opts {
		opt(s)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/chat", s.handleChat)
	mux.HandleFunc("/ws", s.handleWS)

	// Wrap with security-headers + auth middleware.
	s.handler = s.middleware(mux)

	return s
}

// middleware wraps the given handler with security headers and optional
// bearer-token authentication.
func (s *WebServer) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Security headers.
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")

		// Authentication: skip for the index page so the UI can load,
		// but enforce on all API endpoints.
		if s.authToken != "" && r.URL.Path != "/" {
			if !s.checkAuth(r) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// checkAuth verifies the Authorization header against the configured token
// using constant-time comparison.
func (s *WebServer) checkAuth(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || auth[:len(prefix)] != prefix {
		return false
	}
	token := auth[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) == 1
}

// Start binds the listener synchronously so that bind errors (e.g. port in
// use) are returned to the caller, then begins serving HTTP requests.
func (s *WebServer) Start() error {
	s.mu.Lock()
	if s.server != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("web: listen %s: %w", s.addr, err)
	}

	srv := &http.Server{
		Addr:    s.addr,
		Handler: s.handler,
	}

	s.mu.Lock()
	s.server = srv
	s.ln = ln
	s.mu.Unlock()

	slog.Info("web.server.start", "addr", ln.Addr().String())
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("web.server.serve_failed", "err", err)
		}
	}()
	return nil
}

// Stop gracefully shuts down the HTTP server.
func (s *WebServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	server := s.server
	s.server = nil
	s.ln = nil
	s.mu.Unlock()

	if server == nil {
		return nil
	}
	slog.Info("web.server.stop")
	return server.Shutdown(ctx)
}

// Addr returns the actual listening address. If the server was started with
// ":0", this returns the OS-assigned address. Returns "" before Start or
// after Stop.
func (s *WebServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// handleIndex serves the embedded index.html at the root path.
func (s *WebServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(indexHTML, "index.html")
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// handleHealth returns a simple health check response.
func (s *WebServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
}

// chatRequest is the JSON body for POST /api/chat.
type chatRequest struct {
	Message string `json:"message"`
}

// chatResponse is the JSON body returned by POST /api/chat.
type chatResponse struct {
	Response string `json:"response"`
}

// handleChat accepts a chat message and returns a placeholder response.
// The actual CLI integration will be wired later.
func (s *WebServer) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit request body size to prevent memory exhaustion.
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodySize)

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	resp := chatResponse{
		Response: fmt.Sprintf("Echo: %s", req.Message),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

// handleWS is an SSE streaming placeholder. It echoes back a welcome event
// and keeps the connection alive with periodic comments. Real streaming
// integration will be wired later. Concurrent connections are limited by
// maxSSEConnections to prevent resource exhaustion.
func (s *WebServer) handleWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Acquire a semaphore slot; reject when at capacity.
	select {
	case s.sseSem <- struct{}{}:
		defer func() { <-s.sseSem }()
	default:
		http.Error(w, "too many concurrent connections", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Send a welcome event so the client knows the stream is live.
	fmt.Fprintf(w, "event: connected\ndata: {\"status\":\"connected\"}\n\n") //nolint:errcheck
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": keep-alive\n\n") //nolint:errcheck
			flusher.Flush()
		}
	}
}
