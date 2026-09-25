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
)

//go:embed index.html
var indexHTML embed.FS

// WebServer serves the dashboard assets and health endpoint. Its chat and
// stream routes remain unavailable until they are connected to a real run.
type WebServer struct {
	addr      string
	authToken string
	handler   http.Handler

	mu     sync.Mutex
	server *http.Server
	ln     net.Listener
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
		addr: addr,
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
// use) are returned to the caller, then begins serving HTTP requests. Without
// authentication, it accepts only explicit loopback IP bind addresses.
func (s *WebServer) Start() error {
	s.mu.Lock()
	if s.server != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if s.authToken == "" && !isExplicitLoopbackAddress(s.addr) {
		return fmt.Errorf("web: bind %q requires configured authentication unless its host is an explicit loopback IP address", s.addr)
	}

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

// isExplicitLoopbackAddress accepts only literal loopback IP bind addresses.
// Hostnames are intentionally not resolved here, avoiding a DNS check/use
// mismatch between validation and net.Listen.
func isExplicitLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// handleChat rejects the placeholder chat route until a real run owner is
// connected.
func (*WebServer) handleChat(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "web chat is unavailable", http.StatusServiceUnavailable)
}

// handleWS rejects the placeholder stream route before it can begin an SSE
// response or claim that a client is connected.
func (*WebServer) handleWS(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "web stream is unavailable", http.StatusServiceUnavailable)
}
