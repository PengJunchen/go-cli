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
	// defaultWriteMaxBytes is the maximum number of bytes a single write call
	// accepts when none is configured.
	defaultWriteMaxBytes = 1 << 20 // 1 MiB

	// defaultWriteWorkdir is the base directory relative paths are resolved
	// against when none is configured.
	defaultWriteWorkdir = "."
)

// WriteToolOption configures a WriteTool.
type WriteToolOption func(*WriteTool)

// WithWriteWorkdir sets the base directory that relative paths are resolved
// against.
func WithWriteWorkdir(dir string) WriteToolOption {
	return func(t *WriteTool) { t.Workdir = dir }
}

// WithWriteMaxBytes sets the maximum number of bytes a single write may write.
func WithWriteMaxBytes(n int) WriteToolOption {
	return func(t *WriteTool) { t.MaxBytes = n }
}

// WithOverwrite controls whether writing to an existing file is allowed.
func WithOverwrite(b bool) WriteToolOption {
	return func(t *WriteTool) { t.Overwrite = b }
}

// WithDiffGenerator sets the DiffGenerator used to produce a change preview
// before overwriting an existing file. When nil (the default) no diff is
// generated.
func WithDiffGenerator(dg DiffGenerator) WriteToolOption {
	return func(t *WriteTool) { t.diffGenerator = dg }
}

// WriteTool writes content to files, creating parent directories as needed.
// It implements the ToolDefinition interface.
type WriteTool struct {
	// Workdir is the base directory relative paths are resolved against.
	Workdir string
	// MaxBytes caps the size of content a single write will accept.
	MaxBytes int
	// Overwrite controls whether an existing file may be replaced.
	Overwrite bool
	// diffGenerator, when set, produces a diff preview for overwrites of
	// existing files. It is included in the ToolResult metadata under "diff".
	diffGenerator DiffGenerator
}

var _ ToolDefinition = (*WriteTool)(nil)

// NewWriteTool returns a WriteTool with sensible defaults (workdir ".", max
// bytes 1 MiB, overwrite disabled). Options may override the defaults.
func NewWriteTool(opts ...WriteToolOption) *WriteTool {
	t := &WriteTool{
		Workdir:   defaultWriteWorkdir,
		MaxBytes:  defaultWriteMaxBytes,
		Overwrite: false,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name returns the tool name.
func (t *WriteTool) Name() string { return "write" }

// Description returns a brief description of the tool.
func (t *WriteTool) Description() string {
	return "write: writes content to a file, creating parent directories. Args: path (string), content (string)."
}

// Execute writes content to the file at the given path. Relative paths resolve
// against the workdir, and parent directories are created as needed. Writing to
// an existing file fails unless Overwrite is enabled (or append is requested).
func (t *WriteTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	start := time.Now()

	path, ok := call.Args["path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("write.missing_path", "tool", "write")
		return nil, errors.New("write: missing string argument 'path'")
	}

	var content string
	if v, ok := call.Args["content"].(string); ok {
		content = v
	}

	abspath := path
	if !filepath.IsAbs(abspath) && t.Workdir != "" {
		abspath = filepath.Join(t.Workdir, abspath)
	}
	abspath = filepath.Clean(abspath)

	if len(content) > t.MaxBytes {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("write.too_large", "path", abspath, "bytes", len(content), "max", t.MaxBytes)
		return nil, fmt.Errorf("write: content is %d bytes, exceeding the maximum of %d", len(content), t.MaxBytes)
	}

	appendMode := false
	if v, ok := call.Args["append"].(bool); ok {
		appendMode = v
	}

	info, statErr := os.Stat(abspath)
	exists := statErr == nil
	if exists && info.IsDir() {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("write.is_dir", "path", abspath)
		return nil, fmt.Errorf("write: %q is a directory", abspath)
	}
	if exists && !t.Overwrite && !appendMode {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("write.exists", "path", abspath)
		return nil, fmt.Errorf("write: file %q already exists (set overwrite or append)", abspath)
	}

	parent := filepath.Dir(abspath)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("write.mkdir_failed", "dir", parent, "err", err)
		return nil, fmt.Errorf("write: %w", err)
	}

	// When a diff generator is configured and the file already exists, produce
	// a change preview that the approval flow can surface. This never blocks
	// execution; a generation error is silently ignored.
	var diffPreview string
	if t.diffGenerator != nil && exists {
		if oldBytes, rerr := os.ReadFile(abspath); rerr == nil {
			newFull := content
			if appendMode {
				newFull = string(oldBytes) + content
			}
			if d, derr := t.diffGenerator.Generate(string(oldBytes), newFull, path); derr == nil {
				diffPreview = d
			}
		}
	}

	var written int
	if appendMode && exists {
		f, err := os.OpenFile(abspath, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("write.open_failed", "path", abspath, "err", err)
			return nil, fmt.Errorf("write: %w", err)
		}
		n, werr := f.WriteString(content)
		closeErr := f.Close()
		written = n
		if werr != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("write.append_failed", "path", abspath, "err", werr)
			return nil, fmt.Errorf("write: %w", werr)
		}
		if closeErr != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("write.close_failed", "path", abspath, "err", closeErr)
			return nil, fmt.Errorf("write: %w", closeErr)
		}
	} else {
		n, werr := writeAtomic(abspath, []byte(content))
		written = n
		if werr != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("write.write_failed", "path", abspath, "err", werr)
			return nil, fmt.Errorf("write: %w", werr)
		}
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("write.done",
		"path", abspath,
		"bytes", written,
		"duration_ms", time.Since(start).Milliseconds())

	meta := map[string]any{"path": abspath, "bytes": written}
	if diffPreview != "" {
		meta["diff"] = diffPreview
	}
	return &ToolResult{
		Output:   fmt.Sprintf("wrote %d bytes to %s", written, abspath),
		Metadata: meta,
	}, nil
}

// writeAtomic writes data to a temp file in the destination directory and
// renames it into place so a crash never leaves a partially written file. The
// existing file's permission bits (or 0644 for a new file) are preserved.
func writeAtomic(dest string, data []byte) (int, error) {
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".write-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	//nolint:errcheck // best-effort temp file cleanup during error unwinding.
	defer func() { _ = os.Remove(tmpName) }()
	//nolint:errcheck // best-effort temp file close; the file is synced above.
	defer func() { _ = tmp.Close() }()

	n, err := tmp.Write(data)
	if err != nil {
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		return 0, err
	}

	if info, statErr := os.Stat(dest); statErr == nil {
		mode := info.Mode().Perm()
		if err := os.Chmod(tmpName, mode); err != nil {
			return 0, err
		}
	}

	if err := os.Rename(tmpName, dest); err != nil {
		return 0, err
	}
	return n, nil
}
