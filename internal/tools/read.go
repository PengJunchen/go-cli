package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// ReadTool reads the contents of files and lists directories. It implements
// the ToolDefinition interface.
type ReadTool struct {
	workdir        string
	maxBytes       int
	FollowSymlinks bool
	names          []string
}

var _ ToolDefinition = (*ReadTool)(nil)

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

	info, err := os.Stat(abspath)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("read.stat_failed", "path", abspath, "err", err)
		return nil, fmt.Errorf("read: %w", err)
	}

	if info.IsDir() {
		entries, listErr := os.ReadDir(abspath)
		if listErr != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("read.readdir_failed", "path", abspath, "err", listErr)
			return nil, fmt.Errorf("read: %w", listErr)
		}

		var sb strings.Builder
		for _, e := range entries {
			sb.WriteString(e.Name())
			if e.IsDir() {
				sb.WriteString("/")
			}
			sb.WriteString("\n")
		}

		span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
		logger.Info("read.dir",
			"path", abspath,
			"entries", len(entries),
			"duration_ms", time.Since(start).Milliseconds())

		return &ToolResult{
			Output:   strings.TrimSuffix(sb.String(), "\n"),
			Metadata: map[string]any{"path": abspath, "entries": len(entries)},
		}, nil
	}

	if !t.FollowSymlinks && info.Mode()&os.ModeSymlink != 0 {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("read.symlink_denied", "path", abspath)
		return nil, fmt.Errorf("read: refusing to follow symlink %q", abspath)
	}

	data, readErr := os.ReadFile(abspath)
	if readErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("read.read_failed", "path", abspath, "err", readErr)
		return nil, fmt.Errorf("read: %w", readErr)
	}

	if len(data) > t.maxBytes {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("read.too_large", "path", abspath, "bytes", len(data), "max", t.maxBytes)
		return nil, fmt.Errorf("read: file %q is %d bytes, exceeding the maximum of %d", abspath, len(data), t.maxBytes)
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

// resolvePath resolves a possibly-relative path against the workdir and cleans
// the result.
func (t *ReadTool) resolvePath(path string) (string, error) {
	if !filepath.IsAbs(path) && t.workdir != "" {
		path = filepath.Join(t.workdir, path)
	}
	return filepath.Clean(path), nil
}
