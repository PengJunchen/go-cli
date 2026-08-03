package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// ContextFile represents a loaded context file (AGENTS.md, CLAUDE.md, etc.).
type ContextFile struct {
	Path     string
	Content  string
	Priority int // higher = more important
}

// ContextFileLoader loads context files from the working directory. The files
// field is ordered by ascending priority so that later files override earlier
// ones when merged.
type ContextFileLoader struct {
	files []string
}

// defaultContextFiles are the files probed by a fresh loader, ordered by
// ascending priority (AGENTS.md < CLAUDE.md < SYSTEM.md).
var defaultContextFiles = []string{"AGENTS.md", "CLAUDE.md", "SYSTEM.md"}

// NewContextFileLoader returns a loader configured with the default context
// files (AGENTS.md, CLAUDE.md, SYSTEM.md).
func NewContextFileLoader() *ContextFileLoader {
	files := make([]string, len(defaultContextFiles))
	copy(files, defaultContextFiles)
	return &ContextFileLoader{files: files}
}

// WithFiles replaces the set of context files to probe. The order determines
// priority: earlier files have lower priority.
func (l *ContextFileLoader) WithFiles(files []string) *ContextFileLoader {
	cp := make([]string, len(files))
	copy(cp, files)
	l.files = cp
	return l
}

// Load reads all configured context files that exist under dir. Files that do
// not exist are silently skipped. Each loaded file is assigned a priority equal
// to its index in the files list (higher index = higher priority). It emits a
// config.context.load span with the number of files found.
func (l *ContextFileLoader) Load(ctx context.Context, dir string) ([]ContextFile, error) {
	span, _ := tracing.SpanFromContext(ctx, "config.context.load", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())

	var out []ContextFile
	for i, name := range l.files {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				logger.DebugContext(ctx, "config.context.load.skip",
					"file", name,
					"reason", "not_found",
				)
				continue
			}
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return nil, fmt.Errorf("read context file %s: %w", path, err)
		}
		out = append(out, ContextFile{
			Path:     path,
			Content:  string(data),
			Priority: i,
		})
		logger.DebugContext(ctx, "config.context.load.found",
			"file", name,
			"priority", i,
			"bytes", len(data),
		)
	}

	span.SetAttributes(
		tracing.Attribute{Key: "files_found", Value: len(out)},
		tracing.Attribute{Key: "files_probed", Value: len(l.files)},
	)
	span.SetStatus(tracing.SpanStatusOK, "")
	return out, nil
}

// Merge concatenates the content of files in ascending priority order (lowest
// priority first), separating each file with a blank line. Files with empty
// content are skipped.
func (l *ContextFileLoader) Merge(files []ContextFile) string {
	// Stable sort by priority so lower priority comes first.
	ordered := make([]ContextFile, len(files))
	copy(ordered, files)
	sortContextFilesByPriority(ordered)

	var b strings.Builder
	for i, f := range ordered {
		content := strings.TrimSpace(f.Content)
		if content == "" {
			continue
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(content)
	}
	return b.String()
}

// sortContextFilesByPriority sorts files in place by ascending Priority using
// insertion sort (the slices are typically tiny).
func sortContextFilesByPriority(files []ContextFile) {
	for i := 1; i < len(files); i++ {
		for j := i; j > 0 && files[j-1].Priority > files[j].Priority; j-- {
			files[j-1], files[j] = files[j], files[j-1]
		}
	}
}
