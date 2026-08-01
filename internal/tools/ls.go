package tools

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
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

// LSTool lists the entries of a directory in pure Go, with optional dotfile
// inclusion, long (ls -l style) formatting and name/time/size sorting. It
// implements the ToolDefinition interface.
type LSTool struct {
	// Workdir is the base directory relative paths are resolved against.
	Workdir string
}

var _ ToolDefinition = (*LSTool)(nil)

// NewLSTool returns an LSTool with a default workdir of ".".
func NewLSTool() *LSTool {
	return &LSTool{Workdir: lsDefaultWorkdir}
}

// Name returns the tool name.
func (t *LSTool) Name() string { return "ls" }

// Description returns a brief description of the tool.
func (t *LSTool) Description() string {
	return "ls: lists the entries of a directory. Args: path (string, optional), all (bool, optional: include dotfiles), long (bool, optional: ls -l style), sort (string, optional: name|time|size)."
}

// lsEntry is a single listing line for sorting purposes.
type lsEntry struct {
	path  string
	info  fs.FileInfo
	depth int
}

// Execute lists the directory at path (default "."). With all=true dotfiles
// are included; with long=true entries are rendered ls -l style; sort selects
// name (default), time (modification time) or size ordering.
func (t *LSTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tools.ls", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	start := time.Now()

	listPath := lsDefaultPath
	if v := getStringArg(call, "path"); strings.TrimSpace(v) != "" {
		listPath = v
	}
	if !filepath.IsAbs(listPath) {
		listPath = filepath.Join(t.Workdir, listPath)
	}
	listPath = filepath.Clean(listPath)

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

	var entries []lsEntry
	err = filepath.WalkDir(listPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries deterministically
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if p == listPath {
			return nil // skip the root dir itself
		}
		// Only list the immediate children (depth 1), not recursive files.
		rel, rerr := filepath.Rel(listPath, p)
		if rerr != nil {
			rel = p
		}
		if strings.Contains(filepath.ToSlash(rel), "/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !all && strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		fi, ferr := d.Info()
		if ferr != nil {
			return nil
		}
		entries = append(entries, lsEntry{path: rel, info: fi, depth: 1})
		return nil
	})
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "path", Value: listPath}, tracing.Attribute{Key: "success", Value: false})
		logger.Error("ls.walk_failed", "tool", "ls", "path", listPath, "err", err)
		return nil, fmt.Errorf("ls: %w", err)
	}

	switch sortBy {
	case "time":
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].info.ModTime().After(entries[j].info.ModTime()) })
	case "size":
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].info.Size() < entries[j].info.Size() })
	default:
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	}

	var sb strings.Builder
	for _, e := range entries {
		if long {
			sb.WriteString(e.info.Mode().String())
			sb.WriteString(" ")
			sb.WriteString(fmt.Sprintf("%10d", e.info.Size()))
			sb.WriteString(" ")
			sb.WriteString(e.info.ModTime().Format("Jan _2 15:04"))
			sb.WriteString(" ")
			sb.WriteString(e.path)
			if e.info.IsDir() {
				sb.WriteString("/")
			}
			sb.WriteString("\n")
		} else {
			sb.WriteString(e.path)
			if e.info.IsDir() {
				sb.WriteString("/")
			}
			sb.WriteString("\n")
		}
	}

	output := strings.TrimSuffix(sb.String(), "\n")

	span.SetAttributes(
		tracing.Attribute{Key: "path", Value: listPath},
		tracing.Attribute{Key: "entries", Value: len(entries)},
		tracing.Attribute{Key: "success", Value: true},
	)
	logger.Info("ls.done",
		"tool", "ls",
		"path", listPath,
		"entries", len(entries),
		"sort", sortBy,
		"duration_ms", time.Since(start).Milliseconds())

	return &ToolResult{
		Output:   output,
		Metadata: map[string]any{"path": listPath, "entries": len(entries), "sort": sortBy},
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
