package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// WorktreeManager manages git worktrees for parallel session isolation.
// Each session gets its own worktree, allowing multiple sessions to operate
// on different branches simultaneously without interference.
type WorktreeManager struct {
	mu       sync.Mutex
	gitTool  GitTool
	baseDir  string            // parent directory for worktrees
	sessions map[string]string // sessionID -> worktree path
}

// NewWorktreeManager creates a WorktreeManager. baseDir is the parent directory
// where worktrees are created. When empty, the caller should set it before use.
func NewWorktreeManager(gitTool GitTool, baseDir string) *WorktreeManager {
	return &WorktreeManager{
		gitTool:  gitTool,
		baseDir:  baseDir,
		sessions: make(map[string]string),
	}
}

// CreateForSession creates a worktree for the given session. The worktree is
// created at <baseDir>/<sessionID> on a new branch named <branchPrefix><sessionID>.
// If a worktree for the session already exists, its path is returned without
// creating a new one. Returns the absolute path of the worktree.
func (m *WorktreeManager) CreateForSession(ctx context.Context, sessionID string, branchPrefix string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.sessions[sessionID]; ok {
		return existing, nil // Already exists
	}

	path := filepath.Join(m.baseDir, sanitizeSessionID(sessionID))
	branch := fmt.Sprintf("%s%s", branchPrefix, sessionID)

	if err := m.gitTool.WorktreeAdd(ctx, path, branch); err != nil {
		return "", fmt.Errorf("worktree: create for session %s: %w", sessionID, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}

	m.sessions[sessionID] = abs
	return abs, nil
}

// GetForSession returns the worktree path for the given session, or empty
// string if no worktree exists.
func (m *WorktreeManager) GetForSession(sessionID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[sessionID]
}

// RemoveForSession removes the worktree for the given session. If no worktree
// exists for the session, it is a no-op.
func (m *WorktreeManager) RemoveForSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	path, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	if !ok {
		return nil
	}

	if err := m.gitTool.WorktreeRemove(ctx, path); err != nil {
		slog.Warn("worktree_remove_failed", "session_id", sessionID, "path", path, "err", err)
	}
	return nil
}

// List returns a copy of all active session worktrees (sessionID -> path).
func (m *WorktreeManager) List() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.sessions))
	for k, v := range m.sessions {
		out[k] = v
	}
	return out
}

// Cleanup removes all session worktrees. Best-effort: errors are collected
// but do not stop cleanup of remaining worktrees. Returns the first error
// encountered, if any.
func (m *WorktreeManager) Cleanup(ctx context.Context) error {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	var firstErr error
	for _, id := range ids {
		if err := m.RemoveForSession(ctx, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// EnsureBaseDir creates the base directory if it does not exist.
func (m *WorktreeManager) EnsureBaseDir() error {
	return os.MkdirAll(m.baseDir, 0o755)
}

// sanitizeSessionID replaces any character that is not alphanumeric, dash, or
// underscore with an underscore. This prevents path traversal or separator
// injection when the session ID is used in filepath.Join.
func sanitizeSessionID(id string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, id)
}
