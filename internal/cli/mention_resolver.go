package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
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

// Resolve searches for symbols matching the payload pattern. If an LSP
// client is available, it tries workspace/symbol first and falls back to
// WalkDir-based file search when LSP returns no results.
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
	lspUsed := false

	// Try LSP workspace symbol search first.
	if r.client != nil {
		symbols, err := r.client.WorkspaceSymbol(ctx, pattern)
		if err == nil && len(symbols) > 0 {
			lspUsed = true
			for _, sym := range symbols {
				path := strings.TrimPrefix(sym.Location.URI, "file://")
				matches = append(matches, symbolMatch{
					path: path,
					line: sym.Location.Range.Start.Line,
					text: sym.Name,
				})
				if len(matches) >= 10 {
					break
				}
			}
		}
	}

	// Fall back to WalkDir if LSP is unavailable or returned no results.
	if len(matches) == 0 {
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
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("symbol mention: no matches for %q", payload)
	}

	var results []string
	for _, m := range matches {
		results = append(results, fmt.Sprintf("%s:%d: %s", m.path, m.line+1, m.text))
	}

	// Try Hover enrichment when LSP was used successfully.
	if r.client != nil && lspUsed {
		first := matches[0]
		uri := "file://" + first.path
		if hover, err := r.client.Hover(ctx, uri, first.line, 0); err == nil && strings.TrimSpace(hover) != "" {
			results = append(results, "[hover] "+hover)
		}
	}

	return strings.Join(results, "\n"), nil
}

// --- URL resolver ---

// isInternalIP reports whether the given IP refers to a loopback, private,
// link-local, or unspecified address.
func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast()
}

// isInternalAddress reports whether the given host (optionally including a
// port) refers to a loopback, private, link-local, or unspecified address.
// It does not perform DNS resolution; only the literal hostname and IP
// address are checked. Non-IP hostnames other than "localhost" are allowed.
// IPv6 brackets (e.g. [::1]) are stripped before parsing.
func isInternalAddress(host string) bool {
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	// Strip IPv6 brackets if SplitHostPort did not apply (no port present).
	hostname = strings.TrimPrefix(strings.TrimSuffix(hostname, "]"), "[")

	if hostname == "localhost" {
		return true
	}

	ip := net.ParseIP(hostname)
	if ip == nil {
		return false
	}

	return isInternalIP(ip)
}

var (
	scriptStyleRegexp = regexp.MustCompile(`(?is)<(?:script|style)\b[^>]*>.*?</(?:script|style)>`)
	tagRegexp         = regexp.MustCompile(`(?s)<[^>]+>`)
	whitespaceRegexp  = regexp.MustCompile(`\s+`)
)

// URLMentionResolver resolves @url:<URL> mentions by fetching the page and
// converting HTML to plain text.
type URLMentionResolver struct {
	client        *http.Client
	allowInternal bool
}

// NewURLMentionResolver creates a URLMentionResolver with a 30s timeout.
// The HTTP client uses a custom dialer that blocks connections to internal
// addresses even after DNS resolution, preventing DNS-rebinding and
// non-decimal-IP bypass attacks. When allowInternal is set to true (e.g. in
// tests), the secure dialer is bypassed and a standard dialer is used.
func NewURLMentionResolver() *URLMentionResolver {
	r := &URLMentionResolver{}

	baseDialer := &net.Dialer{Timeout: 30 * time.Second}

	// secureDialer checks every resolved IP via Control, which runs after
	// DNS resolution but before the socket connects. This catches DNS
	// rebinding, decimal/hex/octal IP notation, and other hostname-based
	// bypasses that the pre-flight isInternalAddress check cannot detect.
	secureDialer := &net.Dialer{
		Timeout: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip != nil && isInternalIP(ip) {
				return fmt.Errorf("url mention: internal address %q is blocked for security", address)
			}
			return nil
		},
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if r.allowInternal {
				return baseDialer.DialContext(ctx, network, addr)
			}
			return secureDialer.DialContext(ctx, network, addr)
		},
	}

	r.client = &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}
	return r
}

// Resolve fetches the URL and returns the text content. HTML responses are
// stripped of tags and entities. Internal addresses (localhost, private IP
// ranges, etc.) are blocked to prevent SSRF attacks.
func (r *URLMentionResolver) Resolve(ctx context.Context, payload string) (string, error) {
	parsedURL, err := url.Parse(payload)
	if err != nil {
		return "", fmt.Errorf("url mention: %w", err)
	}
	if !r.allowInternal && isInternalAddress(parsedURL.Host) {
		return "", fmt.Errorf("url mention: internal address %q is blocked for security", parsedURL.Host)
	}

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
