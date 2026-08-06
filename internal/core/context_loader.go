package core

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
)

// contextFileNames are the file names searched for in each directory when
// loading project context.
var contextFileNames = []string{"AGENTS.md", "CLAUDE.md"}

// ProjectContextLoader loads project context files (AGENTS.md, CLAUDE.md)
// from the file system. Implementations traverse the directory tree from the
// current working directory upward to the root, collecting context files at
// each level, and also load a global file from the user's home directory.
type ProjectContextLoader interface {
	// Load returns the context files found for the given working directory.
	// Files are ordered so that broader (parent/global) context appears first
	// and more specific (child) context appears last, giving child files
	// priority.
	Load(ctx context.Context, cwd string) ([]ContextFile, error)
}

// DefaultProjectContextLoader is the default ProjectContextLoader
// implementation. It is stateless and safe for concurrent use.
type DefaultProjectContextLoader struct{}

// Compile-time assertion that DefaultProjectContextLoader satisfies
// ProjectContextLoader.
var _ ProjectContextLoader = (*DefaultProjectContextLoader)(nil)

// NewDefaultProjectContextLoader returns a new DefaultProjectContextLoader.
func NewDefaultProjectContextLoader() *DefaultProjectContextLoader {
	return &DefaultProjectContextLoader{}
}

// Load traverses the directory tree from cwd upward to the root, collecting
// AGENTS.md and CLAUDE.md files at each level. It also loads the global
// ~/.go-cli/AGENTS.md file. The result is ordered so that parent/global files
// appear first and child (closer to cwd) files appear last, giving more
// specific context priority. Duplicate paths are deduplicated.
func (l *DefaultProjectContextLoader) Load(_ context.Context, cwd string) ([]ContextFile, error) {
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}

	// Collect candidate paths in reverse priority order: global first, then
	// parent directories upward-to-downward, then cwd last. This produces a
	// list where child-directory files naturally appear after parent files.
	seen := make(map[string]bool)
	var files []ContextFile

	// 1. Global context file: ~/.go-cli/AGENTS.md.
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		globalPath := filepath.Join(home, ".go-cli", "AGENTS.md")
		if cf, ok := loadContextFile(globalPath); ok && !seen[globalPath] {
			seen[globalPath] = true
			files = append(files, cf)
		}
	}

	// 2. Traverse upward from cwd to root, collecting paths. We build the
	// list in root-to-cwd order so that parent files come first.
	var dirPaths []string
	dir := absCwd
	for {
		dirPaths = append(dirPaths, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Reverse so root is first and cwd is last.
	for i, j := 0, len(dirPaths)-1; i < j; i, j = i+1, j-1 {
		dirPaths[i], dirPaths[j] = dirPaths[j], dirPaths[i]
	}

	// 3. Load context files from each directory in root-to-cwd order.
	for _, d := range dirPaths {
		for _, name := range contextFileNames {
			p := filepath.Join(d, name)
			if seen[p] {
				continue
			}
			if cf, ok := loadContextFile(p); ok {
				seen[p] = true
				files = append(files, cf)
			}
		}
	}

	return files, nil
}

// loadContextFile reads a file at the given path and returns a ContextFile if
// it exists and is readable. A missing file returns (ContextFile{}, false)
// without error.
func loadContextFile(path string) (ContextFile, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ContextFile{}, false
	}
	slog.Debug("context_loader.loaded", "path", path, "bytes", len(data))
	return ContextFile{Path: path, Content: string(data)}, true
}
