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

// WithWritePathWhitelist sets the allowed base paths for write operations.
// When configured, both the resolved real path (after symlink resolution) and
// the target file itself (via Lstat) are validated against the whitelist.
// An empty slice allows all paths (no restriction).
func WithWritePathWhitelist(paths []string) WriteToolOption {
	return func(t *WriteTool) { t.whitelist = NewPathWhitelist(paths) }
}

// WithDiffGenerator sets the DiffGenerator used to produce a change preview
// before overwriting an existing file. When nil (the default) no diff is
// generated.
func WithDiffGenerator(dg DiffGenerator) WriteToolOption {
	return func(t *WriteTool) { t.diffGenerator = dg }
}

// WithFileTracker sets the FileTracker used to create backup checkpoints
// before writing files. When nil (the default) no backup is created.
func WithFileTracker(ft *FileTracker) WriteToolOption {
	return func(t *WriteTool) { t.fileTracker = ft }
}

// resolveWithinWorkdir resolves a relative path against the workdir and
// prevents path traversal. When the path is relative and a workdir is set,
// the resolved path must remain within the workdir. Absolute paths are used
// as-is.
func resolveWithinWorkdir(toolName, workdir, path string) (string, error) {
	abspath := path
	if !filepath.IsAbs(abspath) && workdir != "" {
		abspath = filepath.Join(workdir, abspath)
	}
	abspath = filepath.Clean(abspath)

	// Prevent path traversal: when the path is relative and a workdir is
	// set, verify the resolved path stays within the workdir.
	if workdir != "" && !filepath.IsAbs(path) {
		workdirAbs, err := filepath.Abs(filepath.Clean(workdir))
		if err == nil {
			pathAbs, err := filepath.Abs(abspath)
			if err == nil && pathAbs != workdirAbs &&
				!strings.HasPrefix(pathAbs, workdirAbs+string(filepath.Separator)) {
				return abspath, fmt.Errorf("%s: path %q escapes workdir", toolName, path)
			}
		}
	}

	return abspath, nil
}

// resolveAndValidatePath resolves the target path against the workdir and
// enforces the same path whitelist and symlink checks that the bash sandbox
// applies. It performs three layers of defense:
//
//  1. resolveWithinWorkdir: relative path traversal prevention.
//  2. O_NOFOLLOW: always enabled. If the final path component exists and is a
//     symlink (checked via Lstat), the operation is rejected. This prevents
//     writing through symlinks regardless of whether a whitelist is set.
//  3. Symlink escape: when a whitelist is configured, the path is resolved
//     with resolveSymlinks (which follows symlinks in any component) and the
//     real path must fall within one of the whitelisted base directories.
//     When no whitelist is configured this check is skipped (backward-
//     compatible with callers that have not configured a whitelist), but the
//     O_NOFOLLOW check above still applies.
func resolveAndValidatePath(toolName, workdir, path string, whitelist PathWhitelist) (string, error) {
	abspath, err := resolveWithinWorkdir(toolName, workdir, path)
	if err != nil {
		return abspath, err
	}

	// O_NOFOLLOW semantics: always reject if the final path component is a
	// symlink. Lstat does not follow symlinks, so a symlink at abspath is
	// detected and rejected regardless of where it points or whether a
	// whitelist is configured. This default-enabled defense guards against
	// symlink attacks even when no whitelist is set.
	if li, lerr := os.Lstat(abspath); lerr == nil {
		if li.Mode()&os.ModeSymlink != 0 {
			return abspath, fmt.Errorf("%s: path %q is a symlink (O_NOFOLLOW)", toolName, path)
		}
	}

	// When a whitelist is configured, resolve all symlinks in the path and
	// verify the real path stays within an allowed base. This catches
	// symlinks in intermediate directories that escape the whitelist. When
	// no whitelist is configured this check is skipped, but the O_NOFOLLOW
	// check above still applies.
	if whitelist.unrestricted() {
		return abspath, nil
	}

	realPath := resolveSymlinks(abspath)
	if !whitelist.IsAllowed(realPath) {
		return abspath, fmt.Errorf("%s: path %q escapes the path whitelist", toolName, path)
	}

	return abspath, nil
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
	// whitelist, when configured, restricts writes to paths within the
	// allowed base directories. Symlink escape is detected and rejected.
	whitelist PathWhitelist
	// diffGenerator, when set, produces a diff preview for overwrites of
	// existing files. It is included in the ToolResult metadata under "diff".
	diffGenerator DiffGenerator
	// fileTracker, when set, creates backup checkpoints before writing files.
	fileTracker *FileTracker
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

// Parameters returns the JSON Schema describing the required write arguments.
func (t *WriteTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string"},
			"content": map[string]any{"type": "string"},
			"append":  map[string]any{"type": "boolean"},
		},
		"required": []any{"path", "content"},
	}
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

	content, ok := call.Args["content"].(string)
	if !ok {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("write.invalid_content", "tool", "write")
		return nil, errors.New("write: missing or non-string argument 'content'")
	}

	abspath, err := resolveAndValidatePath("write", t.Workdir, path, t.whitelist)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("write.path_traversal", "path", path, "err", err)
		return nil, err
	}

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
	if err := startPreparedEffect(ctx, func() error { return os.MkdirAll(parent, 0o750) }); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("write.mkdir_failed", "dir", parent, "err", err)
		return nil, fmt.Errorf("write: %w", err)
	}

	// When a diff generator is configured and the file already exists, produce
	// a change preview that the approval flow can surface. Ordinary preview
	// errors remain best effort, but unresolved helper execution must stop the
	// mutation before it changes the file.
	var diffPreview string
	if t.diffGenerator != nil && exists {
		if oldBytes, rerr := os.ReadFile(abspath); rerr == nil {
			newFull := content
			if appendMode {
				newFull = string(oldBytes) + content
			}
			if d, derr := t.diffGenerator.Generate(ctx, string(oldBytes), newFull, path); derr == nil {
				diffPreview = d
			} else if unresolvedExecutionError(derr) {
				span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
				logger.Error("write.diff_unresolved", "path", abspath, "err", derr)
				return nil, fmt.Errorf("write: diff execution unresolved: %w", derr)
			}
		}
	}

	// When a file tracker is configured, create a backup checkpoint before
	// writing. Errors are logged but never block the write.
	if t.fileTracker != nil {
		if _, berr := t.fileTracker.Backup(abspath); berr != nil {
			logger.Warn("write.backup_failed", "path", abspath, "err", berr)
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
		var n int
		werr := startPreparedEffect(ctx, func() error { var err error; n, err = f.WriteString(content); return err })
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
		n, werr := writeAtomicContext(ctx, abspath, []byte(content))
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

// PromptGuidelines returns usage hints for the write tool.
func (t *WriteTool) PromptGuidelines() []string {
	return []string{"Use write to create new files or overwrite existing ones"}
}

// writeAtomicContext writes data to a temp file in the destination directory and
// renames it into place so a crash never leaves a partially written file. The
// existing file's permission bits (or 0644 for a new file) are preserved.
func writeAtomicContext(ctx context.Context, dest string, data []byte) (int, error) {
	var tmp *os.File
	err := startPreparedEffect(ctx, func() error { var err error; tmp, err = os.CreateTemp(filepath.Dir(dest), ".write-*"); return err })
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

	if err := startPreparedEffect(ctx, func() error { return os.Rename(tmpName, dest) }); err != nil {
		return 0, err
	}
	return n, nil
}
