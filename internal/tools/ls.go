package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

const (
	// lsDefaultWorkdir is the base directory relative paths are resolved
	// against when none is configured.
	lsDefaultWorkdir = "."
	// lsDefaultPath is the directory listed when no path is supplied.
	lsDefaultPath = "."
)

// LSToolOption configures an LSTool.
type LSToolOption func(*LSTool)

func withLSCanonicalWorkspaceRoot(root string) LSToolOption {
	return func(tool *LSTool) { tool.canonicalWorkspaceRoot = root }
}

// WithLSWorkdir sets the base directory that relative paths are resolved
// against.
func WithLSWorkdir(dir string) LSToolOption {
	return func(t *LSTool) { t.Workdir = dir }
}

// WithLSPathWhitelist sets the allowed base paths for ls operations.
// When configured, the resolved real path (after symlink resolution) is
// validated against the whitelist, providing defense-in-depth against symlink
// escape and absolute path access outside the workdir. An empty slice allows
// all paths (no restriction beyond traversal checks).
func WithLSPathWhitelist(paths []string) LSToolOption {
	return func(t *LSTool) { t.whitelist = NewPathWhitelist(paths) }
}

// LSTool lists the entries of a directory in pure Go, with optional dotfile
// inclusion, long (ls -l style) formatting and name/time/size sorting. It
// implements the ToolDefinition interface.
type LSTool struct {
	// Workdir is the base directory relative paths are resolved against.
	Workdir string
	// whitelist, when configured, restricts listings to paths within the
	// allowed base directories. Symlink escape is detected and rejected.
	whitelist              PathWhitelist
	canonicalWorkspaceRoot string
}

var _ ToolDefinition = (*LSTool)(nil)

// NewLSTool returns an LSTool with a default workdir of ".".
func NewLSTool(opts ...LSToolOption) *LSTool {
	t := &LSTool{Workdir: lsDefaultWorkdir}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Name returns the tool name.
func (t *LSTool) Name() string { return "ls" }

// Description returns a brief description of the tool.
func (t *LSTool) Description() string {
	return "ls: lists the entries of a directory. Args: path (string, optional), all (bool, optional: include dotfiles), long (bool, optional: ls -l style), sort (string, optional: name|time|size)."
}

// Parameters returns the JSON Schema for ls's optional listing controls.
func (t *LSTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
			"all":  map[string]any{"type": "boolean"},
			"long": map[string]any{"type": "boolean"},
			"sort": map[string]any{"type": "string", "enum": []string{"", "name", "time", "size"}},
		},
		"additionalProperties": false,
	}
}

// lsEntry is a single listing line for sorting purposes.
type lsEntry struct {
	path string
	info fs.FileInfo
}

// Execute lists the directory at path (default "."). With all=true dotfiles
// are included; with long=true entries are rendered ls -l style; sort selects
// name (default), time (modification time) or size ordering.
func (t *LSTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	start := time.Now()

	listPath := lsDefaultPath
	if v := getStringArg(call, "path"); strings.TrimSpace(v) != "" {
		listPath = v
	}

	listPath, err := resolveAndValidatePath("ls", t.Workdir, listPath, t.whitelist)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "path", Value: listPath}, tracing.Attribute{Key: "success", Value: false})
		logger.Error("ls.path_traversal", "tool", "ls", "path", listPath, "err", err)
		return nil, err
	}

	all := getBoolArg(call, "all")
	long := getBoolArg(call, "long")
	sortBy := strings.ToLower(strings.TrimSpace(getStringArg(call, "sort")))
	if sortBy == "" {
		sortBy = "name"
	}
	if sortBy != "name" && sortBy != "time" && sortBy != "size" {
		span.SetAttributes(tracing.Attribute{Key: "path", Value: listPath}, tracing.Attribute{Key: "success", Value: false})
		logger.Error("ls.invalid_sort", "tool", "ls", "sort", sortBy)
		return nil, fmt.Errorf("ls: invalid sort %q (expected name, time or size)", sortBy)
	}

	info, err := os.Stat(listPath)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "path", Value: listPath}, tracing.Attribute{Key: "success", Value: false})
		logger.Error("ls.stat_failed", "tool", "ls", "path", listPath, "err", err)
		return nil, fmt.Errorf("ls: %w", err)
	}
	if !info.IsDir() {
		span.SetAttributes(tracing.Attribute{Key: "path", Value: listPath}, tracing.Attribute{Key: "success", Value: false})
		logger.Error("ls.not_dir", "tool", "ls", "path", listPath)
		return nil, fmt.Errorf("ls: %q is not a directory", listPath)
	}

	less := func(a, b lsEntry) bool {
		switch sortBy {
		case "time":
			if !a.info.ModTime().Equal(b.info.ModTime()) {
				return a.info.ModTime().After(b.info.ModTime())
			}
		case "size":
			if a.info.Size() != b.info.Size() {
				return a.info.Size() < b.info.Size()
			}
		}
		return a.path < b.path
	}
	entries := newBoundedTopK[lsEntry](directoryCaptureMaxEntries, directoryCaptureMaxEntryBytes,
		func(entry lsEntry) string { return entry.path }, less)
	directory, err := os.Open(listPath)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "path", Value: listPath}, tracing.Attribute{Key: "success", Value: false})
		logger.Error("ls.open_failed", "tool", "ls", "path", listPath, "err", err)
		return nil, fmt.Errorf("ls: %w", err)
	}
	truncated := false
	err = scanDirectoryBatches(ctx, directory, directoryReadBatchSize, func(entry fs.DirEntry) error {
		if !all && strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			truncated = true
			return nil
		}
		return entries.Add(lsEntry{path: entry.Name(), info: info})
	})
	if closeErr := directory.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "path", Value: listPath}, tracing.Attribute{Key: "success", Value: false})
		logger.Error("ls.readdir_failed", "tool", "ls", "path", listPath, "err", err)
		return nil, fmt.Errorf("ls: %w", err)
	}

	output := newBoundedDirectoryOutput(directoryCaptureMaxOutputBytes)
	for _, e := range entries.Sorted() {
		var line strings.Builder
		if long {
			line.WriteString(e.info.Mode().String())
			line.WriteString(" ")
			line.WriteString(fmt.Sprintf("%10d", e.info.Size()))
			line.WriteString(" ")
			line.WriteString(e.info.ModTime().Format("Jan _2 15:04"))
			line.WriteString(" ")
			line.WriteString(directoryDisplayName(e.path))
			if e.info.IsDir() {
				line.WriteString("/")
			}
		} else {
			line.WriteString(directoryDisplayName(e.path))
			if e.info.IsDir() {
				line.WriteString("/")
			}
		}
		if !output.AppendLine(line.String()) {
			break
		}
	}
	truncated = truncated || entries.Truncated() || output.Truncated()

	span.SetAttributes(
		tracing.Attribute{Key: "path", Value: listPath},
		tracing.Attribute{Key: "entries", Value: output.Records()},
		tracing.Attribute{Key: "truncated", Value: truncated},
		tracing.Attribute{Key: "success", Value: true},
	)
	logger.Info("ls.done",
		"tool", "ls",
		"path", listPath,
		"entries", output.Records(),
		"sort", sortBy,
		"truncated", truncated,
		"duration_ms", time.Since(start).Milliseconds())

	return &ToolResult{
		Output:   output.String(),
		Metadata: map[string]any{"path": listPath, "entries": output.Records(), "sort": sortBy, "truncated": truncated},
	}, nil
}

// getBoolArg returns the boolean value of Args[key], or false when absent or
// not a bool.
func getBoolArg(call ToolCall, key string) bool {
	if v, ok := call.Args[key].(bool); ok {
		return v
	}
	return false
}
