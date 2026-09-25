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

// WithGrepPathWhitelist sets the allowed base paths for grep operations.
// When configured, the resolved real path (after symlink resolution) is
// validated against the whitelist, providing defense-in-depth against symlink
// escape and absolute path access outside the workdir. An empty slice allows
// all paths (no restriction beyond traversal checks).
func WithGrepPathWhitelist(paths []string) GrepToolOption {
	return func(t *GrepTool) { t.whitelist = NewPathWhitelist(paths) }
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
	// whitelist, when configured, restricts searches to paths within the
	// allowed base directories. Symlink escape is detected and rejected.
	whitelist PathWhitelist
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

// Parameters returns the JSON Schema for grep's model-supplied arguments.
func (t *GrepTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string"},
			"glob":    map[string]any{"type": "string"},
			"path":    map[string]any{"type": "string"},
		},
		"required":             []any{"pattern"},
		"additionalProperties": false,
	}
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

	searchPath, err = resolveAndValidatePath("grep", t.Workdir, searchPath, t.whitelist)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("grep.path_traversal", "tool", "grep", "path", searchPath, "err", err)
		return nil, err
	}

	ms, truncated, lifecycle, helperStarted, err := t.search(ctx, re, searchPath, glob)
	if helperStarted {
		reportPreparedLocalProcessCompletion(ctx, lifecycle)
	}
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
// matched lines, output truncation, and the last started helper lifecycle.
func (t *GrepTool) search(ctx context.Context, re *regexp.Regexp, searchPath, glob string) ([]match, bool, managedLocalResult, bool, error) {
	var lastLifecycle managedLocalResult
	helperStarted := false
	if !t.forcePureGo && hasRipgrep() {
		ms, lifecycle, started, err := grepWithRipgrepManaged(ctx, re, searchPath, glob)
		if started {
			lastLifecycle = lifecycle
			helperStarted = true
		}
		if err == nil {
			tms, truncated := t.truncateMatches(ms)
			return tms, truncated, lastLifecycle, helperStarted, nil
		}
		if !shouldFallbackAfterManagedHelper(ctx, err) {
			return nil, false, lastLifecycle, helperStarted, err
		}
		// rg was present but failed (bad path, permission, etc.): fall through
		// to the pure-Go path, but only after the managed helper has joined.
	}
	ms, err := grepPureGo(ctx, re, searchPath, glob, t.whitelist)
	if err != nil {
		return nil, false, lastLifecycle, helperStarted, err
	}
	tms, truncated := t.truncateMatches(ms)
	return tms, truncated, lastLifecycle, helperStarted, nil
}

// grepWithRipgrep runs `rg --no-config --line-number -n [-g <glob>] -e
// <pattern> -- <path>` and parses its output. Pattern and glob values cannot
// be parsed as options, and ripgrep cannot load ambient configuration.
func grepWithRipgrep(ctx context.Context, re *regexp.Regexp, searchPath, glob string) ([]match, error) {
	matches, _, _, err := grepWithRipgrepManaged(ctx, re, searchPath, glob)
	return matches, err
}

func grepWithRipgrepManaged(ctx context.Context, re *regexp.Regexp, searchPath, glob string) ([]match, managedLocalResult, bool, error) {
	args := []string{"--no-config", "--line-number", "-n"}
	if glob != "" {
		args = append(args, "--glob="+glob)
	}
	args = append(args, "-e", re.String(), "--", searchPath)

	cmd := exec.Command("rg", args...)
	cmd.Env = make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, "RIPGREP_CONFIG_PATH") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	out, lifecycle, err := runManagedSearchCommand(ctx, cmd)
	if err != nil {
		return nil, lifecycle, false, fmt.Errorf("grep: rg: %w", err)
	}
	if err := managedSearchLifecycleError(ctx, "grep: rg", lifecycle); err != nil {
		return nil, lifecycle, true, fmt.Errorf("grep: rg: %w", err)
	}
	if lifecycle.runErr != nil {
		var ee *exec.ExitError
		if errors.As(lifecycle.runErr, &ee) && ee.ExitCode() == 1 {
			// rg exits 1 when no matches were found: return empty result.
			return nil, lifecycle, true, nil
		}
		return nil, lifecycle, true, fmt.Errorf("grep: rg: %w", lifecycle.runErr)
	}

	return parseRipgrepOutput(out, re.String()), lifecycle, true, nil
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

// grepPureGo walks searchPath and matches each line against re using Go's
// regexp engine, respecting the glob include filter via path.Match. It skips
// symlinks and non-regular files rather than following paths outside the
// validated search root or blocking on special files.
func grepPureGo(ctx context.Context, re *regexp.Regexp, searchPath, glob string, whitelist PathWhitelist) ([]match, error) {
	var ms []match
	var scanErr error
	walkErr := filepath.WalkDir(searchPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return nil // skip unreadable entries deterministically
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
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
		if err := scanFile(ctx, p, re, &ms, whitelist); err != nil {
			scanErr = err
			return err
		}
		return nil
	})
	if scanErr != nil {
		return nil, scanErr
	}
	if walkErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, walkErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return ms, nil
}

// matchGlob reports whether name matches glob, via path.Match (slash-based).
func matchGlob(glob, name string) bool {
	ok, err := path.Match(glob, name)
	return err == nil && ok
}

// scanFile reads and matches one regular, in-scope file. A context error is
// returned to the walk so cancellation cannot be reported as partial success.
func scanFile(ctx context.Context, filePath string, re *regexp.Regexp, ms *[]match, whitelist PathWhitelist) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return nil // preserve best-effort behavior for entries that disappeared.
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	if !whitelist.unrestricted() && !whitelist.IsAllowed(resolveSymlinks(filePath)) {
		return nil
	}
	f, err := os.Open(filePath)
	if err != nil {
		return nil // preserve best-effort behavior for unreadable files.
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close during scan.

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !sc.Scan() {
			break
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		lineNo++
		content := sc.Text()
		if re.MatchString(content) {
			*ms = append(*ms, match{File: filePath, Line: lineNo, Content: content})
		}
	}
	if scanErr := sc.Err(); scanErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return scanErr
	}
	return ctx.Err()
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
