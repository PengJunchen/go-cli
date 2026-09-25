package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

const (
	// defaultReadMaxBytes is the maximum number of bytes read in one call when
	// no explicit limit is configured.
	defaultReadMaxBytes = 1 << 20 // 1 MiB

	// defaultReadWorkdir is the base directory relative paths are resolved
	// against when no workdir is configured.
	defaultReadWorkdir = "."
)

// ReadToolOption configures a ReadTool.
type ReadToolOption func(*ReadTool)

// WithWorkdir sets the base directory that relative paths are resolved
// against.
func WithWorkdir(dir string) ReadToolOption {
	return func(t *ReadTool) { t.workdir = dir }
}

// WithMaxBytes sets the maximum number of bytes a read may return.
func WithMaxBytes(n int) ReadToolOption {
	return func(t *ReadTool) { t.maxBytes = n }
}

// WithFollowSymlinks controls whether symlinked files are followed.
func WithFollowSymlinks(b bool) ReadToolOption {
	return func(t *ReadTool) { t.FollowSymlinks = b }
}

// WithReadPathWhitelist restricts reads to the configured base paths after
// resolving existing symlinks. An empty slice preserves the unrestricted
// behavior for callers that do not configure a file-tool scope.
func WithReadPathWhitelist(paths []string) ReadToolOption {
	return func(t *ReadTool) { t.whitelist = NewPathWhitelist(paths) }
}

// ReadTool reads the contents of files and lists directories. It implements
// the ToolDefinition interface.
type ReadTool struct {
	workdir        string
	maxBytes       int
	FollowSymlinks bool
	names          []string
	whitelist      PathWhitelist
}

var _ ToolDefinition = (*ReadTool)(nil)

type readDirectoryEntry struct {
	name  string
	isDir bool
}

// NewReadTool returns a ReadTool with sensible defaults (workdir ".", max
// bytes 1 MiB). Options may override the defaults.
func NewReadTool(opts ...ReadToolOption) *ReadTool {
	t := &ReadTool{
		workdir:  defaultReadWorkdir,
		maxBytes: defaultReadMaxBytes,
		names:    []string{"read", "read_file"},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name returns the canonical tool name.
func (t *ReadTool) Name() string { return "read" }

// Names returns all names the tool can be looked up by, including aliases.
func (t *ReadTool) Names() []string { return t.names }

// Description returns a brief description of the tool.
func (t *ReadTool) Description() string {
	return "read: reads a file's contents or lists a directory. Args: path (string)."
}

// Parameters returns the JSON Schema for the path this tool reads or lists.
func (t *ReadTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"required":             []any{"path"},
		"additionalProperties": false,
	}
}

// Execute reads the file at the given path (relative paths resolve against the
// workdir). Directories are listed, not descended into. A file larger than the
// configured maximum produces an error.
func (t *ReadTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	start := time.Now()

	path, ok := call.Args["path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("read.missing_path", "tool", "read")
		return nil, errors.New("read: missing string argument 'path'")
	}

	abspath, err := t.resolvePath(path)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("read.resolve_failed", "path", path, "err", err)
		return nil, err
	}

	// Lstat does not follow symlinks, so the ModeSymlink bit is visible here.
	// os.Stat would resolve the link first and the check below would be dead.
	info, err := os.Lstat(abspath)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("read.stat_failed", "path", abspath, "err", err)
		return nil, fmt.Errorf("read: %w", err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		if !t.FollowSymlinks {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("read.symlink_denied", "path", abspath)
			return nil, fmt.Errorf("read: refusing to follow symlink %q", abspath)
		}
		// Following is allowed: stat the target to obtain accurate IsDir/Size.
		info, err = os.Stat(abspath)
		if err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("read.stat_failed", "path", abspath, "err", err)
			return nil, fmt.Errorf("read: %w", err)
		}
	}

	// Reject special files (devices, FIFOs, sockets, etc.) that never reach
	// EOF or block indefinitely, which would cause unbounded memory growth or
	// a hang inside the read below.
	if info.Mode()&(os.ModeDevice|os.ModeNamedPipe|os.ModeSocket|os.ModeIrregular) != 0 {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("read.special_file", "path", abspath, "mode", info.Mode().String())
		return nil, fmt.Errorf("read: refusing to read special file %q", abspath)
	}

	if info.IsDir() {
		directory, openErr := os.Open(abspath)
		if openErr != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("read.readdir_failed", "path", abspath, "err", openErr)
			return nil, fmt.Errorf("read: %w", openErr)
		}

		entries := newBoundedTopK[readDirectoryEntry](directoryCaptureMaxEntries, directoryCaptureMaxEntryBytes,
			func(entry readDirectoryEntry) string { return entry.name },
			func(a, b readDirectoryEntry) bool { return a.name < b.name },
		)
		listErr := scanDirectoryBatches(ctx, directory, directoryReadBatchSize, func(entry fs.DirEntry) error {
			item := readDirectoryEntry{name: entry.Name(), isDir: entry.IsDir()}
			return entries.Add(item)
		})
		if closeErr := directory.Close(); closeErr != nil {
			listErr = errors.Join(listErr, closeErr)
		}
		if listErr != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("read.readdir_failed", "path", abspath, "err", listErr)
			return nil, fmt.Errorf("read: %w", listErr)
		}

		output := newBoundedDirectoryOutput(directoryCaptureMaxOutputBytes)
		for _, entry := range entries.Sorted() {
			name := entry.name
			if entry.isDir {
				name += "/"
			}
			if !output.AppendLine(name) {
				break
			}
		}
		truncated := entries.Truncated() || output.Truncated()
		span.SetAttributes(
			tracing.Attribute{Key: "entries", Value: output.Records()},
			tracing.Attribute{Key: "truncated", Value: truncated},
			tracing.Attribute{Key: "success", Value: true},
		)
		logger.Info("read.dir",
			"path", abspath,
			"entries", output.Records(),
			"truncated", truncated,
			"duration_ms", time.Since(start).Milliseconds())

		return &ToolResult{
			Output:   output.String(),
			Metadata: map[string]any{"path": abspath, "entries": output.Records(), "truncated": truncated},
		}, nil
	}

	f, openErr := os.Open(abspath)
	if openErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("read.open_failed", "path", abspath, "err", openErr)
		return nil, fmt.Errorf("read: %w", openErr)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // best-effort close; data already read.

	// LimitReader bounds the read so an unexpectedly large or infinite stream
	// cannot exhaust memory. The +1 lets us detect truncation.
	data, readErr := io.ReadAll(io.LimitReader(f, int64(t.maxBytes)+1))
	if readErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("read.read_failed", "path", abspath, "err", readErr)
		return nil, fmt.Errorf("read: %w", readErr)
	}

	if len(data) > t.maxBytes {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("read.too_large", "path", abspath, "max", t.maxBytes)
		return nil, fmt.Errorf("read: file %q is too large, exceeding the maximum of %d bytes", abspath, t.maxBytes)
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("read.file",
		"path", abspath,
		"bytes", len(data),
		"duration_ms", time.Since(start).Milliseconds())

	return &ToolResult{
		Output:   string(data),
		Metadata: map[string]any{"path": abspath, "bytes": len(data)},
	}, nil
}

// PromptGuidelines returns usage hints for the read tool.
func (t *ReadTool) PromptGuidelines() []string {
	return []string{"Use read to examine files instead of cat or sed"}
}

// resolvePath resolves a possibly-relative path against the workdir, prevents
// workdir traversal, and applies the configured path whitelist.
func (t *ReadTool) resolvePath(path string) (string, error) {
	abspath, err := resolveWithinWorkdir("read", t.workdir, path)
	if err != nil {
		return abspath, err
	}
	if t.whitelist.unrestricted() {
		return abspath, nil
	}

	// Resolve existing symlink components, including the final component, so
	// an allowed symlink can only be followed when its target remains in scope.
	// Execute still applies the existing FollowSymlinks policy to the final link.
	// This path check is check-then-use and is not a race-free OS sandbox: a
	// concurrent symlink replacement between validation and open can race it.
	realPath := resolveSymlinks(abspath)
	if !t.whitelist.IsAllowed(realPath) {
		return abspath, fmt.Errorf("read: path %q escapes the path whitelist", path)
	}
	return abspath, nil
}
