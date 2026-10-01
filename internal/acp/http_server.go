package acp

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

const (
	// readHeaderTimeout limits how long an ACP peer can spend sending request
	// headers before the server rejects the connection.
	readHeaderTimeout = 3 * time.Second
	// readTimeout bounds the complete request, including the body capped by
	// maxRequestBodySize. Ten seconds permits a 1 MiB request at roughly 100 KiB/s.
	readTimeout = 10 * time.Second
	// idleTimeout releases keep-alive connections that do not send another request.
	idleTimeout = 60 * time.Second
	// maxRequestHeaderBytes keeps auth and profile headers comfortably below
	// net/http's 1 MiB default while allowing ordinary proxy metadata.
	maxRequestHeaderBytes = 32 << 10
)

const (
	// ACPHTTPProfileHeader selects the private versioned HTTP profile.
	ACPHTTPProfileHeader = "X-Go-CLI-ACP-Profile"
	// ACPHTTPProfileV1 is the only HTTP ACP profile served by this adapter.
	ACPHTTPProfileV1 = "go-cli-acp-json/1"

	localLoopbackPrincipal = "local-loopback"
)

var (
	errHTTPSessionNotFound      = errors.New("acp: session not found")
	errHTTPSessionAliasMismatch = errors.New("acp: sender alias does not match session")
	errHTTPSessionIDRequired    = errors.New("acp: session_id is required")
	// ErrHTTPServerCleanupPending means a caller's wait expired before the
	// server generation's owned work was joined.
	ErrHTTPServerCleanupPending = errors.New("acp: HTTP server cleanup is pending")
	// ErrHTTPServerAdmissionClosed means a terminal owner has sealed this
	// server against any future Start call.
	ErrHTTPServerAdmissionClosed = errors.New("acp: HTTP server admission is closed")
	// ErrHTTPServerWorkerPanic reports that a detached request worker panicked.
	ErrHTTPServerWorkerPanic = errors.New("acp: HTTP server request worker panicked")
)

// httpACPEnvelope is the HTTP-only envelope for the private ACP profile. The
// shared ACPMessage wire type remains transport-neutral for the stdio adapter.
type httpACPEnvelope struct {
	Version    string            `json:"version,omitempty"`
	SessionID  string            `json:"session_id,omitempty"`
	Type       string            `json:"type,omitempty"`
	SenderID   string            `json:"sender_id,omitempty"`
	ReceiverID string            `json:"receiver_id,omitempty"`
	Content    string            `json:"content,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Timestamp  time.Time         `json:"timestamp,omitempty"`
}

func (envelope httpACPEnvelope) message() ACPMessage {
	return ACPMessage{
		Type:       envelope.Type,
		SenderID:   envelope.SenderID,
		ReceiverID: envelope.ReceiverID,
		Content:    envelope.Content,
		Metadata:   envelope.Metadata,
		Timestamp:  envelope.Timestamp,
	}
}

func httpEnvelopeFromMessage(sessionID string, msg ACPMessage) httpACPEnvelope {
	return httpACPEnvelope{
		Version:    ACPHTTPProfileV1,
		SessionID:  sessionID,
		Type:       msg.Type,
		SenderID:   msg.SenderID,
		ReceiverID: msg.ReceiverID,
		Content:    msg.Content,
		Metadata:   msg.Metadata,
		Timestamp:  msg.Timestamp,
	}
}

type httpSessionBinding struct {
	principalID string
	senderAlias string
	session     *Session
}

type httpServerGeneration struct {
	server        *http.Server
	ctx           context.Context
	cancel        context.CancelFunc
	serveDone     chan struct{}
	startDone     chan struct{}
	cleanupDone   chan struct{}
	closeDone     chan struct{}
	dispatches    sync.WaitGroup
	requests      sync.WaitGroup
	dispatchSlots chan struct{}
	closing       bool
	admitting     bool
	serveErr      error
	closeErr      error
	workerPanic   atomic.Bool
}

type httpServerCleanupPendingError struct {
	cause error
	done  <-chan struct{}
}

func (e *httpServerCleanupPendingError) Error() string {
	return ErrHTTPServerCleanupPending.Error() + ": " + e.cause.Error()
}

func (e *httpServerCleanupPendingError) Unwrap() error {
	return errors.Join(ErrHTTPServerCleanupPending, e.cause)
}

func (e *httpServerCleanupPendingError) Done() <-chan struct{} { return e.done }

// HTTPServer implements ACPServer over the private go-cli-acp-json/1 HTTP
// profile. It exposes these routes:
//
//   - POST /connect     — establish a session
//   - POST /send        — deliver an ACP message for processing
//   - POST /disconnect  — tear down a session
//   - GET  /stream      — drain pending response messages (NDJSON)
//
// Sessions are tracked by an opaque server-generated transport session ID
// and bound to the authenticated principal. sender_id is a display alias only.
// This HTTP server implements the private go-cli-acp-json/1 profile; it does
// not claim a durable R2 session or official ACP interoperability.
type HTTPServer struct {
	name                 string
	addr                 string
	handler              *CoreHandler
	authToken            string
	authSubject          string
	noAuthLoopbackPolicy bool
	tlsCertFile          string
	tlsKeyFile           string

	mu         sync.Mutex
	sessions   map[string]httpSessionBinding
	generation *httpServerGeneration
	server     *http.Server
	ln         net.Listener
	ctx        context.Context
	cancel     context.CancelFunc
	running    bool
	sealed     bool

	// maxConcurrency sizes independent HTTP request and detached-dispatch
	// admission limits. Both default to 64; dispatch slots remain occupied
	// until ProcessMessage completes, after the /send handler returns.
	// Set before calling Start.
	maxConcurrency int
	sem            chan struct{}

	// sessionTTL is the maximum idle duration before a session is cleaned up.
	// Default: 30 minutes.
	sessionTTL time.Duration
	// cleanupInterval is how often the cleanup goroutine checks for idle
	// sessions. Default: 5 minutes.
	cleanupInterval time.Duration
	// lastActivity tracks the last activity time for each opaque session ID.
	lastActivity    map[string]time.Time
	sessionIDReader io.Reader
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
		sessions:        make(map[string]httpSessionBinding),
		maxConcurrency:  64,
		sessionTTL:      30 * time.Minute,
		cleanupInterval: 5 * time.Minute,
		lastActivity:    make(map[string]time.Time),
		sessionIDReader: rand.Reader,
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
// an Authorization: Bearer <token> header and each transport session is bound
// to the configured authenticated subject.
func (s *HTTPServer) SetAuth(token, subject string) {
	s.authToken = token
	s.authSubject = subject
}

// SetNoAuthLoopbackPolicy enables Host and Origin validation for an
// unauthenticated loopback server. It must be called before Start. Native
// clients without an Origin header remain supported.
func (s *HTTPServer) SetNoAuthLoopbackPolicy() {
	s.noAuthLoopbackPolicy = true
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
		if s.authSubject == "" {
			http.Error(w, "authenticated principal is not configured", http.StatusServiceUnavailable)
			return
		}
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

func (s *HTTPServer) profileMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		profiles := r.Header.Values(ACPHTTPProfileHeader)
		if len(profiles) != 1 || profiles[0] != ACPHTTPProfileV1 {
			http.Error(w, "missing or unsupported ACP HTTP profile", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type localHTTPAuthority struct {
	host string
	port int
}

func noAuthLoopbackPolicyMiddleware(listener net.Addr, useTLS bool, next http.Handler) http.Handler {
	_, listenerPortText, err := net.SplitHostPort(listener.String())
	listenerPort, portErr := strconv.Atoi(listenerPortText)
	if err != nil || portErr != nil || listenerPort < 1 || listenerPort > 65535 {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "invalid local listener authority", http.StatusForbidden)
		})
	}
	defaultPort := 80
	originScheme := "http"
	if useTLS {
		defaultPort = 443
		originScheme = "https"
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestAuthority, ok := parseLocalHTTPAuthority(r.Host, defaultPort)
		if !ok || requestAuthority.port != listenerPort {
			http.Error(w, "invalid local request authority", http.StatusForbidden)
			return
		}

		origins := r.Header.Values("Origin")
		if len(origins) > 1 || (len(origins) == 1 && !localOriginMatches(origins[0], requestAuthority, originScheme, defaultPort)) {
			http.Error(w, "cross-origin request is not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func parseLocalHTTPAuthority(authority string, defaultPort int) (localHTTPAuthority, bool) {
	if authority == "" || strings.TrimSpace(authority) != authority || strings.ContainsAny(authority, "/?#@,") || strings.HasSuffix(authority, ":") {
		return localHTTPAuthority{}, false
	}
	parsed, err := url.Parse("//" + authority)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return localHTTPAuthority{}, false
	}
	host := parsed.Hostname()
	if !isLoopbackHost(host) {
		return localHTTPAuthority{}, false
	}
	port := defaultPort
	if portText := parsed.Port(); portText != "" {
		parsedPort, err := strconv.Atoi(portText)
		if err != nil || parsedPort < 1 || parsedPort > 65535 {
			return localHTTPAuthority{}, false
		}
		port = parsedPort
	}
	return localHTTPAuthority{host: canonicalLocalHost(host), port: port}, true
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func canonicalLocalHost(host string) string {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func localOriginMatches(origin string, requestAuthority localHTTPAuthority, expectedScheme string, defaultPort int) bool {
	if origin == "" || strings.TrimSpace(origin) != origin || strings.ContainsAny(origin, "?#,") {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return false
	}
	if !strings.EqualFold(parsed.Scheme, expectedScheme) {
		return false
	}
	originAuthority, ok := parseLocalHTTPAuthority(parsed.Host, defaultPort)
	return ok && originAuthority == requestAuthority
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
	if s.sealed {
		s.mu.Unlock()
		return ErrHTTPServerAdmissionClosed
	}
	if s.running {
		s.mu.Unlock()
		return nil
	}
	if previous := s.generation; previous != nil && !channelClosed(previous.closeDone) {
		s.mu.Unlock()
		return &httpServerCleanupPendingError{cause: ErrHTTPServerCleanupPending, done: previous.closeDone}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("acp: listen %s: %w", s.addr, err)
	}
	noAuthLoopbackPolicy := s.noAuthLoopbackPolicy && s.authToken == ""
	if noAuthLoopbackPolicy && !IsLoopbackAddr(ln.Addr().String()) {
		boundAddr := ln.Addr().String()
		_ = ln.Close()
		s.mu.Unlock()
		return fmt.Errorf("acp: unauthenticated server policy requires a loopback listener, got %q", boundAddr)
	}
	serveTLS := s.tlsCertFile != "" && s.tlsKeyFile != ""

	if s.maxConcurrency <= 0 {
		s.maxConcurrency = 64
	}
	s.sem = make(chan struct{}, s.maxConcurrency)

	mux := http.NewServeMux()
	mux.HandleFunc("/connect", s.handleConnect)
	mux.HandleFunc("/send", s.handleSend)
	mux.HandleFunc("/disconnect", s.handleDisconnect)
	mux.HandleFunc("/stream", s.handleStream)
	mux.HandleFunc("/events", s.handleEvents)

	handler := http.Handler(mux)
	handler = s.bodyLimitMiddleware(handler)
	handler = s.profileMiddleware(handler)
	if s.authToken != "" {
		handler = s.authMiddleware(handler)
	}
	if noAuthLoopbackPolicy {
		handler = noAuthLoopbackPolicyMiddleware(ln.Addr(), serveTLS, handler)
	}
	handler = s.rateLimitMiddleware(handler)

	genCtx, cancel := context.WithCancel(ctx)
	generation := &httpServerGeneration{
		ctx:           genCtx,
		cancel:        cancel,
		serveDone:     make(chan struct{}),
		startDone:     make(chan struct{}),
		cleanupDone:   make(chan struct{}),
		closeDone:     make(chan struct{}),
		dispatchSlots: make(chan struct{}, s.maxConcurrency),
		admitting:     true,
	}
	handler = s.generationRequestMiddleware(generation, handler)

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxRequestHeaderBytes,
		// WriteTimeout remains unset because it bounds the whole response, and
		// /events intentionally keeps authenticated SSE responses open.
	}
	generation.server = srv

	s.generation = generation
	s.running = true
	s.server = srv
	s.ln = ln
	s.ctx = genCtx
	s.cancel = cancel
	tlsCertFile := s.tlsCertFile
	tlsKeyFile := s.tlsKeyFile
	listenAddr := ln.Addr().String()
	name := s.name
	s.mu.Unlock()

	go func() {
		defer close(generation.serveDone)
		if serveTLS {
			if err := srv.ServeTLS(ln, tlsCertFile, tlsKeyFile); err != nil && err != http.ErrServerClosed {
				generation.serveErr = err
				slog.Error("acp.server.serve_tls_failed", "err", err)
			}
		} else {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				generation.serveErr = err
				slog.Error("acp.server.serve_failed", "err", err)
			}
		}
	}()

	// Start session cleanup goroutine for idle session reclamation.
	go s.sessionCleanupLoop(generation)
	defer close(generation.startDone)
	slog.Info("acp.server.start", "name", name, "addr", listenAddr, "tls", serveTLS)

	return nil
}

// BeginStop closes request admission and starts the server-owned close
// operation without waiting for its handlers or dispatch workers. It is safe
// to call before Stop; repeated calls share the same generation owner.
func (s *HTTPServer) BeginStop() {
	s.beginStop(false)
}

// SealAdmission permanently prevents future Start calls and closes admission
// for the current generation. Assembly owners use this when the server is a
// terminally owned resource rather than a reusable standalone server.
func (s *HTTPServer) SealAdmission() {
	s.beginStop(true)
}

func (s *HTTPServer) beginStop(seal bool) *httpServerGeneration {
	s.mu.Lock()
	if seal {
		s.sealed = true
	}
	generation := s.generation
	if generation == nil {
		s.mu.Unlock()
		return nil
	}
	startOwner := !generation.closing
	if startOwner {
		generation.closing = true
		generation.admitting = false
		s.running = false
		s.server = nil
		s.ln = nil
		s.cancel = nil
		// Close all sessions before HTTP shutdown so stream handlers can leave.
		for id, binding := range s.sessions {
			binding.session.Close()
			delete(s.sessions, id)
		}
		s.lastActivity = make(map[string]time.Time)
	}
	s.mu.Unlock()

	if startOwner {
		// Launch before any potentially blocking cancellation or logging
		// callback so the caller remains bounded even with a custom slog handler.
		go s.closeGeneration(generation)
	}
	return generation
}

// Stop shuts the server down and releases all sessions. It cancels the
// server-level context to abort any in-flight dispatch goroutines, then
// gracefully shuts down the HTTP server.
func (s *HTTPServer) Stop(ctx context.Context) error {
	generation := s.beginStop(false)
	if generation == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-generation.closeDone:
		return generation.closeErr
	case <-ctx.Done():
		select {
		case <-generation.closeDone:
			return generation.closeErr
		default:
			return &httpServerCleanupPendingError{cause: ctx.Err(), done: generation.closeDone}
		}
	}
}

func (s *HTTPServer) closeGeneration(generation *httpServerGeneration) {
	// Cancel server context to ask handlers and detached dispatch workers to
	// stop. The close owner still joins each worker before publishing Done.
	generation.cancel()
	slog.Info("acp.server.stop", "name", s.name)
	if err := generation.server.Shutdown(context.Background()); err != nil {
		generation.closeErr = errors.Join(generation.closeErr, err)
	}
	<-generation.serveDone
	<-generation.cleanupDone
	generation.requests.Wait()
	generation.dispatches.Wait()
	<-generation.startDone
	generation.closeErr = errors.Join(generation.closeErr, generation.serveErr)
	if generation.workerPanic.Load() {
		generation.closeErr = errors.Join(generation.closeErr, ErrHTTPServerWorkerPanic)
	}
	close(generation.closeDone)
}

func (s *HTTPServer) generationRequestMiddleware(generation *httpServerGeneration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		if s.generation != generation || !generation.admitting {
			s.mu.Unlock()
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		generation.requests.Add(1)
		s.mu.Unlock()
		defer generation.requests.Done()
		next.ServeHTTP(w, r)
	})
}

func channelClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
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

// requestPrincipal returns the authenticated principal, or the fixed
// loopback principal used when bearer authentication is disabled. The latter
// is safe only because serve setup restricts unauthenticated HTTP to loopback.
func requestPrincipal(r *http.Request) string {
	if subject := authSubjectFromContext(r.Context()); subject != "" {
		return subject
	}
	return localLoopbackPrincipal
}

func (s *HTTPServer) createSession(principalID, senderAlias string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.generation == nil || !s.generation.admitting {
		return nil, ErrHTTPServerCleanupPending
	}

	reader := s.sessionIDReader
	if reader == nil {
		reader = rand.Reader
	}
	for range 4 {
		var idBytes [32]byte
		if _, err := io.ReadFull(reader, idBytes[:]); err != nil {
			return nil, errors.New("acp: failed to generate session identifier")
		}
		sessionID := hex.EncodeToString(idBytes[:])
		if _, exists := s.sessions[sessionID]; exists {
			continue
		}
		session := NewSession(sessionID)
		s.sessions[sessionID] = httpSessionBinding{
			principalID: principalID,
			senderAlias: senderAlias,
			session:     session,
		}
		s.lastActivity[sessionID] = time.Now()
		return session, nil
	}
	return nil, errors.New("acp: failed to allocate unique session identifier")
}

func (s *HTTPServer) getSessionBinding(sessionID, principalID, senderAlias string) (httpSessionBinding, error) {
	if sessionID == "" {
		return httpSessionBinding{}, errHTTPSessionIDRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.sessions[sessionID]
	if !ok || binding.principalID != principalID {
		return httpSessionBinding{}, errHTTPSessionNotFound
	}
	if senderAlias != "" && senderAlias != binding.senderAlias {
		return httpSessionBinding{}, errHTTPSessionAliasMismatch
	}
	s.lastActivity[sessionID] = time.Now()
	return binding, nil
}

func (s *HTTPServer) removeSession(sessionID, principalID, senderAlias string) (*Session, error) {
	if sessionID == "" {
		return nil, errHTTPSessionIDRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.sessions[sessionID]
	if !ok || binding.principalID != principalID {
		return nil, errHTTPSessionNotFound
	}
	if senderAlias != "" && senderAlias != binding.senderAlias {
		return nil, errHTTPSessionAliasMismatch
	}
	delete(s.sessions, sessionID)
	delete(s.lastActivity, sessionID)
	return binding.session, nil
}

func decodeHTTPEnvelope(w http.ResponseWriter, r *http.Request, allowEmpty bool) (httpACPEnvelope, bool) {
	var envelope httpACPEnvelope
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&envelope); err != nil {
		if errors.Is(err, io.EOF) && allowEmpty {
			return envelope, true
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return envelope, false
		}
		http.Error(w, "invalid JSON request body", http.StatusBadRequest)
		return envelope, false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return envelope, false
		}
		http.Error(w, "invalid JSON request body", http.StatusBadRequest)
		return envelope, false
	}
	return envelope, true
}

func writeSessionLookupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errHTTPSessionIDRequired):
		http.Error(w, "session_id is required", http.StatusBadRequest)
	case errors.Is(err, errHTTPSessionAliasMismatch):
		http.Error(w, "sender_id does not match session alias", http.StatusBadRequest)
	case errors.Is(err, errHTTPSessionNotFound):
		http.Error(w, "session not found", http.StatusNotFound)
	default:
		http.Error(w, "session unavailable", http.StatusInternalServerError)
	}
}

func validateOptionalEnvelopeVersion(w http.ResponseWriter, envelope httpACPEnvelope) bool {
	if envelope.Version != "" && envelope.Version != ACPHTTPProfileV1 {
		http.Error(w, "unsupported ACP HTTP envelope version", http.StatusBadRequest)
		return false
	}
	return true
}

// handleConnect creates an ephemeral session bound to the request principal.
// sender_id is stored only as a display alias; the server generates the
// opaque session_id used by all subsequent requests.
func (s *HTTPServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	envelope, ok := decodeHTTPEnvelope(w, r, false)
	if !ok {
		return
	}
	if envelope.Version != ACPHTTPProfileV1 {
		http.Error(w, "missing or unsupported ACP HTTP envelope version", http.StatusBadRequest)
		return
	}
	senderAlias := envelope.SenderID
	if senderAlias == "" {
		senderAlias = "anonymous"
	}
	sess, err := s.createSession(requestPrincipal(r), senderAlias)
	if err != nil {
		if errors.Is(err, ErrHTTPServerCleanupPending) {
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		slog.Error("acp.server.connect_failed", "name", s.name, "reason", "session_id_unavailable")
		http.Error(w, "unable to create session", http.StatusInternalServerError)
		return
	}
	slog.Info("acp.server.connect", "name", s.name, "status", "connected")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"version":    ACPHTTPProfileV1,
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

	envelope, ok := decodeHTTPEnvelope(w, r, false)
	if !ok {
		return
	}
	if !validateOptionalEnvelopeVersion(w, envelope) {
		return
	}
	msg := envelope.message()
	binding, err := s.getSessionBinding(envelope.SessionID, requestPrincipal(r), msg.SenderID)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}
	sess := binding.session
	msg.SenderID = binding.senderAlias

	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}

	// Process the message asynchronously to avoid blocking the HTTP
	// response. The response (if any) is enqueued onto the session and
	// will be delivered via /stream. We use the server-level context
	// (not r.Context()) because the request context is canceled as soon
	// as this handler returns.
	if s.handler != nil {
		s.mu.Lock()
		generation := s.generation
		if generation == nil || !generation.admitting {
			s.mu.Unlock()
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		srvCtx := generation.ctx
		select {
		case generation.dispatchSlots <- struct{}{}:
		default:
			s.mu.Unlock()
			http.Error(w, "too many concurrent dispatches", http.StatusServiceUnavailable)
			return
		}
		generation.dispatches.Add(1)
		s.mu.Unlock()
		if srvCtx != nil {
			dispatchCtx := context.WithValue(srvCtx, dispatchIdentityContextKey{}, dispatchIdentity{
				principalID: binding.principalID,
				sessionID:   sess.ID(),
				session:     sess,
			})
			go func() {
				defer func() {
					if recover() != nil {
						generation.workerPanic.Store(true)
						slog.Error("acp.server.process_message_panicked", "name", s.name, "reason", "panic")
					}
					<-generation.dispatchSlots
					generation.dispatches.Done()
				}()
				s.handler.ProcessMessage(dispatchCtx, msg, sess)
			}()
		} else {
			<-generation.dispatchSlots
			generation.dispatches.Done()
		}
	} else {
		// Echo mode: respond with an ack.
		sess.Enqueue(ACPMessage{
			Type:       TypeAck,
			SenderID:   s.name,
			ReceiverID: msg.SenderID,
			Content:    "message received",
			Timestamp:  time.Now(),
		})
	}

	w.WriteHeader(http.StatusOK)
}

// handleDisconnect removes only the session bound to this request principal.
func (s *HTTPServer) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	envelope, ok := decodeHTTPEnvelope(w, r, true)
	if !ok {
		return
	}
	if !validateOptionalEnvelopeVersion(w, envelope) {
		return
	}
	if envelope.SessionID == "" {
		envelope.SessionID = r.URL.Query().Get("session_id")
	}
	sess, err := s.removeSession(envelope.SessionID, requestPrincipal(r), envelope.SenderID)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}
	sess.Close()

	slog.Info("acp.server.disconnect", "name", s.name, "status", "disconnected")
	w.WriteHeader(http.StatusOK)
}

// handleStream drains only a session whose opaque ID is bound to the current
// principal. sender_id is an optional display-alias consistency check.
func (s *HTTPServer) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	senderAlias := r.URL.Query().Get("sender_id")
	binding, err := s.getSessionBinding(sessionID, requestPrincipal(r), senderAlias)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}
	sess := binding.session

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)

	s.writeMessages(w, sessionID, sess.Drain())

	if flusher != nil {
		flusher.Flush()
	}
}

// writeMessages writes messages as newline-delimited JSON to w.
func (s *HTTPServer) writeMessages(w http.ResponseWriter, sessionID string, msgs []ACPMessage) {
	for _, msg := range msgs {
		data, err := json.Marshal(httpEnvelopeFromMessage(sessionID, msg))
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

	sessionID := r.URL.Query().Get("session_id")
	senderAlias := r.URL.Query().Get("sender_id")
	binding, err := s.getSessionBinding(sessionID, requestPrincipal(r), senderAlias)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}
	sess := binding.session

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
func (s *HTTPServer) sessionCleanupLoop(generation *httpServerGeneration) {
	defer close(generation.cleanupDone)
	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-generation.ctx.Done():
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
			if binding, ok := s.sessions[id]; ok {
				binding.session.Close()
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
