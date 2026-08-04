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

// EditFileToolOption configures an EditFileTool.
type EditFileToolOption func(*EditFileTool)

// WithEditDiffGenerator sets the DiffGenerator used to produce a change preview
// before applying an edit. When nil (the default) no diff is generated.
func WithEditDiffGenerator(dg DiffGenerator) EditFileToolOption {
	return func(t *EditFileTool) { t.diffGenerator = dg }
}

// EditFileTool replaces an old_string block in a file with new_string, in the
// style of apply_patch. It implements the ToolDefinition interface.
type EditFileTool struct {
	// Workdir is the base directory relative paths are resolved against.
	Workdir string
	// diffGenerator, when set, produces a diff preview of the edit. It is
	// included in the ToolResult metadata under "diff".
	diffGenerator DiffGenerator
}

var _ ToolDefinition = (*EditFileTool)(nil)

// NewEditFileTool returns an EditFileTool with a default workdir of ".".
func NewEditFileTool(opts ...EditFileToolOption) *EditFileTool {
	t := &EditFileTool{
		Workdir: defaultReadWorkdir,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name returns the tool name.
func (t *EditFileTool) Name() string { return "edit" }

// Description returns a brief description of the tool.
func (t *EditFileTool) Description() string {
	return "edit: replaces an old_string block in a file with new_string. Args: file_path (string), old_string (string), new_string (string)."
}

// Execute edits the file at file_path by locating a single occurrence of
// old_string and replacing it with new_string. The old_string must match
// exactly once; zero or multiple occurrences produce an error. An empty
// new_string removes the matched block.
func (t *EditFileTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())

	start := time.Now()

	path, ok := call.Args["file_path"].(string)
	if !ok || strings.TrimSpace(path) == "" {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.missing_path", "tool", "edit")
		return nil, errors.New("edit: missing string argument 'file_path'")
	}

	oldString, ok := call.Args["old_string"].(string)
	if !ok {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.missing_old_string", "tool", "edit")
		return nil, errors.New("edit: missing string argument 'old_string'")
	}

	var newString string
	if v, ok := call.Args["new_string"].(string); ok {
		newString = v
	}

	abspath := path
	if !filepath.IsAbs(abspath) && t.Workdir != "" {
		abspath = filepath.Join(t.Workdir, abspath)
	}
	abspath = filepath.Clean(abspath)

	info, statErr := os.Stat(abspath)
	if statErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.stat_failed", "path", abspath, "err", statErr)
		return &ToolResult{Output: ""}, fmt.Errorf("edit: %w", statErr)
	}
	if info.IsDir() {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.is_dir", "path", abspath)
		return nil, fmt.Errorf("edit: %q is a directory", abspath)
	}

	content, readErr := os.ReadFile(abspath)
	if readErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.read_failed", "path", abspath, "err", readErr)
		return &ToolResult{Output: ""}, fmt.Errorf("edit: %w", readErr)
	}

	data := string(content)
	count := strings.Count(data, oldString)
	switch count {
	case 0:
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.no_match", "path", abspath, "old_string", oldString)
		return &ToolResult{Output: ""}, fmt.Errorf("edit: old_string not found in %s", abspath)
	case 1:
		// OK: exactly one occurrence.
	default:
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.multiple_match", "path", abspath, "old_string", oldString, "count", count)
		return &ToolResult{Output: ""}, fmt.Errorf("edit: old_string matches %d times in %s, expected exactly once", count, abspath)
	}

	updated := strings.Replace(data, oldString, newString, 1)

	// When a diff generator is configured, produce a change preview that the
	// approval flow can surface. This never blocks execution; a generation
	// error is silently ignored.
	var diffPreview string
	if t.diffGenerator != nil {
		if d, derr := t.diffGenerator.Generate(data, updated, path); derr == nil {
			diffPreview = d
		}
	}

	// Write the updated content atomically (temp file + rename) so a crash or
	// write error never leaves the file truncated with partial content. The
	// original permission bits are preserved by writeAtomic.
	if _, werr := writeAtomic(abspath, []byte(updated)); werr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.write_failed", "path", abspath, "err", werr)
		return &ToolResult{Output: ""}, fmt.Errorf("edit: %w", werr)
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("edit.done",
		"path", abspath,
		"old_len", len(oldString),
		"new_len", len(newString),
		"duration_ms", time.Since(start).Milliseconds())

	meta := map[string]any{"path": abspath, "bytes": len(updated)}
	if diffPreview != "" {
		meta["diff"] = diffPreview
	}
	return &ToolResult{
		Output:   fmt.Sprintf("replaced %d-byte block with %d-byte block in %s", len(oldString), len(newString), abspath),
		Metadata: meta,
	}, nil
}
