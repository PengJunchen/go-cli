package acp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

const testHTTPACPProfile = "go-cli-acp-json/1"

type testHTTPACPEnvelope struct {
	Version    string            `json:"version,omitempty"`
	SessionID  string            `json:"session_id,omitempty"`
	Type       string            `json:"type,omitempty"`
	SenderID   string            `json:"sender_id,omitempty"`
	ReceiverID string            `json:"receiver_id,omitempty"`
	Content    string            `json:"content,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Timestamp  time.Time         `json:"timestamp,omitempty"`
}

func runtimeTestToken(t *testing.T) string {
	t.Helper()
	var raw [32]byte
	_, err := rand.Read(raw[:])
	require.NoError(t, err)
	return hex.EncodeToString(raw[:])
}

func profileHTTPRequest(t *testing.T, method, target, token string, body any, withProfile bool) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, target, reader)
	require.NoError(t, err)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if withProfile {
		request.Header.Set("X-Go-CLI-ACP-Profile", testHTTPACPProfile)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

func doProfileRequest(t *testing.T, client *http.Client, request *http.Request) *http.Response {
	t.Helper()
	response, err := client.Do(request)
	require.NoError(t, err)
	return response
}

func decodeConnectedSessionID(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var result struct {
		Version   string `json:"version"`
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
	require.Equal(t, testHTTPACPProfile, result.Version)
	require.NotEmpty(t, result.SessionID)
	require.Equal(t, "connected", result.Status)
	return result.SessionID
}

func TestHTTPServer_RequiresProfileOnEveryRoute(t *testing.T) {
	srv, baseURL := startTestHTTPServer(t, nil)
	client := &http.Client{Timeout: 2 * time.Second}
	paths := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/connect", testHTTPACPEnvelope{Type: TypeConnect}},
		{http.MethodPost, "/send", testHTTPACPEnvelope{Type: TypeMessage}},
		{http.MethodPost, "/disconnect", testHTTPACPEnvelope{Type: TypeDisconnect}},
		{http.MethodGet, "/stream?session_id=missing", nil},
		{http.MethodGet, "/events?session_id=missing", nil},
	}
	for _, item := range paths {
		t.Run(item.method+item.path, func(t *testing.T) {
			request := profileHTTPRequest(t, item.method, baseURL+item.path, "", item.body, false)
			response := doProfileRequest(t, client, request)
			defer response.Body.Close()
			require.Equal(t, http.StatusBadRequest, response.StatusCode)
			message, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Contains(t, strings.ToLower(string(message)), "profile")
		})
	}
	require.Zero(t, srv.ActiveSessions(), "invalid-profile requests must not create session state")
}

func TestHTTPServer_ConnectRequiresExactEnvelopeProfile(t *testing.T) {
	_, baseURL := startTestHTTPServer(t, nil)
	client := &http.Client{Timeout: 2 * time.Second}
	for _, version := range []string{"", "go-cli-acp-json/2", "official-acp"} {
		t.Run("version="+version, func(t *testing.T) {
			request := profileHTTPRequest(t, http.MethodPost, baseURL+"/connect", "", testHTTPACPEnvelope{
				Version:  version,
				Type:     TypeConnect,
				SenderID: "display alias",
			}, true)
			response := doProfileRequest(t, client, request)
			defer response.Body.Close()
			require.Equal(t, http.StatusBadRequest, response.StatusCode)
		})
	}
}

func TestHTTPServer_SendWithoutSessionDoesNotCreateOne(t *testing.T) {
	srv, baseURL := startTestHTTPServer(t, nil)
	client := &http.Client{Timeout: 2 * time.Second}
	request := profileHTTPRequest(t, http.MethodPost, baseURL+"/send", "", testHTTPACPEnvelope{
		Type:     TypeMessage,
		SenderID: "display-only",
		Content:  "not dispatched",
	}, true)
	response := doProfileRequest(t, client, request)
	defer response.Body.Close()
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.Zero(t, srv.ActiveSessions(), "send must never implicitly create an identity/session")
}

func TestHTTPServer_RoutesRejectConflictingEnvelopeProfile(t *testing.T) {
	_, baseURL := startTestHTTPServer(t, nil)
	client := &http.Client{Timeout: 2 * time.Second}
	connect := profileHTTPRequest(t, http.MethodPost, baseURL+"/connect", "", testHTTPACPEnvelope{
		Version:  testHTTPACPProfile,
		Type:     TypeConnect,
		SenderID: "alias",
	}, true)
	sessionID := decodeConnectedSessionID(t, doProfileRequest(t, client, connect))
	send := profileHTTPRequest(t, http.MethodPost, baseURL+"/send", "", testHTTPACPEnvelope{
		Version:   "go-cli-acp-json/2",
		SessionID: sessionID,
		Type:      TypeMessage,
		SenderID:  "alias",
		Content:   "must not dispatch",
	}, true)
	response := doProfileRequest(t, client, send)
	defer response.Body.Close()
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
}

func TestHTTPServer_ConnectRandomFailureIsVisibleAndDoesNotPanic(t *testing.T) {
	srv := NewHTTPServer("test", "127.0.0.1:0", nil)
	srv.sessionIDReader = strings.NewReader("short")
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	request := profileHTTPRequest(t, http.MethodPost, "http://"+srv.Addr()+"/connect", "", testHTTPACPEnvelope{
		Version:  testHTTPACPProfile,
		Type:     TypeConnect,
		SenderID: "display",
	}, true)
	response := doProfileRequest(t, &http.Client{Timeout: 2 * time.Second}, request)
	defer response.Body.Close()
	require.Equal(t, http.StatusInternalServerError, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "unable to create session")
	require.Zero(t, srv.ActiveSessions())
}

func TestHTTPServer_DoesNotLogCredentialOrDisplayIdentity(t *testing.T) {
	previousLogger := slog.Default()
	var logOutput bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logOutput, nil)))
	defer slog.SetDefault(previousLogger)

	srv := NewHTTPServer("test", "127.0.0.1:0", nil)
	token := runtimeTestToken(t)
	alias := "display-secret-" + runtimeTestToken(t)
	srv.SetAuth(token, "principal-a")
	require.NoError(t, srv.Start(context.Background()))
	baseURL := "http://" + srv.Addr()
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	connect := profileHTTPRequest(t, http.MethodPost, baseURL+"/connect", token, testHTTPACPEnvelope{
		Version:  testHTTPACPProfile,
		Type:     TypeConnect,
		SenderID: alias,
	}, true)
	sessionID := decodeConnectedSessionID(t, doProfileRequest(t, &http.Client{Timeout: 2 * time.Second}, connect))
	send := profileHTTPRequest(t, http.MethodPost, baseURL+"/send", token, testHTTPACPEnvelope{
		SessionID: sessionID,
		Type:      TypeMessage,
		SenderID:  alias,
		Content:   "body-secret-" + alias,
	}, true)
	response := doProfileRequest(t, &http.Client{Timeout: 2 * time.Second}, send)
	response.Body.Close()
	disconnect := profileHTTPRequest(t, http.MethodPost, baseURL+"/disconnect", token, testHTTPACPEnvelope{
		SessionID: sessionID,
		Type:      TypeDisconnect,
		SenderID:  alias,
	}, true)
	response = doProfileRequest(t, &http.Client{Timeout: 2 * time.Second}, disconnect)
	response.Body.Close()

	logged := logOutput.String()
	for _, secret := range []string{token, alias, sessionID, "body-secret-"} {
		require.NotContains(t, logged, secret)
	}
}

func TestHTTPServer_AuthPrecedesProfileAndSessionLookup(t *testing.T) {
	srv := NewHTTPServer("test", "127.0.0.1:0", nil)
	token := runtimeTestToken(t)
	srv.SetAuth(token, "principal-a")
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	baseURL := "http://" + srv.Addr()
	client := &http.Client{Timeout: 2 * time.Second}

	request := profileHTTPRequest(t, http.MethodPost, baseURL+"/connect", "", testHTTPACPEnvelope{Type: TypeConnect}, false)
	response := doProfileRequest(t, client, request)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, "authentication must be checked before profile parsing")
	response.Body.Close()

	request = profileHTTPRequest(t, http.MethodGet, baseURL+"/stream?session_id=not-a-session", "incorrect-token", nil, true)
	response = doProfileRequest(t, client, request)
	require.Equal(t, http.StatusUnauthorized, response.StatusCode, "authentication must precede session lookup")
	response.Body.Close()
	require.Zero(t, srv.ActiveSessions())
}

func TestHTTPServer_OpaquePrincipalBoundSessionsAndAliases(t *testing.T) {
	srv := NewHTTPServer("test", "127.0.0.1:0", nil)
	token := runtimeTestToken(t)
	srv.SetAuth(token, "principal-a")
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	baseURL := "http://" + srv.Addr()
	client := &http.Client{Timeout: 2 * time.Second}
	post := func(path string, envelope testHTTPACPEnvelope, requestToken string) *http.Response {
		t.Helper()
		request := profileHTTPRequest(t, http.MethodPost, baseURL+path, requestToken, envelope, true)
		return doProfileRequest(t, client, request)
	}
	connect := func(alias string) string {
		t.Helper()
		return decodeConnectedSessionID(t, post("/connect", testHTTPACPEnvelope{
			Version:  testHTTPACPProfile,
			Type:     TypeConnect,
			SenderID: alias,
		}, token))
	}

	firstSession := connect("shared alias")
	secondSession := connect("shared alias")
	require.NotEqual(t, firstSession, secondSession, "same display alias must not reuse a transport session")
	require.Len(t, firstSession, 64)
	require.Len(t, secondSession, 64)
	require.NotEqual(t, "shared alias", firstSession)

	// A forged alias cannot redirect a send to another session.
	response := post("/send", testHTTPACPEnvelope{
		SessionID: secondSession,
		Type:      TypeMessage,
		SenderID:  "forged alias",
		Content:   "must not enqueue",
	}, token)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	response.Body.Close()

	response = post("/send", testHTTPACPEnvelope{
		SessionID: firstSession,
		Type:      TypeMessage,
		SenderID:  "shared alias",
		Content:   "first-session-only",
	}, token)
	require.Equal(t, http.StatusOK, response.StatusCode)
	response.Body.Close()

	streamURL := func(sessionID string) string {
		return baseURL + "/stream?session_id=" + url.QueryEscape(sessionID)
	}
	readStream := func(sessionID string) string {
		t.Helper()
		request := profileHTTPRequest(t, http.MethodGet, streamURL(sessionID), token, nil, true)
		response := doProfileRequest(t, client, request)
		defer response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return string(body)
	}
	firstBody := readStream(firstSession)
	require.Contains(t, firstBody, firstSession)
	secondBody := readStream(secondSession)
	require.Empty(t, secondBody, "another session must not receive this session's queued acknowledgement")

	// Authentication is not replaceable by a matching alias or a leaked opaque ID.
	otherToken := runtimeTestToken(t)
	srv.SetAuth(otherToken, "principal-b")
	request := profileHTTPRequest(t, http.MethodGet, streamURL(firstSession), otherToken, nil, true)
	response = doProfileRequest(t, client, request)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	response.Body.Close()

	// Restore the owning principal for disconnect and prove deleted IDs cannot be reused.
	srv.SetAuth(token, "principal-a")
	response = post("/disconnect", testHTTPACPEnvelope{
		SessionID: firstSession,
		Type:      TypeDisconnect,
		SenderID:  "shared alias",
	}, token)
	require.Equal(t, http.StatusOK, response.StatusCode)
	response.Body.Close()
	require.Equal(t, 1, srv.ActiveSessions())

	response = post("/send", testHTTPACPEnvelope{
		SessionID: firstSession,
		Type:      TypeMessage,
		SenderID:  "shared alias",
		Content:   "after disconnect",
	}, token)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	response.Body.Close()
	request = profileHTTPRequest(t, http.MethodGet, streamURL(firstSession), token, nil, true)
	response = doProfileRequest(t, client, request)
	require.Equal(t, http.StatusNotFound, response.StatusCode)
	response.Body.Close()
	require.Equal(t, 1, srv.ActiveSessions(), "unknown or deleted IDs must never create sessions")
}

func TestHTTPServer_RejectsAmbiguousProfileHeaderOnEveryRoute(t *testing.T) {
	srv, baseURL, token := startAuthenticatedIdentityHTTPServer(t, nil)
	client := &http.Client{Timeout: 2 * time.Second}
	connect := profileHTTPRequest(t, http.MethodPost, baseURL+"/connect", token, testHTTPACPEnvelope{
		Version:  testHTTPACPProfile,
		Type:     TypeConnect,
		SenderID: "profile-alias",
	}, true)
	sessionID := decodeConnectedSessionID(t, doProfileRequest(t, client, connect))

	routes := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/connect", testHTTPACPEnvelope{Version: testHTTPACPProfile, Type: TypeConnect, SenderID: "duplicate"}},
		{http.MethodPost, "/send", testHTTPACPEnvelope{SessionID: sessionID, Type: TypeMessage, SenderID: "profile-alias", Content: "must not dispatch"}},
		{http.MethodGet, "/stream?session_id=" + url.QueryEscape(sessionID), nil},
		{http.MethodGet, "/events?session_id=" + url.QueryEscape(sessionID), nil},
		{http.MethodPost, "/disconnect", testHTTPACPEnvelope{SessionID: sessionID, Type: TypeDisconnect, SenderID: "profile-alias"}},
	}
	variants := []struct {
		name   string
		values []string
	}{
		{"supported then unsupported", []string{testHTTPACPProfile, "unsupported-profile"}},
		{"unsupported then supported", []string{"unsupported-profile", testHTTPACPProfile}},
		{"comma combined", []string{testHTTPACPProfile + ", unsupported-profile"}},
	}

	for _, variant := range variants {
		for _, route := range routes {
			t.Run(variant.name+"/"+route.method+route.path, func(t *testing.T) {
				request := profileHTTPRequest(t, route.method, baseURL+route.path, token, route.body, false)
				for _, value := range variant.values {
					request.Header.Add(ACPHTTPProfileHeader, value)
				}
				response := doProfileRequest(t, client, request)
				defer response.Body.Close()
				require.Equal(t, http.StatusBadRequest, response.StatusCode)
			})
		}
	}
	require.Equal(t, 1, srv.ActiveSessions(), "ambiguous profile values must not create or remove sessions")
}

func TestHTTPServer_SameAliasDispatchEventsStaySessionScoped(t *testing.T) {
	factory := &httpIdentityAgentFactory{created: make(chan *httpIdentityAgent, 2)}
	dispatcher := core.NewDefaultSubagentDispatcher(factory)
	handler := NewCoreHandler(dispatcher, nil)
	srv, baseURL, token := startAuthenticatedIdentityHTTPServer(t, handler)
	client := &http.Client{Timeout: 2 * time.Second}
	connect := func() string {
		t.Helper()
		request := profileHTTPRequest(t, http.MethodPost, baseURL+"/connect", token, testHTTPACPEnvelope{
			Version:  testHTTPACPProfile,
			Type:     TypeConnect,
			SenderID: "shared-display-alias",
		}, true)
		return decodeConnectedSessionID(t, doProfileRequest(t, client, request))
	}
	firstID, secondID := connect(), connect()
	firstSession := identityTestSession(t, srv, firstID)
	secondSession := identityTestSession(t, srv, secondID)
	send := func(sessionID, content string) {
		t.Helper()
		request := profileHTTPRequest(t, http.MethodPost, baseURL+"/send", token, testHTTPACPEnvelope{
			SessionID: sessionID,
			Type:      TypeMessage,
			SenderID:  "shared-display-alias",
			Content:   content,
		}, true)
		response := doProfileRequest(t, client, request)
		response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
	}
	send(firstID, "first task")
	firstAgent := <-factory.created
	send(secondID, "second task")
	secondAgent := <-factory.created
	t.Cleanup(func() {
		firstAgent.stop()
		secondAgent.stop()
	})
	require.NotEqual(t, firstAgent.dispatchID, secondAgent.dispatchID, "the display alias must not become the dispatch task identity")
	firstRoute := identityDispatchRoute(t, handler, firstAgent.dispatchID)
	secondRoute := identityDispatchRoute(t, handler, secondAgent.dispatchID)
	require.Equal(t, "principal-a", firstRoute.principalID)
	require.Equal(t, firstID, firstRoute.sessionID)
	require.Same(t, firstSession, firstRoute.session)
	require.Equal(t, "principal-a", secondRoute.principalID)
	require.Equal(t, secondID, secondRoute.sessionID)
	require.Same(t, secondSession, secondRoute.session)

	firstAgent.events <- core.AgentEvent{Kind: "message", Content: "event-from-first-task", Timestamp: time.Now()}
	waitForEitherIdentityEvent(t, firstSession, secondSession, "event-from-first-task")
	firstEntries, _ := firstSession.EventHistorySince(0)
	secondEntries, _ := secondSession.EventHistorySince(0)
	require.Len(t, firstEntries, 1, "first dispatch events must remain in the session that sent the dispatch")
	require.Empty(t, secondEntries, "a matching display alias must not route another session's event")
}

func TestHTTPServer_SameAliasDispatchCleanupKeepsConcurrentRoute(t *testing.T) {
	factory := &httpIdentityAgentFactory{created: make(chan *httpIdentityAgent, 2)}
	dispatcher := core.NewDefaultSubagentDispatcher(factory)
	handler := NewCoreHandler(dispatcher, nil)
	srv, baseURL, token := startAuthenticatedIdentityHTTPServer(t, handler)
	client := &http.Client{Timeout: 2 * time.Second}
	connect := func() string {
		t.Helper()
		request := profileHTTPRequest(t, http.MethodPost, baseURL+"/connect", token, testHTTPACPEnvelope{
			Version:  testHTTPACPProfile,
			Type:     TypeConnect,
			SenderID: "shared-display-alias",
		}, true)
		return decodeConnectedSessionID(t, doProfileRequest(t, client, request))
	}
	firstID, secondID := connect(), connect()
	secondSession := identityTestSession(t, srv, secondID)
	send := func(sessionID, content string) {
		t.Helper()
		request := profileHTTPRequest(t, http.MethodPost, baseURL+"/send", token, testHTTPACPEnvelope{
			SessionID: sessionID,
			Type:      TypeMessage,
			SenderID:  "shared-display-alias",
			Content:   content,
		}, true)
		response := doProfileRequest(t, client, request)
		response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
	}
	send(firstID, "first task")
	firstAgent := <-factory.created
	send(secondID, "second task")
	secondAgent := <-factory.created
	t.Cleanup(func() {
		firstAgent.stop()
		secondAgent.stop()
	})

	firstAgent.stop()
	waitForDispatchRouteRemoval(t, handler, firstAgent.dispatchID)
	secondRoute := identityDispatchRoute(t, handler, secondAgent.dispatchID)
	require.Equal(t, "principal-a", secondRoute.principalID)
	require.Equal(t, secondID, secondRoute.sessionID)
	require.Same(t, secondSession, secondRoute.session)
	secondAgent.events <- core.AgentEvent{Kind: "message", Content: "event-from-second-task", Timestamp: time.Now()}
	waitForIdentityEvent(t, secondSession, "event-from-second-task")
}

func TestCoreHandler_CancelingSameAliasDispatchKeepsConcurrentRoute(t *testing.T) {
	factory := &httpIdentityAgentFactory{created: make(chan *httpIdentityAgent, 2)}
	dispatcher := core.NewDefaultSubagentDispatcher(factory)
	handler := NewCoreHandler(dispatcher, nil)
	firstSession := NewSession("first-session")
	secondSession := NewSession("second-session")
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstDone := make(chan struct{})
	go func() {
		handler.ProcessMessage(firstCtx, ACPMessage{Type: TypeMessage, SenderID: "same-alias", Content: "first"}, firstSession)
		close(firstDone)
	}()
	firstAgent := <-factory.created
	t.Cleanup(firstAgent.stop)
	secondDone := make(chan struct{})
	go func() {
		handler.ProcessMessage(context.Background(), ACPMessage{Type: TypeMessage, SenderID: "same-alias", Content: "second"}, secondSession)
		close(secondDone)
	}()
	secondAgent := <-factory.created
	t.Cleanup(secondAgent.stop)
	require.NotEqual(t, firstAgent.dispatchID, secondAgent.dispatchID)

	cancelFirst()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("canceled dispatch did not return")
	}
	waitForDispatchRouteRemoval(t, handler, firstAgent.dispatchID)
	secondRoute := identityDispatchRoute(t, handler, secondAgent.dispatchID)
	require.Empty(t, secondRoute.principalID, "direct CoreHandler calls must not invent an authenticated principal")
	require.Equal(t, secondSession.ID(), secondRoute.sessionID)
	require.Same(t, secondSession, secondRoute.session)
	secondAgent.events <- core.AgentEvent{Kind: "message", Content: "event-after-cancel", Timestamp: time.Now()}
	waitForIdentityEvent(t, secondSession, "event-after-cancel")

	secondAgent.stop()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("completed dispatch did not return")
	}
}

type httpIdentityAgent struct {
	dispatchID string
	events     chan core.AgentEvent
	done       chan struct{}
	stopOnce   sync.Once
}

func (a *httpIdentityAgent) stop() {
	a.stopOnce.Do(func() {
		close(a.events)
		close(a.done)
	})
}

func (a *httpIdentityAgent) Name() string { return "http-identity-agent" }
func (a *httpIdentityAgent) Run(context.Context, string) (<-chan core.AgentEvent, error) {
	return a.events, nil
}
func (a *httpIdentityAgent) Send(context.Context, string) error { return nil }
func (a *httpIdentityAgent) Interrupt(context.Context) error    { return nil }
func (a *httpIdentityAgent) Wait(ctx context.Context) (core.AgentMessage, error) {
	select {
	case <-a.done:
		return core.AgentMessage{Role: "assistant", Content: "finished"}, nil
	case <-ctx.Done():
		a.stop()
		return core.AgentMessage{}, ctx.Err()
	}
}

type httpIdentityAgentFactory struct {
	created chan *httpIdentityAgent
}

func (f *httpIdentityAgentFactory) Create(_ context.Context, dispatchID string, _ core.SubAgentConfig) (core.SubAgent, error) {
	agent := &httpIdentityAgent{
		dispatchID: dispatchID,
		events:     make(chan core.AgentEvent, 2),
		done:       make(chan struct{}),
	}
	f.created <- agent
	return agent, nil
}

func startAuthenticatedIdentityHTTPServer(t *testing.T, handler *CoreHandler) (*HTTPServer, string, string) {
	t.Helper()
	srv := NewHTTPServer("test", "127.0.0.1:0", handler)
	token := runtimeTestToken(t)
	srv.SetAuth(token, "principal-a")
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	return srv, "http://" + srv.Addr(), token
}

func identityTestSession(t *testing.T, srv *HTTPServer, sessionID string) *Session {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	binding, ok := srv.sessions[sessionID]
	require.True(t, ok)
	require.Equal(t, "principal-a", binding.principalID)
	return binding.session
}

func identityDispatchRoute(t *testing.T, handler *CoreHandler, dispatchID string) *dispatchSessionRoute {
	t.Helper()
	value, ok := handler.sessions.Load(dispatchID)
	require.True(t, ok, "dispatch route %q should remain bound", dispatchID)
	route, ok := value.(*dispatchSessionRoute)
	require.True(t, ok, "dispatch route %q should retain its session identity", dispatchID)
	return route
}

func waitForEitherIdentityEvent(t *testing.T, first, second *Session, content string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		firstEntries, _ := first.EventHistorySince(0)
		secondEntries, _ := second.EventHistorySince(0)
		for _, entries := range [][]EventEntry{firstEntries, secondEntries} {
			for _, entry := range entries {
				if entry.Event.Content == content {
					return
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("event %q was not published to either session", content)
}

func waitForIdentityEvent(t *testing.T, session *Session, content string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		entries, _ := session.EventHistorySince(0)
		for _, entry := range entries {
			if entry.Event.Content == content {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("event %q was not published to the expected session", content)
}

func waitForDispatchRouteRemoval(t *testing.T, handler *CoreHandler, dispatchID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := handler.sessions.Load(dispatchID); !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("dispatch route %q was not removed after completion", dispatchID)
}
