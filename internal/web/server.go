package web

import (
	"context"
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

// WebServer is a self-contained HTTP server that provides a browser-based
// Web UI for interacting with the go-cli. It exposes a simple chat interface
// backed by JSON HTTP endpoints and an SSE streaming placeholder.
//
// The server has no dependency on the CLI assembly; chat responses are
// placeholder echoes. Real CLI integration will be wired later.
type WebServer struct {
	addr    string
	handler http.Handler

	mu     sync.Mutex
	server *http.Server
	ln     net.Listener
}

// NewWebServer creates a WebServer listening on addr.
func NewWebServer(addr string) *WebServer {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/health", handleHealth)
	mux.HandleFunc("/api/chat", handleChat)
	mux.HandleFunc("/ws", handleWS)

	return &WebServer{
		addr:    addr,
		handler: mux,
	}
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
func handleIndex(w http.ResponseWriter, r *http.Request) {
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
func handleHealth(w http.ResponseWriter, r *http.Request) {
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
func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
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
// integration will be wired later.
func handleWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
