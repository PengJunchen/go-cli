package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

const (
	// grepDefaultMaxMatches caps the number of matched lines returned in a
	// single call so a broad pattern cannot flood the result.
	grepDefaultMaxMatches = 1000

	// grepDefaultMaxOutput caps the total number of bytes of matched output
	// returned in a single call.
	grepDefaultMaxOutput = 1 << 20 // 1 MiB

	// grepDefaultWorkdir is the base directory relative paths are resolved
	// against when none is configured.
	grepDefaultWorkdir = "."

	// grepDefaultPath is the default search path when none is supplied.
	grepDefaultPath = "."
)

// GrepToolOption configures a GrepTool.
type GrepToolOption func(*GrepTool)

// WithGrepWorkdir sets the base directory that relative paths are resolved
// against.
func WithGrepWorkdir(dir string) GrepToolOption {
	return func(t *GrepTool) { t.Workdir = dir }
}

// WithForcePureGo forces the pure-Go search path instead of probing for
// ripgrep. This is primarily used by tests to exercise the fallback regardless
// of whether rg is installed on the machine.
func WithForcePureGo(b bool) GrepToolOption {
	return func(t *GrepTool) { t.forcePureGo = b }
}

// WithGrepMaxMatches caps the number of matched lines returned in one call.
func WithGrepMaxMatches(n int) GrepToolOption {
	return func(t *GrepTool) { t.MaxMatches = n }
}

// WithGrepMaxOutput caps the total number of bytes of matched output returned
// in one call.
func WithGrepMaxOutput(n int) GrepToolOption {
	return func(t *GrepTool) { t.MaxOutput = n }
}

// GrepTool searches for a regular expression across files in a directory,
// preferring ripgrep (rg) when available and falling back to a pure-Go
// implementation. It implements the ToolDefinition interface.
type GrepTool struct {
	// Workdir is the base directory relative paths are resolved against.
	Workdir string
	// MaxMatches caps the number of matched lines returned in one call.
	MaxMatches int
	// MaxOutput caps the total number of bytes of matched output returned in
	// one call.
	MaxOutput int
	// forcePureGo bypasses ripgrep discovery and always uses the Go fallback.
	forcePureGo bool
}

var _ ToolDefinition = (*GrepTool)(nil)

// NewGrepTool returns a GrepTool with a default workdir of ".".
func NewGrepTool(opts ...GrepToolOption) *GrepTool {
	t := &GrepTool{
		Workdir:    grepDefaultWorkdir,
		MaxMatches: grepDefaultMaxMatches,
		MaxOutput:  grepDefaultMaxOutput,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name returns the tool name.
func (t *GrepTool) Name() string { return "grep" }

// Description returns a brief description of the tool.
func (t *GrepTool) Description() string {
	return "grep: searches files under a directory for a regexp pattern. Args: pattern (string), glob (string, optional include filter), path (string, optional)."
}

// Execute searches for pattern under path (default ".") and returns matching
// lines as "file:line:content". Needs to accept an empty-path default resolved
// against the workdir.
func (t *GrepTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	start := time.Now()

	patternStr, ok := call.Args["pattern"].(string)
	if !ok || strings.TrimSpace(patternStr) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("grep.missing_pattern", "tool", "grep")
		return nil, errors.New("grep: missing string argument 'pattern'")
	}

	re, err := regexp.Compile(patternStr)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("grep.invalid_pattern", "tool", "grep", "err", err)
		return &ToolResult{Output: ""}, fmt.Errorf("grep: invalid pattern: %w", err)
	}

	var glob string
	if v, ok := call.Args["glob"].(string); ok {
		glob = v
	}

	searchPath := grepDefaultPath
	if v, ok := call.Args["path"].(string); ok && strings.TrimSpace(v) != "" {
		searchPath = v
	}
	if !filepath.IsAbs(searchPath) {
		searchPath = filepath.Join(t.Workdir, searchPath)
	}
	searchPath = filepath.Clean(searchPath)

	ms, truncated, err := t.search(ctx, re, searchPath, glob)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("grep.search_failed", "tool", "grep", "path", searchPath, "err", err)
		return &ToolResult{Output: ""}, err
	}

	output := buildMatchOutput(ms)
	if truncated {
		output += "\n[results truncated]"
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("grep.done",
		"tool", "grep",
		"path", searchPath,
		"matches", len(ms),
		"truncated", truncated,
		"duration_ms", time.Since(start).Milliseconds())

	return &ToolResult{
		Output: output,
		Metadata: map[string]any{
			"path":      searchPath,
			"matches":   len(ms),
			"truncated": truncated,
		},
	}, nil
}

// search runs ripgrep when available, otherwise the pure-Go walker. It returns
// matched lines and whether the result was truncated by the output caps.
func (t *GrepTool) search(ctx context.Context, re *regexp.Regexp, searchPath, glob string) ([]match, bool, error) {
	if !t.forcePureGo && hasRipgrep() {
		ms, err := grepWithRipgrep(ctx, re, searchPath, glob)
		if err == nil {
			tms, truncated := t.truncateMatches(ms)
			return tms, truncated, nil
		}
		// rg was present but failed (bad path, permission, etc.): fall through
		// to the pure-Go path.
	}
	tms, truncated := t.truncateMatches(grepPureGo(ctx, re, searchPath, glob))
	return tms, truncated, nil
}

// grepWithRipgrep runs `rg --line-number -n -g <glob> <pattern> <path>` and
// parses its output. The pattern is passed as a literal argument (not combined
// with flags) to avoid shell-argument injection, and --no-ignore is not used so
// behavior stays deterministic.
func grepWithRipgrep(ctx context.Context, re *regexp.Regexp, searchPath, glob string) ([]match, error) {
	args := []string{"--line-number", "-n"}
	if glob != "" {
		args = append(args, "-g", glob)
	}
	args = append(args, re.String(), searchPath)

	cmd := exec.CommandContext(ctx, "rg", args...)
	cmd.Stderr = &bytes.Buffer{}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			// rg exits 1 when no matches were found: return empty result.
			return nil, nil
		}
		return nil, fmt.Errorf("grep: rg: %w", err)
	}

	return parseRipgrepOutput(out, re.String()), nil
}

// parseRipgrepOutput splits rg "path:line:content" output into matches.
func parseRipgrepOutput(out []byte, pattern string) []match {
	var ms []match
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		file, lineNo, content, ok := splitMatchLine(line)
		if !ok {
			continue
		}
		ms = append(ms, match{File: file, Line: lineNo, Content: content})
	}
	return ms
}

// splitMatchLine parses a "file:line:content" line into its parts.
func splitMatchLine(line string) (file string, lineNo int, content string, ok bool) {
	first := strings.IndexByte(line, ':')
	if first < 0 {
		return "", 0, "", false
	}
	second := strings.IndexByte(line[first+1:], ':')
	if second < 0 {
		return "", 0, "", false
	}
	lineNumStr := line[first+1 : first+1+second]
	var n int
	if _, err := fmt.Sscanf(lineNumStr, "%d", &n); err != nil {
		return "", 0, "", false
	}
	return line[:first], n, line[first+1+second+1:], true
}

// grepPureGo walks searchPath and matches each line against re using
// regexp.MatchString, respecting the glob include filter via path.Match.
func grepPureGo(ctx context.Context, re *regexp.Regexp, searchPath, glob string) []match {
	var ms []match
	// Recursive walk is best-effort: unreadable or missing entries are skipped
	// deterministically, so any returned walk error is intentionally ignored.
	if walkErr := filepath.WalkDir(searchPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries deterministically
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if d.IsDir() {
			return nil
		}
		if glob != "" {
			// Match against the base file name and the slash-joined relative
			// path so globs like "*.go" or "**/*.txt" both apply.
			rel, rerr := filepath.Rel(searchPath, p)
			if rerr != nil {
				rel = p
			}
			slash := filepath.ToSlash(rel)
			if !matchGlob(glob, d.Name()) && !matchGlob(glob, slash) {
				return nil
			}
		}
		scanFile(p, re, &ms)
		return nil
	}); walkErr != nil {
		return ms
	}
	return ms
}

// matchGlob reports whether name matches glob, via path.Match (slash-based).
func matchGlob(glob, name string) bool {
	ok, err := path.Match(glob, name)
	return err == nil && ok
}

// scanFile reads and matches a single file's lines against re.
func scanFile(filePath string, re *regexp.Regexp, ms *[]match) {
	f, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close during scan.

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		content := sc.Text()
		if re.MatchString(content) {
			*ms = append(*ms, match{File: filePath, Line: lineNo, Content: content})
		}
	}
}

// match is a single grep result line.
type match struct {
	File    string
	Line    int
	Content string
}

// buildMatchOutput renders matches as "file:line:content" lines.
func buildMatchOutput(ms []match) string {
	var sb strings.Builder
	for _, m := range ms {
		sb.WriteString(m.File)
		sb.WriteString(":")
		sb.WriteString(fmt.Sprintf("%d", m.Line))
		sb.WriteString(":")
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// truncateMatches caps matches by the tool's count and byte-output limits.
func (t *GrepTool) truncateMatches(ms []match) ([]match, bool) {
	truncated := len(ms) > t.MaxMatches
	if truncated {
		ms = ms[:t.MaxMatches]
	}

	bytesUsed := 0
	limit := len(ms)
	for i, m := range ms {
		lineBytes := len(m.File) + 1 + len(fmt.Sprintf("%d", m.Line)) + 1 + len(m.Content) + 1
		if bytesUsed+lineBytes > t.MaxOutput {
			truncated = true
			limit = i
			break
		}
		bytesUsed += lineBytes
	}
	return ms[:limit], truncated
}

// hasRipgrep reports whether the `rg` binary is on PATH.
func hasRipgrep() bool {
	_, err := exec.LookPath("rg")
	return err == nil
}
