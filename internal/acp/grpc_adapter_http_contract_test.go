package acp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func startHTTPProfilePeer(t *testing.T) *HTTPServer {
	t.Helper()
	server := NewHTTPServer("loopback", "127.0.0.1:0", nil)
	require.NoError(t, server.Start(context.Background()))
	t.Cleanup(func() { _ = server.Stop(context.Background()) }) //nolint:errcheck // test cleanup
	return server
}

func TestGRPCAdapterPrivateHTTPProfileLoopback(t *testing.T) {
	server := startHTTPProfilePeer(t)
	client := NewGRPCAdapter("http://"+server.Addr(), WithName("display-alias"))
	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))
	inbound := client.ReceiveMessages()
	require.NotNil(t, inbound)

	server.mu.Lock()
	require.Len(t, server.sessions, 1)
	var sessionID string
	var binding httpSessionBinding
	for id, current := range server.sessions {
		sessionID = id
		binding = current
	}
	server.mu.Unlock()
	require.NotEmpty(t, sessionID)
	require.NotEqual(t, "display-alias", sessionID)
	require.Equal(t, "display-alias", binding.senderAlias)

	require.NoError(t, client.SendMessage(ctx, ACPMessage{
		Type: TypeMessage, SenderID: "display-alias", ReceiverID: "loopback", Content: "hello",
	}))
	select {
	case response, ok := <-inbound:
		require.True(t, ok, "connection reader must remain open before disconnect")
		require.Equal(t, TypeAck, response.Type)
		require.Equal(t, "message received", response.Content)
		require.Equal(t, "display-alias", response.ReceiverID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the loopback HTTPServer response")
	}

	require.NoError(t, client.Disconnect(ctx))
	require.Nil(t, client.ReceiveMessages())
	server.mu.Lock()
	remaining := len(server.sessions)
	server.mu.Unlock()
	require.Zero(t, remaining, "disconnect must remove the exact server session")
}

func TestGRPCAdapterAuthenticatedHTTPProfileLoopback(t *testing.T) {
	const token = "loopback-test-bearer-token"
	server := NewHTTPServer("loopback-auth", "127.0.0.1:0", nil)
	server.SetAuth(token, "principal-a")
	require.NoError(t, server.Start(context.Background()))
	t.Cleanup(func() { _ = server.Stop(context.Background()) }) //nolint:errcheck // test cleanup

	client := NewGRPCAdapter(
		"http://"+server.Addr(),
		WithName("same-display-alias"),
		WithHTTPBearerToken(token),
	)
	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))
	server.mu.Lock()
	sessionCount := len(server.sessions)
	var principalID, displayAlias string
	for _, binding := range server.sessions {
		principalID = binding.principalID
		displayAlias = binding.senderAlias
	}
	server.mu.Unlock()
	require.Equal(t, 1, sessionCount)
	require.Equal(t, "principal-a", principalID)
	require.Equal(t, "same-display-alias", displayAlias)
	require.NoError(t, client.SendMessage(ctx, ACPMessage{
		Type: TypeMessage, SenderID: "same-display-alias", Content: "authenticated",
	}))
	select {
	case response := <-client.ReceiveMessages():
		require.Equal(t, TypeAck, response.Type)
		require.Equal(t, "message received", response.Content)
		require.Equal(t, "same-display-alias", response.ReceiverID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for authenticated loopback response")
	}
	require.NoError(t, client.Disconnect(ctx))
}

func TestGRPCAdapterRejectsWrongHTTPBearerTokenWithoutLeaking(t *testing.T) {
	server := NewHTTPServer("loopback-auth", "127.0.0.1:0", nil)
	server.SetAuth("expected-test-token", "principal-a")
	require.NoError(t, server.Start(context.Background()))
	t.Cleanup(func() { _ = server.Stop(context.Background()) }) //nolint:errcheck // test cleanup

	const wrongToken = "wrong-test-token"
	client := NewGRPCAdapter("http://"+server.Addr(), WithHTTPBearerToken(wrongToken))
	err := client.Connect(context.Background())
	require.Error(t, err)
	require.ErrorIs(t, err, errHTTPUnauthorized)
	require.NotContains(t, err.Error(), wrongToken)
	server.mu.Lock()
	remaining := len(server.sessions)
	server.mu.Unlock()
	require.Zero(t, remaining)
}

func TestGRPCAdapterUnknownHTTPConnectOutcomeFencesWithoutReplay(t *testing.T) {
	var connectRequests atomic.Int32
	var disconnectRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connect":
			connectRequests.Add(1)
			profiles := r.Header.Values(ACPHTTPProfileHeader)
			if len(profiles) != 1 || profiles[0] != ACPHTTPProfileV1 {
				http.Error(w, "profile mismatch", http.StatusBadRequest)
				return
			}
			var envelope httpACPEnvelope
			if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil || envelope.Version != ACPHTTPProfileV1 {
				http.Error(w, "version mismatch", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"go-cli-acp-json/1"}`))
		case "/disconnect":
			disconnectRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := NewGRPCAdapter(server.URL, WithName("unknown-create"))
	err := client.Connect(context.Background())
	require.ErrorIs(t, err, ErrHTTPConnectOutcomeUnknown)
	require.ErrorIs(t, err, errHTTPACPProtocol)
	cleanupErr := client.Disconnect(context.Background())
	require.ErrorIs(t, cleanupErr, ErrHTTPConnectOutcomeUnknown)
	require.ErrorIs(t, client.Connect(context.Background()), ErrHTTPConnectOutcomeUnknown)
	require.ErrorIs(t, client.Disconnect(context.Background()), ErrHTTPConnectOutcomeUnknown)
	require.EqualValues(t, 1, connectRequests.Load(), "an unknowable create must not be replayed")
	require.Zero(t, disconnectRequests.Load(), "the client cannot invent an opaque session ID to disconnect")
}

func TestGRPCAdapterKnownHTTPConnectDenialAllowsExplicitRetry(t *testing.T) {
	var connectRequests atomic.Int32
	var disconnectRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connect":
			if connectRequests.Add(1) == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"go-cli-acp-json/1","session_id":"opaque-after-denial"}`))
		case "/stream":
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
		case "/disconnect":
			disconnectRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client := NewGRPCAdapter(server.URL, WithName("known-denial"))
	err := client.Connect(context.Background())
	require.ErrorIs(t, err, errHTTPForbidden)
	require.NotErrorIs(t, err, ErrHTTPConnectOutcomeUnknown)
	require.NoError(t, client.Disconnect(context.Background()), "a known denial created no session to clean up")
	require.NoError(t, client.Connect(context.Background()), "the caller may explicitly retry after known denial cleanup")
	require.EqualValues(t, 2, connectRequests.Load())
	require.NoError(t, client.Disconnect(context.Background()))
	require.EqualValues(t, 1, disconnectRequests.Load())
}

func TestGRPCAdapterHTTPBearerTokenIsNotForwardedAcrossRedirect(t *testing.T) {
	const token = "must-not-leak-token"
	forwarded := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/connect", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)

	client := NewGRPCAdapter(redirect.URL, WithHTTPBearerToken(token))
	err := client.Connect(context.Background())
	require.Error(t, err)
	require.NotContains(t, err.Error(), token)
	select {
	case auth := <-forwarded:
		t.Fatalf("bearer token was forwarded to redirect target: %q", auth)
	default:
	}
}

func TestGRPCAdapterRejectsEndpointUserInfoAndQueryWithoutLeaking(t *testing.T) {
	const sensitive = "endpoint-secret"
	client := NewGRPCAdapter("http://user:"+sensitive+"@127.0.0.1:1234/acp?key="+sensitive, WithHTTPBearerToken(sensitive))
	ctx, exporter, _ := newTraceContext(t, "trace-http-endpoint-redaction")
	err := client.Connect(ctx)
	require.Error(t, err)
	require.NotContains(t, err.Error(), sensitive)
	require.NotContains(t, err.Error(), "user")
	span := waitForSpan(t, exporter, "acp.connect")
	require.Equal(t, "<invalid-endpoint>", spanAttr(span, "endpoint"))
	require.NotContains(t, spanAttr(span, "endpoint"), sensitive)
	adapter := client.(*gRPCAdapter)
	require.NotContains(t, adapter.safeEndpointOrigin(), sensitive)
	require.NotContains(t, adapter.safeEndpointOrigin(), "user")
	require.Equal(t, "<invalid-endpoint>", adapter.safeEndpointOrigin())
}
