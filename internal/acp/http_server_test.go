package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
)

// startTestHTTPServer creates and starts an HTTPServer on an ephemeral port,
// returning the server and its base URL. It polls until the server is ready.
func startTestHTTPServer(t *testing.T, handler *CoreHandler) (*HTTPServer, string) {
	t.Helper()
	srv := NewHTTPServer("test", "127.0.0.1:0", handler)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	baseURL := "http://" + srv.Addr()

	// Poll until the server accepts connections.
	ready := false
	for i := 0; i < 50; i++ {
		resp, err := http.Get(baseURL + "/stream")
		if err == nil {
			resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("server did not become ready")
	}
	return srv, baseURL
}

// postACP sends a POST request with a JSON ACPMessage body.
func postACP(t *testing.T, baseURL, path string, msg ACPMessage) *http.Response {
	t.Helper()
	body, _ := json.Marshal(msg)
	resp, err := http.Post(baseURL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s failed: %v", path, err)
	}
	return resp
}

// drainStream performs a GET /stream and returns decoded messages.
func drainStream(t *testing.T, baseURL, senderID string) []ACPMessage {
	t.Helper()
	url := baseURL + "/stream"
	if senderID != "" {
		url += "?sender_id=" + senderID
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET /stream failed: %v", err)
	}
	defer resp.Body.Close()

	var msgs []ACPMessage
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var msg ACPMessage
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			t.Fatalf("failed to unmarshal stream message: %v", err)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// pollStream retries GET /stream until at least minMsgs are received or
// timeout expires. This replaces time.Sleep for async synchronization.
func pollStream(t *testing.T, baseURL, senderID string, minMsgs int) []ACPMessage {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs := drainStream(t, baseURL, senderID)
		if len(msgs) >= minMsgs {
			return msgs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d messages", minMsgs)
	return nil
}

func TestHTTPServer_StartStop(t *testing.T) {
	srv := NewHTTPServer("test", "127.0.0.1:0", nil)

	if srv.Running() {
		t.Fatal("expected not running before Start")
	}

	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if !srv.Running() {
		t.Fatal("expected running after Start")
	}

	if addr := srv.Addr(); addr == "" {
		t.Error("expected non-empty Addr after Start")
	}

	// Double start is a no-op.
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("double Start failed: %v", err)
	}

	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if srv.Running() {
		t.Fatal("expected not running after Stop")
	}

	if addr := srv.Addr(); addr != "" {
		t.Errorf("expected empty Addr after Stop, got %q", addr)
	}

	// Double stop is a no-op.
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("double Stop failed: %v", err)
	}
}

func TestHTTPServer_BindError(t *testing.T) {
	// Bind a port, then try to bind the same port again.
	srv1 := NewHTTPServer("first", "127.0.0.1:0", nil)
	if err := srv1.Start(context.Background()); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	defer srv1.Stop(context.Background())

	// Use the same address — should fail.
	srv2 := NewHTTPServer("second", srv1.Addr(), nil)
	err := srv2.Start(context.Background())
	if err == nil {
		srv2.Stop(context.Background())
		t.Fatal("expected error when binding to an in-use port")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Errorf("expected listen error, got: %v", err)
	}
}

func TestHTTPServer_Name(t *testing.T) {
	srv := NewHTTPServer("my-server", ":0", nil)
	if srv.Name() != "my-server" {
		t.Errorf("expected name 'my-server', got %q", srv.Name())
	}
}

func TestHTTPServer_EchoMode(t *testing.T) {
	// nil handler => echo/ack mode (no agent dispatch).
	srv, baseURL := startTestHTTPServer(t, nil)

	// Connect.
	resp := postACP(t, baseURL, "/connect", ACPMessage{
		Type:     TypeConnect,
		SenderID: "client-echo",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	if n := srv.ActiveSessions(); n != 1 {
		t.Errorf("expected 1 session, got %d", n)
	}

	// Send a message — echo mode should enqueue an ack.
	resp = postACP(t, baseURL, "/send", ACPMessage{
		Type:     TypeMessage,
		SenderID: "client-echo",
		Content:  "hello",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Drain the stream — should get the ack.
	msgs := pollStream(t, baseURL, "client-echo", 1)
	if msgs[0].Type != TypeAck {
		t.Errorf("expected TypeAck, got %s", msgs[0].Type)
	}
	if msgs[0].Content != "message received" {
		t.Errorf("unexpected content: %q", msgs[0].Content)
	}

	// Disconnect.
	resp = postACP(t, baseURL, "/disconnect", ACPMessage{
		Type:     TypeDisconnect,
		SenderID: "client-echo",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disconnect: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	if n := srv.ActiveSessions(); n != 0 {
		t.Errorf("expected 0 sessions after disconnect, got %d", n)
	}
}

func TestHTTPServer_DispatchMode(t *testing.T) {
	dispatched := make(chan struct{}, 1)
	d := &mockSubagentDispatcher{
		result:     core.SubagentResult{Content: "agent reply"},
		dispatched: dispatched,
	}
	handler := NewCoreHandler(d, nil)

	_, baseURL := startTestHTTPServer(t, handler)

	// Connect.
	postACP(t, baseURL, "/connect", ACPMessage{
		Type:     TypeConnect,
		SenderID: "client-dispatch",
	}).Body.Close()

	// Send a message — should be dispatched to the agent.
	postACP(t, baseURL, "/send", ACPMessage{
		Type:       TypeMessage,
		SenderID:   "client-dispatch",
		ReceiverID: "test",
		Content:    "do work",
	}).Body.Close()

	// Wait for dispatch to complete.
	<-dispatched

	// Drain the stream — should get the agent response.
	msgs := pollStream(t, baseURL, "client-dispatch", 1)
	if msgs[0].Type != TypeResponse {
		t.Errorf("expected TypeResponse, got %s", msgs[0].Type)
	}
	if msgs[0].Content != "agent reply" {
		t.Errorf("unexpected content: %q", msgs[0].Content)
	}
}

func TestHTTPServer_DispatchModeWithContextCancellation(t *testing.T) {
	// Verify that the server context (not request context) is used for dispatch.
	// A slow dispatcher should still complete because the server context stays
	// alive after the HTTP handler returns.
	dispatched := make(chan struct{}, 1)
	d := &mockSubagentDispatcher{
		result:     core.SubagentResult{Content: "slow reply"},
		delay:      100 * time.Millisecond,
		dispatched: dispatched,
	}
	handler := NewCoreHandler(d, nil)

	_, baseURL := startTestHTTPServer(t, handler)

	postACP(t, baseURL, "/connect", ACPMessage{
		Type:     TypeConnect,
		SenderID: "client-slow",
	}).Body.Close()

	// Send and immediately return — the request context is canceled, but
	// the server context should keep the dispatch alive.
	postACP(t, baseURL, "/send", ACPMessage{
		Type:     TypeMessage,
		SenderID: "client-slow",
		Content:  "slow work",
	}).Body.Close()

	<-dispatched

	msgs := pollStream(t, baseURL, "client-slow", 1)
	if msgs[0].Type != TypeResponse {
		t.Errorf("expected TypeResponse, got %s", msgs[0].Type)
	}
	if msgs[0].Content != "slow reply" {
		t.Errorf("unexpected content: %q", msgs[0].Content)
	}
}

func TestHTTPServer_MethodNotAllowed(t *testing.T) {
	_, baseURL := startTestHTTPServer(t, nil)

	for _, path := range []string{"/connect", "/send", "/disconnect"} {
		resp, err := http.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s: expected 405, got %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp, err := http.Post(baseURL+"/stream", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /stream failed: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /stream: expected 405, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPServer_InvalidJSON(t *testing.T) {
	_, baseURL := startTestHTTPServer(t, nil)

	resp, err := http.Post(baseURL+"/connect", "application/json", strings.NewReader("not-json"))
	if err != nil {
		t.Fatalf("POST /connect failed: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHTTPServer_StreamAllSessions(t *testing.T) {
	_, baseURL := startTestHTTPServer(t, nil)

	// Connect two clients.
	postACP(t, baseURL, "/connect", ACPMessage{Type: TypeConnect, SenderID: "a"}).Body.Close()
	postACP(t, baseURL, "/connect", ACPMessage{Type: TypeConnect, SenderID: "b"}).Body.Close()

	// Send messages from both.
	postACP(t, baseURL, "/send", ACPMessage{Type: TypeMessage, SenderID: "a", Content: "msg-a"}).Body.Close()
	postACP(t, baseURL, "/send", ACPMessage{Type: TypeMessage, SenderID: "b", Content: "msg-b"}).Body.Close()

	// Drain all sessions (no sender_id query param).
	msgs := pollStream(t, baseURL, "", 2)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
}

func TestHTTPServer_ConnectAnonymous(t *testing.T) {
	_, baseURL := startTestHTTPServer(t, nil)

	// Connect with empty SenderID.
	resp := postACP(t, baseURL, "/connect", ACPMessage{Type: TypeConnect})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()

	if result["session_id"] != "anonymous" {
		t.Errorf("expected session_id 'anonymous', got %q", result["session_id"])
	}
}

func TestHTTPServer_GRPCAdapterIntegration(t *testing.T) {
	// End-to-end integration test: gRPCAdapter client ↔ HTTPServer.
	// Uses nil handler (echo/ack mode) so the client should receive an ack.
	_, baseURL := startTestHTTPServer(t, nil)

	client := NewGRPCAdapter(baseURL, WithName("integration-client"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Disconnect(context.Background())

	if err := client.SendMessage(ctx, ACPMessage{
		Type:       TypeMessage,
		SenderID:   "integration-client",
		ReceiverID: "test",
		Content:    "integration test",
	}); err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	// ReceiveMessages should deliver the ack.
	select {
	case msg := <-client.ReceiveMessages():
		if msg.Type != TypeAck {
			t.Errorf("expected TypeAck, got %s", msg.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for message from ReceiveMessages")
	}
}

func TestEnsureServerName(t *testing.T) {
	if got := ensureServerName(""); got != "acp-server" {
		t.Errorf("expected 'acp-server', got %q", got)
	}
	if got := ensureServerName("   "); got != "acp-server" {
		t.Errorf("expected 'acp-server', got %q", got)
	}
	if got := ensureServerName("custom"); got != "custom" {
		t.Errorf("expected 'custom', got %q", got)
	}
}
