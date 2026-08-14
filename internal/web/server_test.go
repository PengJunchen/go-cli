package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// startTestWebServer creates and starts a WebServer on an ephemeral port,
// returning the base URL. It polls until the server is ready.
func startTestWebServer(t *testing.T) string {
	t.Helper()
	srv := NewWebServer("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	baseURL := "http://" + srv.Addr()

	// Poll until the server accepts connections.
	for i := 0; i < 50; i++ {
		resp, err := http.Get(baseURL + "/api/health")
		if err == nil {
			resp.Body.Close()
			return baseURL
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not become ready")
	return ""
}

func TestWebServer_Health(t *testing.T) {
	baseURL := startTestWebServer(t)

	resp, err := http.Get(baseURL + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if result["status"] != "ok" {
		t.Fatalf("expected status \"ok\", got %q", result["status"])
	}
}

func TestWebServer_Index(t *testing.T) {
	baseURL := startTestWebServer(t)

	resp, err := http.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET / failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	if !strings.Contains(string(body), "go-cli Web UI") {
		t.Fatalf("response body does not contain \"go-cli Web UI\"")
	}
}

func TestWebServer_Chat(t *testing.T) {
	baseURL := startTestWebServer(t)

	reqBody, _ := json.Marshal(map[string]string{"message": "hello"})
	resp, err := http.Post(baseURL+"/api/chat", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /api/chat failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if _, ok := result["response"]; !ok {
		t.Fatalf("response does not contain \"response\" field, got %v", result)
	}
}

// startTestWebServerWithAuth creates and starts a WebServer with the given
// auth token on an ephemeral port, returning the base URL. It polls until the
// server is ready using the index page, which does not require authentication.
func startTestWebServerWithAuth(t *testing.T, token string) string {
	t.Helper()
	srv := NewWebServer("127.0.0.1:0", WithAuthToken(token))
	if err := srv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	baseURL := "http://" + srv.Addr()

	// Poll until the server accepts connections. Use the index page since
	// API endpoints require auth.
	for i := 0; i < 50; i++ {
		resp, err := http.Get(baseURL + "/")
		if err == nil {
			resp.Body.Close()
			return baseURL
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not become ready")
	return ""
}

func TestWebServer_AuthRequired(t *testing.T) {
	baseURL := startTestWebServerWithAuth(t, "secret-token")

	// Without token → 401.
	resp, err := http.Get(baseURL + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}

	// Wrong token → 401.
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/api/health", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/health with wrong token failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", resp.StatusCode)
	}

	// Correct token → 200.
	req, _ = http.NewRequest(http.MethodGet, baseURL+"/api/health", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/health with correct token failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with correct token, got %d", resp.StatusCode)
	}

	// Index page is accessible without auth.
	resp2, err := http.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET / without token failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for index without auth, got %d", resp2.StatusCode)
	}
}

func TestWebServer_UnauthorizedAccess(t *testing.T) {
	baseURL := startTestWebServerWithAuth(t, "secret-token")

	reqBody, _ := json.Marshal(map[string]string{"message": "hello"})

	// POST /api/chat without token → 401.
	resp, err := http.Post(baseURL+"/api/chat", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /api/chat failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}

	// POST /api/chat with correct token → 200.
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/chat", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/chat with token failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with correct token, got %d", resp.StatusCode)
	}
}

func TestWebServer_SSEConcurrencyLimit(t *testing.T) {
	srv := NewWebServer("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	baseURL := "http://" + srv.Addr()

	// Verify the semaphore has the expected capacity.
	if cap(srv.sseSem) != maxSSEConnections {
		t.Fatalf("expected sseSem capacity %d, got %d", maxSSEConnections, cap(srv.sseSem))
	}

	// Fill the semaphore to simulate all slots being in use.
	for i := 0; i < maxSSEConnections; i++ {
		srv.sseSem <- struct{}{}
	}

	// A new SSE connection should be rejected with 503.
	resp, err := http.Get(baseURL + "/ws")
	if err != nil {
		t.Fatalf("GET /ws failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when SSE slots exhausted, got %d", resp.StatusCode)
	}

	// Release one slot so the next connection can succeed.
	<-srv.sseSem

	// A new SSE connection should now succeed. Use a short-lived context so
	// the streaming handler exits promptly.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/ws", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /ws failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after releasing a slot, got %d", resp2.StatusCode)
	}
}
