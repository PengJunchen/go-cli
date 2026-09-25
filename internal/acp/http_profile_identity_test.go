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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
