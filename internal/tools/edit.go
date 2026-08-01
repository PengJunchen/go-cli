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

// EditFileTool replaces an old_string block in a file with new_string, in the
// style of apply_patch. It implements the ToolDefinition interface.
type EditFileTool struct {
	// Workdir is the base directory relative paths are resolved against.
	Workdir string
}

var _ ToolDefinition = (*EditFileTool)(nil)

// NewEditFileTool returns an EditFileTool with a default workdir of ".".
func NewEditFileTool() *EditFileTool {
	return &EditFileTool{
		Workdir: defaultReadWorkdir,
	}
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

	// Open the existing file for writing (no O_CREATE) and truncate it before
	// writing the updated content, preserving the original permission bits.
	f, openErr := os.OpenFile(abspath, os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if openErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.open_failed", "path", abspath, "err", openErr)
		return &ToolResult{Output: ""}, fmt.Errorf("edit: %w", openErr)
	}
	_, writeErr := f.WriteString(updated)
	closeErr := f.Close()
	if writeErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.write_failed", "path", abspath, "err", writeErr)
		return &ToolResult{Output: ""}, fmt.Errorf("edit: %w", writeErr)
	}
	if closeErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.close_failed", "path", abspath, "err", closeErr)
		return &ToolResult{Output: ""}, fmt.Errorf("edit: %w", closeErr)
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("edit.done",
		"path", abspath,
		"old_len", len(oldString),
		"new_len", len(newString),
		"duration_ms", time.Since(start).Milliseconds())

	return &ToolResult{
		Output:   fmt.Sprintf("replaced %d-byte block with %d-byte block in %s", len(oldString), len(newString), abspath),
		Metadata: map[string]any{"path": abspath, "bytes": len(updated)},
	}, nil
}
