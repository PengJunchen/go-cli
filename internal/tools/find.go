package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

const (
	// findDefaultWorkdir is the base directory relative paths are resolved
	// against when none is configured.
	findDefaultWorkdir = "."
	// findDefaultPath is the search root when no path is supplied.
	findDefaultPath = "."
	// findDefaultMaxDepth disables depth limiting (0 means unlimited).
	findDefaultMaxDepth = 0
	// findMaxResults caps the number of paths returned in one call.
	findMaxResults = 1000
)

// FindToolOption configures a FindTool.
type FindToolOption func(*FindTool)

// WithFindWorkdir sets the base directory relative paths are resolved against.
func WithFindWorkdir(dir string) FindToolOption {
	return func(t *FindTool) { t.Workdir = dir }
}

// WithFindForceNode forces the pure-Go walker instead of probing for the `fd`
// or `find` binaries. This is primarily used by tests to exercise the fallback
// deterministically.
func WithFindForceNode(b bool) FindToolOption {
	return func(t *FindTool) { t.forceNode = b }
}

// FindTool searches for files and directories under a path using a pattern,
// preferring the external `fd` binary when available, followed by `find`, and
// falling back to a pure-Go implementation. It implements the ToolDefinition
// interface.
type FindTool struct {
	// Workdir is the base directory relative paths are resolved against.
	Workdir string
	// forceNode bypasses external binary discovery and always uses the Go
	// walker.
	forceNode bool
}

var _ ToolDefinition = (*FindTool)(nil)

// NewFindTool returns a FindTool with a default workdir of ".".
func NewFindTool(opts ...FindToolOption) *FindTool {
	t := &FindTool{Workdir: findDefaultWorkdir}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name returns the tool name.
func (t *FindTool) Name() string { return "find" }

// Description returns a brief description of the tool.
func (t *FindTool) Description() string {
	return "find: searches for files/directories under a path by name pattern. Args: path (string, optional), pattern (string, optional glob), type (string, optional: f|d), max_depth (int, optional)."
}

// Execute searches under path (default ".") for entries whose base name
// matches pattern (default matches everything). type filters to files ("f") or
// directories ("d"); max_depth limits traversal depth. Results are returned as
// one path per line.
func (t *FindTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	start := time.Now()

	searchPath := findDefaultPath
	if v := getStringArg(call, "path"); strings.TrimSpace(v) != "" {
		searchPath = v
	}
	if !filepath.IsAbs(searchPath) {
		searchPath = filepath.Join(t.Workdir, searchPath)
	}
	searchPath = filepath.Clean(searchPath)

	pattern := getStringArg(call, "pattern")
	kind := strings.ToLower(strings.TrimSpace(getStringArg(call, "type")))

	maxDepth := findDefaultMaxDepth
	if v, ok := call.Args["max_depth"].(float64); ok {
		maxDepth = int(v)
	} else if v, ok := call.Args["max_depth"].(int); ok {
		maxDepth = v
	}

	matches, err := t.find(ctx, searchPath, pattern, kind, maxDepth)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "path", Value: searchPath}, tracing.Attribute{Key: "pattern", Value: pattern}, tracing.Attribute{Key: "success", Value: false})
		logger.Error("find.failed", "tool", "find", "path", searchPath, "pattern", pattern, "err", err)
		return &ToolResult{Output: ""}, err
	}

	sort.Strings(matches)
	if len(matches) > findMaxResults {
		matches = matches[:findMaxResults]
	}

	output := strings.Join(matches, "\n")

	span.SetAttributes(
		tracing.Attribute{Key: "path", Value: searchPath},
		tracing.Attribute{Key: "pattern", Value: pattern},
		tracing.Attribute{Key: "matches", Value: len(matches)},
		tracing.Attribute{Key: "success", Value: true},
	)
	logger.Info("find.done",
		"tool", "find",
		"path", searchPath,
		"pattern", pattern,
		"matches", len(matches),
		"duration_ms", time.Since(start).Milliseconds())

	return &ToolResult{
		Output:   output,
		Metadata: map[string]any{"path": searchPath, "pattern": pattern, "matches": len(matches)},
	}, nil
}

// find runs fd, then find, then falls back to the pure-Go walker.
func (t *FindTool) find(ctx context.Context, searchPath, pattern, kind string, maxDepth int) ([]string, error) {
	if !t.forceNode {
		if paths, ok, err := findWithFd(ctx, searchPath, pattern, kind, maxDepth); ok {
			return paths, err
		}
		if paths, ok, err := findWithFind(ctx, searchPath, pattern, kind, maxDepth); ok {
			return paths, err
		}
	}
	return findPureGo(ctx, searchPath, pattern, kind, maxDepth)
}

// findWithFd runs `fd --type <kind> --max-depth <n> <pattern> <path>` when the
// fd binary is available. ok is false when fd is not on PATH or fails.
func findWithFd(ctx context.Context, searchPath, pattern, kind string, maxDepth int) ([]string, bool, error) {
	if _, err := exec.LookPath("fd"); err != nil {
		return nil, false, nil
	}
	args := []string{}
	if kind == "f" {
		args = append(args, "--type", "f")
	} else if kind == "d" {
		args = append(args, "--type", "d")
	}
	if maxDepth > 0 {
		args = append(args, "--max-depth", fmt.Sprintf("%d", maxDepth))
	}
	if pattern != "" {
		args = append(args, pattern)
	}
	args = append(args, searchPath)

	cmd := exec.CommandContext(ctx, "fd", args...)
	cmd.Stderr = &bytes.Buffer{}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			// fd exits 1 when no matches: empty result is valid.
			return []string{}, true, nil
		}
		return nil, false, err
	}
	return splitLines(string(out)), true, nil
}

// findWithFind runs `find <path> -maxdepth <n> -type <kind> -name <pattern>`.
// ok is false when the find binary is unavailable or fails.
func findWithFind(ctx context.Context, searchPath, pattern, kind string, maxDepth int) ([]string, bool, error) {
	path, err := exec.LookPath("find")
	if err != nil {
		return nil, false, nil
	}
	args := []string{searchPath}
	if maxDepth > 0 {
		args = append(args, "-maxdepth", fmt.Sprintf("%d", maxDepth))
	}
	switch kind {
	case "f":
		args = append(args, "-type", "f")
	case "d":
		args = append(args, "-type", "d")
	}
	if pattern != "" {
		args = append(args, "-name", pattern)
	}

	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stderr = &bytes.Buffer{}
	out, err := cmd.Output()
	if err != nil {
		return nil, false, err
	}
	return splitLines(string(out)), true, nil
}

// findPureGo walks searchPath using filepath.WalkDir, matching entries by base
// name against pattern. It returns matched paths relative to searchPath when
// possible (mirroring fd/find output), otherwise absolute.
func findPureGo(ctx context.Context, searchPath, pattern, kind string, maxDepth int) ([]string, error) {
	var matches []string
	info, err := os.Stat(searchPath)
	if err != nil {
		return nil, fmt.Errorf("find: %w", err)
	}
	if !info.IsDir() {
		// A single file path: include it if its base name matches.
		if findMatches(filepath.Base(searchPath), pattern) && findKindOK(kind, false) {
			return []string{searchPath}, nil
		}
		return []string{}, nil
	}

	base, err := filepath.Abs(searchPath)
	if err != nil {
		base = searchPath
	}
	walkErr := filepath.WalkDir(searchPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries deterministically
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if p == searchPath {
			return nil // skip the root dir itself
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			rel = p
		}
		depth := len(strings.Split(filepath.ToSlash(rel), "/"))
		if maxDepth > 0 && depth > maxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !findKindOK(kind, d.IsDir()) {
			return nil
		}
		name := d.Name()
		if pattern != "" && !matchGlob(pattern, name) {
			return nil
		}
		matches = append(matches, filepath.Clean(rel))
		return nil
	})
	// The walk is best-effort: unreadable entries are skipped deterministically
	// inside the callback, so a root-level error still returns partial results.
	if walkErr != nil && ctx.Err() != nil {
		return matches, ctx.Err()
	}
	return matches, nil
}

// findKindOK reports whether a DirEntry of the given isDir-ness should be
// included given the requested type filter. type "" matches everything.
func findKindOK(kind string, isDir bool) bool {
	switch kind {
	case "f":
		return !isDir
	case "d":
		return isDir
	default:
		return true
	}
}

// findMatches reports whether name matches pattern, where an empty pattern
// matches everything.
func findMatches(name, pattern string) bool {
	if pattern == "" {
		return true
	}
	return matchGlob(pattern, name)
}

// splitLines splits output into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
