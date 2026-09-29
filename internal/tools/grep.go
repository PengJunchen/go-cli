package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path"
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
	capture := newGrepMatchCapture(t.MaxMatches, t.MaxOutput)
	if !t.forcePureGo && hasRipgrep() {
		lifecycle, started, err := grepWithRipgrepJSONManaged(ctx, re, searchPath, glob, capture)
		if started {
			lastLifecycle = lifecycle
			helperStarted = true
		}
		if err == nil {
			return capture.Matches(), capture.Truncated(), lastLifecycle, helperStarted, nil
		}
		if errors.Is(err, errGrepCaptureLimit) || errors.Is(err, errGrepOutputProtocol) {
			return nil, false, lastLifecycle, helperStarted, err
		}
		if !shouldFallbackAfterManagedHelper(ctx, err) {
			return nil, false, lastLifecycle, helperStarted, err
		}
		// rg was present but failed (bad path, permission, etc.): fall through
		// to the pure-Go path, but only after the managed helper has joined.
	}
	capture = newGrepMatchCapture(t.MaxMatches, t.MaxOutput)
	truncated, err := grepPureGo(ctx, re, searchPath, glob, t.whitelist, capture)
	if err != nil {
		return nil, false, lastLifecycle, helperStarted, err
	}
	return capture.Matches(), truncated, lastLifecycle, helperStarted, nil
}

// grepWithRipgrep runs ripgrep through its JSON event protocol.
func grepWithRipgrep(ctx context.Context, re *regexp.Regexp, searchPath, glob string) ([]match, error) {
	capture := newGrepMatchCapture(grepDefaultMaxMatches, grepDefaultMaxOutput)
	_, _, err := grepWithRipgrepJSONManaged(ctx, re, searchPath, glob, capture)
	if err != nil {
		return nil, err
	}
	return capture.Matches(), nil
}

// matchGlob reports whether name matches glob, via path.Match (slash-based).
func matchGlob(glob, name string) bool {
	ok, err := path.Match(glob, name)
	return err == nil && ok
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
		sb.WriteString(grepDisplayPath(m.File))
		sb.WriteString(":")
		sb.WriteString(fmt.Sprintf("%d", m.Line))
		sb.WriteString(":")
		sb.WriteString(m.Content)
		sb.WriteString("\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// hasRipgrep reports whether the `rg` binary is on PATH.
func hasRipgrep() bool {
	_, err := exec.LookPath("rg")
	return err == nil
}
