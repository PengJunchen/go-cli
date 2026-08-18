package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

const defaultMentionMaxBytes = 64 * 1024

// MentionExpander expands @filepath tokens in user input by inlining file content.
type MentionExpander struct {
	cwd       string
	maxBytes  int
	resolvers map[string]MentionResolver
}

// NewMentionExpander creates a MentionExpander rooted at cwd. If maxBytes is
// zero or negative, defaultMentionMaxBytes is used.
func NewMentionExpander(cwd string, maxBytes int) *MentionExpander {
	if maxBytes <= 0 {
		maxBytes = defaultMentionMaxBytes
	}
	return &MentionExpander{cwd: cwd, maxBytes: maxBytes, resolvers: make(map[string]MentionResolver)}
}

// SetResolver registers a resolver for a typed mention prefix (e.g. "symbol",
// "url", "session"). Must be called before the first Expand call (during
// initialization); Expand is not safe to call concurrently with SetResolver.
func (e *MentionExpander) SetResolver(typ string, r MentionResolver) {
	e.resolvers[typ] = r
}

// mentionRegexp matches @path tokens while avoiding email addresses.
// The leading capture group ensures @ is not preceded by a word char, dot, @, or hyphen.
var mentionRegexp = regexp.MustCompile(`(^|[^\w.@-])@([\w./\-~]+)`)

// typedMentionRegexp matches typed @-mentions such as @symbol:func:main or
// @url:https://example.com. Capture groups: prefix, type, payload.
var typedMentionRegexp = regexp.MustCompile(`(^|[^\w.@-])@(symbol|url|session):(\S+)`)

// Expand scans input for typed @-mentions (Phase 1) and @path file tokens
// (Phase 2), replacing them with resolved content. It returns the expanded
// string, the list of mentions that were resolved, the total number of content
// bytes inlined, and any error.
func (e *MentionExpander) Expand(ctx context.Context, input string) (string, []string, int, error) {
	var files []string
	var totalBytes int

	// Phase 1: typed mentions (@symbol:, @url:, @session:).
	phase1 := input
	if len(e.resolvers) > 0 {
		phase1 = typedMentionRegexp.ReplaceAllStringFunc(input, func(match string) string {
			sub := typedMentionRegexp.FindStringSubmatch(match)
			if len(sub) < 4 {
				return match
			}
			prefix, typ, payload := sub[1], sub[2], sub[3]

			resolver, ok := e.resolvers[typ]
			if !ok {
				return match
			}

			content, err := resolver.Resolve(ctx, payload)
			if err != nil {
				slog.Warn("mention_resolver_failed", "type", typ, "payload", payload, "err", err)
				return match
			}

			files = append(files, typ+":"+payload)
			totalBytes += len(content)

			return prefix + fmt.Sprintf(`<mention type="%s">%s</mention>`, typ, content)
		})
	}

	// Phase 2: file @path mentions (unchanged, operates on Phase 1 output).
	result := mentionRegexp.ReplaceAllStringFunc(phase1, func(match string) string {
		// Extract the @path part (skip leading non-word char if present)
		idx := strings.Index(match, "@")
		if idx < 0 {
			return match
		}
		prefix := match[:idx]
		path := match[idx+1:]

		content, ok := e.expandFile(path)
		if !ok {
			return match // file doesn't exist, keep original token
		}

		files = append(files, path)
		totalBytes += len(content)

		return prefix + fmt.Sprintf(`<file path="%s"><contents>%s</contents></file>`, path, content)
	})

	return result, files, totalBytes, nil
}

// expandFile reads a file and returns its content as a string.
// Returns (content, false) if the file doesn't exist or is not a regular file.
func (e *MentionExpander) expandFile(path string) (string, bool) {
	fullPath := path
	if !filepath.IsAbs(fullPath) {
		fullPath = filepath.Join(e.cwd, fullPath)
	}

	info, err := os.Stat(fullPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}

	f, err := os.Open(fullPath)
	if err != nil {
		return "", false
	}
	defer f.Close() //nolint:errcheck

	limited := io.LimitReader(f, int64(e.maxBytes+1))
	data, err := io.ReadAll(limited)
	if err != nil {
		return "", false
	}

	truncated := false
	if len(data) > e.maxBytes {
		data = data[:e.maxBytes]
		truncated = true
	}

	// Check for binary content (null bytes or invalid UTF-8)
	if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
		return fmt.Sprintf("[binary file, %d bytes]", info.Size()), true
	}

	content := string(data)
	if truncated {
		content += fmt.Sprintf("\n[truncated at %d bytes]", e.maxBytes)
	}

	return content, true
}
