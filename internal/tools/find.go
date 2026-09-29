package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
	findMaxResults = directoryCaptureMaxEntries
)

type findCaptureResult struct {
	paths     []string
	truncated bool
}

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

// WithFindPathWhitelist sets the allowed base paths for find operations.
// When configured, the resolved real path (after symlink resolution) is
// validated against the whitelist, providing defense-in-depth against symlink
// escape and absolute path access outside the workdir. An empty slice allows
// all paths (no restriction beyond traversal checks).
func WithFindPathWhitelist(paths []string) FindToolOption {
	return func(t *FindTool) { t.whitelist = NewPathWhitelist(paths) }
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
	// whitelist, when configured, restricts searches to paths within the
	// allowed base directories. Symlink escape is detected and rejected.
	whitelist PathWhitelist
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

// Parameters returns the JSON Schema for find's optional search filters.
func (t *FindTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":      map[string]any{"type": "string"},
			"pattern":   map[string]any{"type": "string"},
			"type":      map[string]any{"type": "string", "enum": []string{"", "f", "d"}},
			"max_depth": map[string]any{"type": "integer"},
		},
		"additionalProperties": false,
	}
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

	searchPath, err := resolveAndValidatePath("find", t.Workdir, searchPath, t.whitelist)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "path", Value: searchPath}, tracing.Attribute{Key: "success", Value: false})
		logger.Error("find.path_traversal", "tool", "find", "path", searchPath, "err", err)
		return nil, err
	}

	pattern := getStringArg(call, "pattern")
	kind := strings.ToLower(strings.TrimSpace(getStringArg(call, "type")))

	maxDepth := findDefaultMaxDepth
	if v, ok := call.Args["max_depth"].(float64); ok {
		maxDepth = int(v)
	} else if v, ok := call.Args["max_depth"].(int); ok {
		maxDepth = v
	}

	captured, lifecycle, helperStarted, err := t.find(ctx, searchPath, pattern, kind, maxDepth)
	if helperStarted {
		reportPreparedLocalProcessCompletion(ctx, lifecycle)
	}
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "path", Value: searchPath}, tracing.Attribute{Key: "pattern", Value: pattern}, tracing.Attribute{Key: "success", Value: false})
		logger.Error("find.failed", "tool", "find", "path", searchPath, "pattern", pattern, "err", err)
		return &ToolResult{Output: ""}, err
	}

	output := newBoundedDirectoryOutput(directoryCaptureMaxOutputBytes)
	for _, match := range captured.paths {
		if !output.AppendLine(directoryDisplayName(match)) {
			break
		}
	}
	truncated := captured.truncated || output.Truncated()

	span.SetAttributes(
		tracing.Attribute{Key: "path", Value: searchPath},
		tracing.Attribute{Key: "pattern", Value: pattern},
		tracing.Attribute{Key: "matches", Value: output.Records()},
		tracing.Attribute{Key: "truncated", Value: truncated},
		tracing.Attribute{Key: "success", Value: true},
	)
	logger.Info("find.done",
		"tool", "find",
		"path", searchPath,
		"pattern", pattern,
		"matches", output.Records(),
		"truncated", truncated,
		"duration_ms", time.Since(start).Milliseconds())

	return &ToolResult{
		Output:   output.String(),
		Metadata: map[string]any{"path": searchPath, "pattern": pattern, "matches": output.Records(), "truncated": truncated},
	}, nil
}

// find runs fd, then find, then falls back to the pure-Go walker.
func (t *FindTool) find(ctx context.Context, searchPath, pattern, kind string, maxDepth int) (findCaptureResult, managedLocalResult, bool, error) {
	if info, err := os.Stat(searchPath); err == nil && !info.IsDir() {
		captured, err := findPureGo(ctx, searchPath, pattern, kind, maxDepth)
		return captured, managedLocalResult{}, false, err
	}

	var lastLifecycle managedLocalResult
	helperStarted := false
	if !t.forceNode {
		captured, ok, lifecycle, started, err := findWithFdManaged(ctx, searchPath, pattern, kind, maxDepth)
		if started {
			lastLifecycle = lifecycle
			helperStarted = true
		}
		if !shouldFallbackAfterManagedHelper(ctx, err) {
			return findCaptureResult{}, lastLifecycle, helperStarted, err
		}
		if ok {
			return captured, lastLifecycle, helperStarted, err
		}

		captured, ok, lifecycle, started, err = findWithFindManaged(ctx, searchPath, pattern, kind, maxDepth)
		if started {
			lastLifecycle = lifecycle
			helperStarted = true
		}
		if !shouldFallbackAfterManagedHelper(ctx, err) {
			return findCaptureResult{}, lastLifecycle, helperStarted, err
		}
		if ok {
			return captured, lastLifecycle, helperStarted, err
		}
	}
	captured, err := findPureGo(ctx, searchPath, pattern, kind, maxDepth)
	return captured, lastLifecycle, helperStarted, err
}

// findWithFd runs fd with glob matching and NUL-separated output when the fd
// binary is available. The separator prevents a model-supplied pattern from
// enabling fd execution options. ok is false when fd is not on PATH or fails.
func findWithFd(ctx context.Context, searchPath, pattern, kind string, maxDepth int) ([]string, bool, error) {
	captured, ok, _, _, err := findWithFdManaged(ctx, searchPath, pattern, kind, maxDepth)
	return captured.paths, ok, err
}

func findWithFdManaged(ctx context.Context, searchPath, pattern, kind string, maxDepth int) (findCaptureResult, bool, managedLocalResult, bool, error) {
	if _, err := exec.LookPath("fd"); err != nil {
		return findCaptureResult{}, false, managedLocalResult{}, false, nil
	}
	args := []string{"--print0", "--glob", "--case-sensitive", "--hidden", "--no-ignore"}
	if kind == "f" {
		args = append(args, "--type", "f")
	} else if kind == "d" {
		args = append(args, "--type", "d")
	}
	if maxDepth > 0 {
		args = append(args, "--max-depth", fmt.Sprintf("%d", maxDepth))
	}
	args = append(args, "--", pattern)
	args = append(args, searchPath)

	cmd := exec.Command("fd", args...)
	capture := newFindPathCaptureWriter(findMaxResults, directoryCaptureMaxEntryBytes)
	lifecycle, err := runManagedSearchCommandWithStdoutWriter(ctx, cmd, capture)
	if err != nil {
		return findCaptureResult{}, false, lifecycle, false, err
	}
	if err := managedSearchLifecycleError(ctx, "find: fd", lifecycle); err != nil {
		if captureErr := capture.CaptureError(); captureErr != nil {
			err = errors.Join(err, captureErr)
		}
		return findCaptureResult{}, false, lifecycle, true, fmt.Errorf("find: fd: %w", err)
	}
	captureErr := capture.Finish()
	if captureErr != nil {
		return findCaptureResult{}, false, lifecycle, true, errors.Join(captureErr, lifecycle.runErr)
	}
	if lifecycle.runErr != nil {
		return findCaptureResult{}, false, lifecycle, true, lifecycle.runErr
	}
	return findCaptureResult{paths: capture.Results(), truncated: capture.Truncated()}, true, lifecycle, true, nil
}

func findWithFindManaged(ctx context.Context, searchPath, pattern, kind string, maxDepth int) (findCaptureResult, bool, managedLocalResult, bool, error) {
	path, err := exec.LookPath("find")
	if err != nil {
		return findCaptureResult{}, false, managedLocalResult{}, false, nil
	}
	args := []string{searchPath}
	if info, statErr := os.Stat(searchPath); statErr == nil && info.IsDir() {
		args = append(args, "-mindepth", "1")
	}
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
	args = append(args, "-print0")

	cmd := exec.Command(path, args...)
	capture := newFindPathCaptureWriter(findMaxResults, directoryCaptureMaxEntryBytes)
	lifecycle, err := runManagedSearchCommandWithStdoutWriter(ctx, cmd, capture)
	if err != nil {
		return findCaptureResult{}, false, lifecycle, false, err
	}
	if err := managedSearchLifecycleError(ctx, "find: find", lifecycle); err != nil {
		if captureErr := capture.CaptureError(); captureErr != nil {
			err = errors.Join(err, captureErr)
		}
		return findCaptureResult{}, false, lifecycle, true, fmt.Errorf("find: find: %w", err)
	}
	captureErr := capture.Finish()
	if captureErr != nil {
		return findCaptureResult{}, false, lifecycle, true, errors.Join(captureErr, lifecycle.runErr)
	}
	if lifecycle.runErr != nil {
		return findCaptureResult{}, false, lifecycle, true, lifecycle.runErr
	}
	return findCaptureResult{paths: capture.Results(), truncated: capture.Truncated()}, true, lifecycle, true, nil
}

// findPureGo walks searchPath depth-first with one-entry ReadDir calls,
// matching entries by base name against pattern. It returns matched paths
// relative to searchPath when possible (mirroring fd/find output), otherwise
// absolute. Open directory handles are bounded by traversal depth.
func findPureGo(ctx context.Context, searchPath, pattern, kind string, maxDepth int) (result findCaptureResult, err error) {
	if err := ctx.Err(); err != nil {
		return findCaptureResult{}, err
	}
	info, err := os.Stat(searchPath)
	if err != nil {
		return findCaptureResult{}, fmt.Errorf("find: %w", err)
	}
	if !info.IsDir() {
		// A single file path: include it if its base name matches.
		if findMatches(filepath.Base(searchPath), pattern) && findKindOK(kind, info.Mode()) {
			paths := newBoundedTopK[string](findMaxResults, directoryCaptureMaxEntryBytes,
				func(path string) string { return path }, func(a, b string) bool { return a < b })
			if err := paths.Add(searchPath); err != nil {
				return findCaptureResult{}, err
			}
			return findCaptureResult{paths: paths.Sorted(), truncated: paths.Truncated()}, nil
		}
		return findCaptureResult{}, nil
	}
	rootInfo, err := os.Lstat(searchPath)
	if err != nil {
		return findCaptureResult{}, fmt.Errorf("find: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		// WalkDir does not descend through a symlink when the symlink itself is
		// the search root, and its root callback is excluded from results.
		return findCaptureResult{}, nil
	}

	root, err := os.Open(searchPath)
	if err != nil {
		return findCaptureResult{}, fmt.Errorf("find: open root: %w", err)
	}
	stack := []findDirectoryFrame{{file: root, fullPath: searchPath}}
	defer func() {
		for i := len(stack) - 1; i >= 0; i-- {
			if closeErr := stack[i].file.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("find: close directory: %w", closeErr))
			}
		}
	}()
	paths := newBoundedTopK[string](findMaxResults, directoryCaptureMaxEntryBytes,
		func(path string) string { return path }, func(a, b string) bool { return a < b })
	truncated := false
	popFrame := func() error {
		index := len(stack) - 1
		file := stack[index].file
		stack = stack[:index]
		return file.Close()
	}

	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return findCaptureResult{}, err
		}
		frame := &stack[len(stack)-1]
		entries, readErr := frame.file.ReadDir(1)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return findCaptureResult{}, ctxErr
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if frame.depth == 0 {
				return findCaptureResult{}, fmt.Errorf("find: read root: %w", readErr)
			}
			truncated = true
			if closeErr := popFrame(); closeErr != nil {
				return findCaptureResult{}, errors.Join(fmt.Errorf("find: read child: %w", readErr), closeErr)
			}
			continue
		}
		if len(entries) == 0 {
			if readErr == nil {
				if frame.depth == 0 {
					return findCaptureResult{}, io.ErrNoProgress
				}
				truncated = true
			}
			if closeErr := popFrame(); closeErr != nil {
				return findCaptureResult{}, fmt.Errorf("find: close directory: %w", closeErr)
			}
			continue
		}

		entry := entries[0]
		if maxDepth > 0 && frame.depth+1 > maxDepth {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return findCaptureResult{}, ctxErr
			}
			truncated = true
			continue
		}
		name := entry.Name()
		relativePath := filepath.Join(frame.relativePath, name)
		fullPath := filepath.Join(frame.fullPath, name)
		isDir := info.IsDir()
		if findKindOK(kind, info.Mode()) && findMatches(name, pattern) {
			cleanPath := filepath.Clean(relativePath)
			if err := paths.Add(cleanPath); err != nil {
				return findCaptureResult{}, err
			}
		}
		if !isDir || (maxDepth > 0 && frame.depth+1 >= maxDepth) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return findCaptureResult{}, err
		}
		child, openErr := os.Open(fullPath)
		if openErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return findCaptureResult{}, ctxErr
			}
			truncated = true
			continue
		}
		if err := ctx.Err(); err != nil {
			_ = child.Close()
			return findCaptureResult{}, err
		}
		stack = append(stack, findDirectoryFrame{file: child, fullPath: fullPath, relativePath: relativePath, depth: frame.depth + 1})
	}

	return findCaptureResult{paths: paths.Sorted(), truncated: truncated || paths.Truncated()}, nil
}

type findDirectoryFrame struct {
	file         *os.File
	fullPath     string
	relativePath string
	depth        int
}

// findKindOK reports whether an entry with the given mode should be included
// given the requested type filter. type "" matches everything.
func findKindOK(kind string, mode os.FileMode) bool {
	switch kind {
	case "f":
		return mode.IsRegular()
	case "d":
		return mode.IsDir()
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
