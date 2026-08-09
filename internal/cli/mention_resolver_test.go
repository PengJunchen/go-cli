package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/session"
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
	result, err := resolver.Resolve(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Contains(t, result, "Hello World")

	// Via MentionExpander.
	e := NewMentionExpander(t.TempDir(), 0)
	e.SetResolver("url", NewURLMentionResolver())
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
