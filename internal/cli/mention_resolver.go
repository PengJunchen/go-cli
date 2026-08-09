package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// MentionResolver resolves a typed @-mention (e.g. @symbol:func:main) into
// text content that replaces the mention token in the user message.
type MentionResolver interface {
	Resolve(ctx context.Context, payload string) (string, error)
}

// --- Symbol resolver ---

// SymbolMentionResolver resolves @symbol:<kind>:<name> mentions by searching
// .go files for matching lines and, when an LSP client is available, enriching
// the result with hover information.
type SymbolMentionResolver struct {
	client  tools.LSPClient
	rootURI string
	cwd     string
}

// NewSymbolMentionResolver creates a SymbolMentionResolver. The client may be
// nil, in which case only file-text search is performed.
func NewSymbolMentionResolver(client tools.LSPClient, rootURI, cwd string) *SymbolMentionResolver {
	return &SymbolMentionResolver{client: client, rootURI: rootURI, cwd: cwd}
}

type symbolMatch struct {
	path string
	line int // 0-based
	text string
}

// Resolve searches .go files under cwd for lines matching the payload pattern.
// The payload is either "kind:name" (e.g. "func:main") or a bare "name".
func (r *SymbolMentionResolver) Resolve(ctx context.Context, payload string) (string, error) {
	pattern := payload
	if idx := strings.Index(payload, ":"); idx >= 0 {
		kind := payload[:idx]
		name := payload[idx+1:]
		if kind != "" && name != "" {
			pattern = kind + " " + name
		}
	}

	var matches []symbolMatch
	_ = filepath.WalkDir(r.cwd, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if strings.Contains(line, pattern) {
				matches = append(matches, symbolMatch{path: path, line: i, text: strings.TrimSpace(line)})
				if len(matches) >= 10 {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})

	if len(matches) == 0 {
		return "", fmt.Errorf("symbol mention: no matches for %q", payload)
	}

	var results []string
	for _, m := range matches {
		results = append(results, fmt.Sprintf("%s:%d: %s", m.path, m.line+1, m.text))
	}

	if r.client != nil {
		first := matches[0]
		uri := "file://" + first.path
		if hover, err := r.client.Hover(ctx, uri, first.line, 0); err == nil && strings.TrimSpace(hover) != "" {
			results = append(results, "[hover] "+hover)
		}
	}

	return strings.Join(results, "\n"), nil
}

// --- URL resolver ---

var (
	scriptStyleRegexp = regexp.MustCompile(`(?is)<(?:script|style)\b[^>]*>.*?</(?:script|style)>`)
	tagRegexp         = regexp.MustCompile(`(?s)<[^>]+>`)
	whitespaceRegexp  = regexp.MustCompile(`\s+`)
)

// URLMentionResolver resolves @url:<URL> mentions by fetching the page and
// converting HTML to plain text.
type URLMentionResolver struct {
	client *http.Client
}

// NewURLMentionResolver creates a URLMentionResolver with a 30s timeout.
func NewURLMentionResolver() *URLMentionResolver {
	return &URLMentionResolver{
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Resolve fetches the URL and returns the text content. HTML responses are
// stripped of tags and entities.
func (r *URLMentionResolver) Resolve(ctx context.Context, payload string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, payload, nil)
	if err != nil {
		return "", fmt.Errorf("url mention: %w", err)
	}
	req.Header.Set("User-Agent", "go-cli/mention-resolver")

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("url mention: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("url mention: HTTP %d for %s", resp.StatusCode, payload)
	}

	limited := io.LimitReader(resp.Body, 64*1024)
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("url mention: %w", err)
	}

	if strings.Contains(resp.Header.Get("Content-Type"), "html") {
		return stripHTML(string(data)), nil
	}
	return string(data), nil
}

// stripHTML removes script/style blocks, HTML tags, decodes basic entities,
// and collapses whitespace.
func stripHTML(s string) string {
	s = scriptStyleRegexp.ReplaceAllString(s, "")
	s = tagRegexp.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = whitespaceRegexp.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// --- Session resolver ---

// SessionMentionResolver resolves @session:<id> mentions by loading the
// historical session entry from the store.
type SessionMentionResolver struct {
	store session.SessionStore
}

// NewSessionMentionResolver creates a SessionMentionResolver backed by store.
func NewSessionMentionResolver(store session.SessionStore) *SessionMentionResolver {
	return &SessionMentionResolver{store: store}
}

// Resolve loads the session entry by ID and returns a formatted summary.
func (r *SessionMentionResolver) Resolve(ctx context.Context, payload string) (string, error) {
	if r.store == nil {
		return "", fmt.Errorf("session mention: no session store configured")
	}

	entry, err := r.store.Get(ctx, payload)
	if err != nil || entry == nil {
		return "", fmt.Errorf("session mention: session not found: %s", payload)
	}

	content := entry.Content
	if content == "" {
		content = entry.Summary
	}
	if content == "" {
		return "", fmt.Errorf("session mention: empty session entry: %s", payload)
	}

	return fmt.Sprintf("[Session %s — %s]\n%s", entry.ID, entry.Type, content), nil
}
