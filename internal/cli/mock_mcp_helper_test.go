package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/mcp"
)

// mockMCPHandshake validates the Streamable HTTP 2025-06-18 request envelope,
// handles the initialize handshake and initialized notification, and delegates
// ordinary JSON-RPC requests to next.
func mockMCPHandshake(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Streamable HTTP uses POST for client messages", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" ||
			!strings.Contains(r.Header.Get("Accept"), "application/json") ||
			!strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			http.Error(w, "invalid Streamable HTTP headers", http.StatusBadRequest)
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
			JSONRPC string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			ID      json.RawMessage `json:"id"`
			Params  struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		if jsonErr := json.Unmarshal(body, &req); jsonErr != nil {
			http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
			return
		}
		if req.JSONRPC != "2.0" || req.Method == "" {
			http.Error(w, "invalid JSON-RPC envelope", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			if !validMockMCPJSONRPCID(req.ID) || req.Params.ProtocolVersion != mcp.LatestProtocolVersion || r.Header.Get("MCP-Protocol-Version") != "" {
				http.Error(w, "invalid initialize request", http.StatusBadRequest)
				return
			}
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result": map[string]any{
					"protocolVersion": mcp.LatestProtocolVersion,
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "mock", "version": "1.0"},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		case "notifications/initialized":
			if len(req.ID) != 0 || r.Header.Get("MCP-Protocol-Version") != mcp.LatestProtocolVersion {
				http.Error(w, "invalid initialized notification", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			return
		default:
			if !validMockMCPJSONRPCID(req.ID) || r.Header.Get("MCP-Protocol-Version") != mcp.LatestProtocolVersion {
				http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
				return
			}
			next(w, r)
		}
	}
}

func validMockMCPJSONRPCID(raw json.RawMessage) bool {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch value.(type) {
	case string, float64:
		return true
	default:
		return false
	}
}

// writeMockMCPResult responds to the current JSON-RPC request using its exact
// ID. The adapter performs initialize before tools/list, so fixture responses
// must not assume that tools/list has a fixed request ID.
func writeMockMCPResult(w http.ResponseWriter, r *http.Request, result any) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read JSON-RPC request: "+err.Error(), http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))

	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(body, &request); err != nil || !validMockMCPJSONRPCID(request.ID) {
		http.Error(w, "JSON-RPC request has no ID", http.StatusBadRequest)
		return
	}
	response := map[string]any{
		"jsonrpc": "2.0",
		"id":      request.ID,
		"result":  result,
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "encode JSON-RPC response: "+err.Error(), http.StatusInternalServerError)
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

type canonicalMCPTestTrustManager struct {
	*approval.DefaultTrustManager
}

func (m canonicalMCPTestTrustManager) IsTrusted(ctx context.Context, path string) bool {
	return m.DefaultTrustManager.IsTrusted(ctx, canonicalMCPProjectRoot(path))
}

func trustedMCPTestConfig(t *testing.T) *config.Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	baseManager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore()).(*approval.DefaultTrustManager)
	manager := canonicalMCPTestTrustManager{DefaultTrustManager: baseManager}
	previous := approval.GetTrustManager()
	approval.RegisterTrustManager(manager)
	t.Cleanup(func() { approval.RegisterTrustManager(previous) })

	uncurated, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	snapshot := uncurated.CapturedProjectSourcesForGrant()
	if snapshot != nil {
		require.NoError(t, manager.TrustProjectSourceSnapshot(context.Background(), snapshot))
	}
	trusted, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	return trusted
}

func trustedGlobalMCPTestConfig(t *testing.T, servers []config.MCPServerConfig) *config.Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".go-cli")
	require.NoError(t, os.MkdirAll(globalDir, 0o700))
	data, err := json.Marshal(config.Config{MCP: config.MCPConfig{Servers: servers}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "config.json"), data, 0o600))
	baseManager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore()).(*approval.DefaultTrustManager)
	manager := canonicalMCPTestTrustManager{DefaultTrustManager: baseManager}
	previous := approval.GetTrustManager()
	approval.RegisterTrustManager(manager)
	t.Cleanup(func() { approval.RegisterTrustManager(previous) })
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	return cfg
}

func mockMCPInputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}
