package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// TestMentionSymbol verifies that @symbol:func:main resolves symbol definitions
// by searching .go files, both directly and via MentionExpander.
func TestMentionSymbol(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"), 0644))

	// Direct resolver test.
	resolver := NewSymbolMentionResolver(nil, "", dir)
	result, err := resolver.Resolve(context.Background(), "func:main")
	require.NoError(t, err)
	assert.Contains(t, result, "func main")
	assert.Contains(t, result, "main.go")

	// Via MentionExpander.
	e := NewMentionExpander(dir, 0)
	e.SetResolver("symbol", NewSymbolMentionResolver(nil, "", dir))
	expanded, files, _, err := e.Expand(context.Background(), "explain @symbol:func:main")
	require.NoError(t, err)
	assert.Contains(t, expanded, `<mention type="symbol">`)
	assert.Contains(t, expanded, "func main")
	assert.Contains(t, files[0], "symbol:func:main")
}

// TestMentionURL verifies that @url:<URL> fetches a web page and injects the
// extracted text, both directly and via MentionExpander.
func TestMentionURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><p>Hello World</p></body></html>`))
	}))
	defer srv.Close()

	// Direct resolver test.
	resolver := NewURLMentionResolver()
	resolver.allowInternal = true
	result, err := resolver.Resolve(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Contains(t, result, "Hello World")

	// Via MentionExpander.
	e := NewMentionExpander(t.TempDir(), 0)
	urlResolver := NewURLMentionResolver()
	urlResolver.allowInternal = true
	e.SetResolver("url", urlResolver)
	expanded, _, _, err := e.Expand(context.Background(), "fetch @url:"+srv.URL)
	require.NoError(t, err)
	assert.Contains(t, expanded, `<mention type="url">`)
	assert.Contains(t, expanded, "Hello World")
}

// TestMentionSession verifies that @session:<id> loads a historical session
// entry, both directly and via MentionExpander.
func TestMentionSession(t *testing.T) {
	store := session.NewMemoryStore()
	require.NoError(t, store.Append(context.Background(), &session.SessionEntry{
		ID:        "abc123",
		Type:      session.EntryTypeUser,
		Content:   "Previous discussion about X",
		Timestamp: time.Now(),
	}))

	// Direct resolver test.
	resolver := NewSessionMentionResolver(store)
	result, err := resolver.Resolve(context.Background(), "abc123")
	require.NoError(t, err)
	assert.Contains(t, result, "abc123")
	assert.Contains(t, result, "Previous discussion about X")

	// Via MentionExpander.
	e := NewMentionExpander(t.TempDir(), 0)
	e.SetResolver("session", NewSessionMentionResolver(store))
	expanded, _, _, err := e.Expand(context.Background(), "recall @session:abc123")
	require.NoError(t, err)
	assert.Contains(t, expanded, `<mention type="session">`)
	assert.Contains(t, expanded, "Previous discussion about X")
}

// TestSymbolMentionLSP verifies that when an LSP client is available and
// returns WorkspaceSymbol results, the symbol resolver uses the LSP path
// instead of WalkDir, and applies Hover enrichment.
func TestSymbolMentionLSP(t *testing.T) {
	client := &mockLSPClient{
		workspaceSyms: []tools.SymbolInformation{
			{
				Name:          "main",
				Kind:          12, // Function
				ContainerName: "main",
				Location: tools.Location{
					URI: "file:///test/main.go",
					Range: tools.Range{
						Start: tools.Position{Line: 5, Character: 0},
						End:   tools.Position{Line: 5, Character: 10},
					},
				},
			},
		},
		hover: "func main()",
	}

	resolver := NewSymbolMentionResolver(client, "file:///test", "/tmp")
	result, err := resolver.Resolve(context.Background(), "func:main")
	require.NoError(t, err)

	// LSP path should format as path:line: symbolName.
	assert.Contains(t, result, "main.go")
	assert.Contains(t, result, "main")
	// Line should be 1-based (0-based line 5 → 6).
	assert.Contains(t, result, ":6:")

	// Hover enrichment should be applied since LSP was used successfully.
	assert.Contains(t, result, "[hover] func main()")
}

// TestSymbolMentionLSPFallback verifies that when the LSP client returns no
// results, the resolver falls back to WalkDir.
func TestSymbolMentionLSPFallback(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"), 0644))

	// LSP returns no results → should fall back to WalkDir.
	client := &mockLSPClient{}
	resolver := NewSymbolMentionResolver(client, "", dir)
	result, err := resolver.Resolve(context.Background(), "func:main")
	require.NoError(t, err)
	assert.Contains(t, result, "func main")
	assert.Contains(t, result, "main.go")
}

// TestSymbolMentionLSPError verifies that when the LSP client returns an
// error, the resolver treats it as non-fatal and falls back to WalkDir.
func TestSymbolMentionLSPError(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"), 0644))

	// LSP returns an error → should fall back to WalkDir.
	client := &mockLSPClient{workspaceErr: errors.New("lsp server unavailable")}
	resolver := NewSymbolMentionResolver(client, "", dir)
	result, err := resolver.Resolve(context.Background(), "func:main")
	require.NoError(t, err)
	assert.Contains(t, result, "func main")
	assert.Contains(t, result, "main.go")
}

// TestSSRFProtection verifies that internal addresses are blocked by the
// URL mention resolver to prevent SSRF attacks.
func TestSSRFProtection(t *testing.T) {
	resolver := NewURLMentionResolver()

	internalURLs := []string{
		"http://localhost:8080",
		"http://10.0.0.1",
		"http://192.168.1.1",
		"http://127.0.0.1",
		"http://172.16.0.1",
		"http://0.0.0.0",
		"http://169.254.1.1",
		// IPv6 loopback without port — brackets must be stripped.
		"http://[::1]",
		// IPv6 loopback with port.
		"http://[::1]:8080",
		// IPv6 link-local.
		"http://[fe80::1]",
		// IPv6 unique-local (private).
		"http://[fc00::1]",
	}

	for _, u := range internalURLs {
		_, err := resolver.Resolve(context.Background(), u)
		require.Error(t, err, "expected security error for %s", u)
		assert.Contains(t, err.Error(), "blocked for security")
	}
}
