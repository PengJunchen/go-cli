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
