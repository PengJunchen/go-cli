package acp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuiltInHTTPClientLocallyJoinedAfterSuccessfulConnectAndDisconnect(t *testing.T) {
	peer := startHTTPProfilePeer(t)
	client := NewGRPCAdapter("http://"+peer.Addr(), WithName("local-join"))
	require.NoError(t, client.Connect(context.Background()))
	require.NoError(t, client.Disconnect(context.Background()))
	require.True(t, IsBuiltInHTTPClientLocallyJoined(client), "real HTTP Disconnect joins the reader and remote-delete request")
}

func TestBuiltInHTTPClientLocallyJoinedAfterFailedConnectCleanup(t *testing.T) {
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/connect" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
	}))
	defer peer.Close()

	client := NewGRPCAdapter(peer.URL, WithName("failed-connect"))
	require.Error(t, client.Connect(context.Background()))
	require.False(t, IsBuiltInHTTPClientLocallyJoined(client), "Connect failure alone does not prove its worker joined")
	require.NoError(t, client.Disconnect(context.Background()))
	require.True(t, IsBuiltInHTTPClientLocallyJoined(client), "Disconnect waits for the failed connect generation")
}

func TestBuiltInHTTPClientLocalJoinDoesNotHideRemoteDeleteFailure(t *testing.T) {
	var deletes atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/connect":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"version":    ACPHTTPProfileV1,
				"session_id": "remote-session",
			})
		case "/stream":
			<-r.Context().Done()
		case "/disconnect":
			deletes.Add(1)
			http.Error(w, "remote delete uncertain", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer peer.Close()

	client := NewGRPCAdapter(peer.URL, WithName("remote-delete-error"))
	require.NoError(t, client.Connect(context.Background()))
	err := client.Disconnect(context.Background())
	require.Error(t, err)
	require.True(t, IsBuiltInHTTPClientLocallyJoined(client), "local worker join is independent of the remote DELETE result")
	require.EqualValues(t, 1, deletes.Load())
	require.True(t, strings.Contains(err.Error(), "status 503"), "remote DELETE failure must remain observable: %v", err)
}

func TestCustomNilDisconnectHasNoBuiltInCompletionAttestation(t *testing.T) {
	client := newStubClient()
	require.NoError(t, client.Disconnect(context.Background()))
	require.False(t, IsBuiltInHTTPClientLocallyJoined(client), "third-party nil is not a trusted join proof")
}

func TestBuiltInHTTPClientPendingDisconnectAttestsOnlyAfterDone(t *testing.T) {
	peer := startHTTPProfilePeer(t)
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	var releaseOnce atomic.Bool
	release := func() {
		if releaseOnce.CompareAndSwap(false, true) {
			close(releaseClose)
		}
	}
	t.Cleanup(release)
	transport := &streamBodyCloseGateTransport{
		base:         http.DefaultTransport,
		closeStarted: closeStarted,
		releaseClose: releaseClose,
	}
	client := NewGRPCAdapter("http://"+peer.Addr(), WithName("pending-local-join"))
	client.(*gRPCAdapter).client.Transport = transport
	require.NoError(t, client.Connect(context.Background()))
	select {
	case <-closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("stream body close did not block")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := client.Disconnect(waitCtx)
	var pending interface{ Done() <-chan struct{} }
	require.ErrorAs(t, err, &pending)
	require.False(t, IsBuiltInHTTPClientLocallyJoined(client), "pending cleanup must not attest local completion")
	release()
	select {
	case <-pending.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP generation cleanup did not join")
	}
	require.True(t, IsBuiltInHTTPClientLocallyJoined(client), "completed local cleanup must be attestable")
	require.ErrorIs(t, client.Disconnect(context.Background()), context.DeadlineExceeded,
		"the first cleanup caller's expired budget remains the terminal result")
}
