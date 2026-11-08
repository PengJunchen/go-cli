package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// EditFileToolOption configures an EditFileTool.
type EditFileToolOption func(*EditFileTool)

// EditMatchError reports that an edit did not find exactly one target match.
// The matching attempt does not modify the file.
type EditMatchError struct {
	path         string
	count        int
	sourceDigest [32]byte
}

// MatchFailureKind identifies a zero-match or multiple-match failure.
func (e *EditMatchError) MatchFailureKind() string {
	if e == nil || e.path == "" {
		return ""
	}
	if e.count == 0 {
		return "no_match"
	}
	if e.count > 1 {
		return "multiple_match"
	}
	return ""
}

func (e *EditMatchError) Error() string {
	const recovery = "the file was not changed by this edit. Read the latest relevant file content and use an exact contiguous old_string, preserving whitespace and newlines; do not retry the same arguments"
	switch e.MatchFailureKind() {
	case "no_match":
		return fmt.Sprintf("edit: old_string not found in %s; %s", e.path, recovery)
	case "multiple_match":
		return fmt.Sprintf("edit: old_string matches %d times in %s, expected exactly once; %s. Include enough surrounding context to make the match unique", e.count, e.path, recovery)
	default:
		return "edit: invalid match failure"
	}
}

// WithEditPathWhitelist sets the allowed base paths for edit operations.
// When configured, both the resolved real path (after symlink resolution) and
// the target file itself (via Lstat) are validated against the whitelist.
// An empty slice allows all paths (no restriction).
func WithEditPathWhitelist(paths []string) EditFileToolOption {
	return func(t *EditFileTool) { t.whitelist = NewPathWhitelist(paths) }
}

// WithEditDiffGenerator sets the DiffGenerator used to produce a change preview
// before applying an edit. When nil (the default) no diff is generated.
func WithEditDiffGenerator(dg DiffGenerator) EditFileToolOption {
	return func(t *EditFileTool) { t.diffGenerator = dg }
}

// WithEditFileTracker sets the FileTracker used to create backup checkpoints
// before editing files. When nil (the default) no backup is created.
func WithEditFileTracker(ft *FileTracker) EditFileToolOption {
	return func(t *EditFileTool) { t.fileTracker = ft }
}

// EditFileTool replaces an old_string block in a file with new_string, in the
// style of apply_patch. It implements the ToolDefinition interface.
type EditFileTool struct {
	// Workdir is the base directory relative paths are resolved against.
	Workdir string
	// whitelist, when configured, restricts edits to paths within the
	// allowed base directories. Symlink escape is detected and rejected.
	whitelist PathWhitelist
	// diffGenerator, when set, produces a diff preview of the edit. It is
	// included in the ToolResult metadata under "diff".
	diffGenerator DiffGenerator
	// fileTracker, when set, creates backup checkpoints before editing files.
	fileTracker *FileTracker
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

// Parameters returns the JSON Schema describing the required edit arguments.
func (t *EditFileTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path":  map[string]any{"type": "string"},
			"old_string": map[string]any{"type": "string"},
			"new_string": map[string]any{"type": "string"},
		},
		"required": []any{"file_path", "old_string", "new_string"},
	}
}

// PromptGuidelines returns usage hints for the edit tool.
func (t *EditFileTool) PromptGuidelines() []string {
	return []string{"Use edit to make targeted changes to existing files"}
}

// Execute edits the file at file_path by locating a single occurrence of
// old_string and replacing it with new_string. The old_string must match
// exactly once; zero or multiple occurrences produce an error. An empty
// new_string removes the matched block.
func (t *EditFileTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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

	newString, ok := call.Args["new_string"].(string)
	if !ok {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.invalid_new_string", "tool", "edit")
		return nil, errors.New("edit: missing or non-string argument 'new_string'")
	}

	abspath, err := resolveAndValidatePath("edit", t.Workdir, path, t.whitelist)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.path_traversal", "path", path, "err", err)
		return nil, err
	}
	if err := rejectUnchangedPreparedEditRetry(ctx, abspath, oldString); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.recovery_retry_blocked", "path", abspath)
		return nil, err
	}

	reportTargetError := func(event string, targetErr error) {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		var conflict *EditConflictError
		if errors.As(targetErr, &conflict) {
			logger.Error("edit.conflict", "path", abspath, "reason", conflict.Reason, "success", false)
		} else {
			logger.Error(event, "path", abspath, "err", targetErr)
		}
	}

	target, err := captureEditTarget(abspath, t.whitelist)
	if err != nil {
		reportTargetError("edit.read_failed", err)
		return nil, fmt.Errorf("edit: %w", err)
	}
	defer func() { _ = target.close() }()
	content, mode := target.snapshot()

	data := string(content)
	count := strings.Count(data, oldString)
	switch count {
	case 0:
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.no_match", "path", abspath)
		return nil, bindPreparedEditMatch(ctx, &EditMatchError{path: abspath, count: count, sourceDigest: sha256.Sum256(content)})
	case 1:
		// OK: exactly one occurrence.
	default:
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("edit.multiple_match", "path", abspath, "count", count)
		return nil, bindPreparedEditMatch(ctx, &EditMatchError{path: abspath, count: count, sourceDigest: sha256.Sum256(content)})
	}

	updated := strings.Replace(data, oldString, newString, 1)

	// When a diff generator is configured, produce a change preview that the
	// approval flow can surface. Ordinary preview errors remain best effort,
	// but unresolved helper execution must stop the mutation.
	var diffPreview string
	if t.diffGenerator != nil {
		if d, derr := t.diffGenerator.Generate(ctx, data, updated, path); derr == nil {
			diffPreview = d
		} else if unresolvedExecutionError(derr) {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			logger.Error("edit.diff_unresolved", "path", abspath, "err", derr)
			return nil, fmt.Errorf("edit: diff execution unresolved: %w", derr)
		}
	}

	if err := ctx.Err(); err != nil {
		reportTargetError("edit.canceled", err)
		return nil, fmt.Errorf("edit: %w", err)
	}
	if err := target.revalidate(); err != nil {
		reportTargetError("edit.revalidate_failed", err)
		return nil, fmt.Errorf("edit: %w", err)
	}

	// When a file tracker is configured, create a backup checkpoint before
	// applying the edit. Errors are logged but never block the edit.
	if t.fileTracker != nil {
		if _, berr := t.fileTracker.backupExistingSnapshot(abspath, content, mode); berr != nil {
			logger.Warn("edit.backup_failed", "path", abspath, "err", berr)
		}
	}

	// Create and commit the replacement relative to the captured parent handle.
	if werr := target.write(ctx, []byte(updated)); werr != nil {
		reportTargetError("edit.write_failed", werr)
		return nil, fmt.Errorf("edit: %w", werr)
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
