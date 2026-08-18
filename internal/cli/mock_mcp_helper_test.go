package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/mcp"
)

// mockMCPHandshake wraps an http.HandlerFunc to handle the MCP initialize
// handshake (initialize + notifications/initialized) before delegating to
// the original handler for all other JSON-RPC methods (e.g. tools/list).
func mockMCPHandshake(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			next(w, r)
			return
		}
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))

		var req struct {
			Method string `json:"method"`
			ID     any    `json:"id"`
		}
		if jsonErr := json.Unmarshal(body, &req); jsonErr != nil {
			next(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": mcp.LatestProtocolVersion,
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "mock", "version": "1.0"},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		case "notifications/initialized":
			w.WriteHeader(http.StatusOK)
			return
		default:
			next(w, r)
		}
	}
}

// alwaysTrustManager is a TrustManager that trusts every project path.
// It is used in tests to bypass trust checks for auto-discovery.
type alwaysTrustManager struct{}

func (alwaysTrustManager) IsTrusted(context.Context, string) bool     { return true }
func (alwaysTrustManager) TrustProject(context.Context, string) error { return nil }
func (alwaysTrustManager) RevokeTrust(context.Context, string) error  { return nil }
func (alwaysTrustManager) TrustedProjects() []string                  { return nil }

// setupTestTrust registers a trust manager that trusts all projects, so
// tests using auto-discovery (which requires project trust) can proceed.
// It restores the original trust manager on test cleanup.
func setupTestTrust(t *testing.T) {
	t.Helper()
	orig := approval.GetTrustManager()
	t.Cleanup(func() { approval.RegisterTrustManager(orig) })
	approval.RegisterTrustManager(alwaysTrustManager{})
}
