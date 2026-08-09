package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// worktreeSession is the persisted record for a single session worktree. It is
// serialized to sessions.json so that the session→worktree mapping survives
// process restarts.
type worktreeSession struct {
	SessionID string    `json:"sessionID"`
	Path      string    `json:"path"`
	Branch    string    `json:"branch"`
	CreatedAt time.Time `json:"createdAt"`
}

// WorktreeManager manages git worktrees for parallel session isolation.
// Each session gets its own worktree, allowing multiple sessions to operate
// on different branches simultaneously without interference. The session→
// worktree mapping is persisted to <baseDir>/sessions.json so that worktrees
// can be recovered and cleaned up after a crash or restart.
type WorktreeManager struct {
	mu       sync.Mutex
	gitTool  GitTool
	baseDir  string                     // parent directory for worktrees
	sessions map[string]worktreeSession // sessionID -> worktree record
	// cleanupOnce ensures Cleanup executes at most once even when invoked
	// concurrently (e.g. a signal handler racing with normal shutdown). The
	// error from the first (and only) execution is saved in cleanupErr and
	// returned by subsequent calls.
	cleanupOnce sync.Once
	cleanupErr  error
}

// NewWorktreeManager creates a WorktreeManager. baseDir is the parent directory
// where worktrees are created. When empty, the caller should set it before use.
// If a sessions.json file already exists under baseDir, the persisted sessions
// are loaded into memory.
func NewWorktreeManager(gitTool GitTool, baseDir string) *WorktreeManager {
	m := &WorktreeManager{
		gitTool:  gitTool,
		baseDir:  baseDir,
		sessions: make(map[string]worktreeSession),
	}
	m.loadSessions()
	return m
}

// sessionsFile returns the path to the persisted session mapping.
func (m *WorktreeManager) sessionsFile() string {
	return filepath.Join(m.baseDir, "sessions.json")
}

// loadSessions reads sessions.json into m.sessions. A missing or unreadable
// file is treated as an empty mapping (no error), so a fresh baseDir is safe.
func (m *WorktreeManager) loadSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadSessionsLocked()
}

// loadSessionsLocked is the lock-free variant; the caller must hold m.mu.
func (m *WorktreeManager) loadSessionsLocked() {
	data, err := os.ReadFile(m.sessionsFile())
	if err != nil {
		return // file missing or unreadable: start empty
	}
	var records []worktreeSession
	if err := json.Unmarshal(data, &records); err != nil {
		return // corrupt file: start empty rather than failing
	}
	for _, r := range records {
		m.sessions[r.SessionID] = r
	}
}

// saveSessionsLocked writes m.sessions to sessions.json as a sorted JSON array.
// The write is atomic: data is written to a temporary file then renamed into
// place, so a crash mid-write cannot leave a corrupt sessions.json. The caller
// must hold m.mu.
func (m *WorktreeManager) saveSessionsLocked() error {
	records := make([]worktreeSession, 0, len(m.sessions))
	for id, s := range m.sessions {
		s.SessionID = id
		records = append(records, s)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].SessionID < records[j].SessionID
	})
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	tmpFile := m.sessionsFile() + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpFile, m.sessionsFile())
}

// CreateForSession creates a worktree for the given session. The worktree is
// created at <baseDir>/<sessionID> on a new branch named <branchPrefix><sessionID>.
// If a worktree for the session already exists, its path is returned without
// creating a new one. The session mapping is persisted to sessions.json.
// Returns the absolute path of the worktree.
func (m *WorktreeManager) CreateForSession(ctx context.Context, sessionID string, branchPrefix string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.sessions[sessionID]; ok {
		return existing.Path, nil // Already exists
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

	m.sessions[sessionID] = worktreeSession{
		SessionID: sessionID,
		Path:      abs,
		Branch:    branch,
		CreatedAt: time.Now(),
	}
	if err := m.saveSessionsLocked(); err != nil {
		return "", fmt.Errorf("worktree: persist sessions: %w", err)
	}
	return abs, nil
}

// GetForSession returns the worktree path for the given session, or empty
// string if no worktree exists.
func (m *WorktreeManager) GetForSession(sessionID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[sessionID].Path
}

// RemoveForSession removes the worktree for the given session. If no worktree
// exists for the session, it is a no-op. The session is only unregistered after
// a successful removal, so a failed removal leaves the worktree tracked and
// retryable. Errors from the underlying git worktree remove are propagated.
func (m *WorktreeManager) RemoveForSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	sess, ok := m.sessions[sessionID]
	m.mu.Unlock()

	if !ok {
		return nil
	}

	if err := m.gitTool.WorktreeRemove(ctx, sess.Path); err != nil {
		return fmt.Errorf("worktree: remove for session %s: %w", sessionID, err)
	}

	m.mu.Lock()
	delete(m.sessions, sessionID)
	if err := m.saveSessionsLocked(); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("worktree: persist sessions: %w", err)
	}
	m.mu.Unlock()
	return nil
}

// RemoveOrphan removes a worktree at the given path that is not associated with
// any session. It is intended for cleaning up orphaned worktrees discovered by
// ScanOrphans. Errors from the underlying git worktree remove are propagated.
func (m *WorktreeManager) RemoveOrphan(ctx context.Context, path string) error {
	if err := m.gitTool.WorktreeRemove(ctx, path); err != nil {
		return fmt.Errorf("worktree: remove orphan %s: %w", path, err)
	}
	return nil
}

// List returns a copy of all active session worktrees (sessionID -> path).
func (m *WorktreeManager) List() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.sessions))
	for k, v := range m.sessions {
		out[k] = v.Path
	}
	return out
}

// ScanOrphans returns the absolute paths of worktree directories under baseDir
// that are not registered in the session mapping. These are "orphaned"
// worktrees, typically left behind by a crashed process. A missing baseDir
// yields an empty result without error.
func (m *WorktreeManager) ScanOrphans() ([]string, error) {
	m.mu.Lock()
	known := make(map[string]bool, len(m.sessions))
	for _, s := range m.sessions {
		known[s.Path] = true
	}
	m.mu.Unlock()

	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var orphans []string
	for _, e := range entries {
		if !e.IsDir() {
			continue // skip files such as sessions.json
		}
		p := filepath.Join(m.baseDir, e.Name())
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if !known[abs] {
			orphans = append(orphans, abs)
		}
	}
	return orphans, nil
}

// Cleanup removes all session worktrees as well as any orphaned worktrees
// discovered under baseDir. Best-effort: errors are collected but do not stop
// cleanup of remaining worktrees. Returns all encountered errors joined
// together (nil if none).
//
// Cleanup is idempotent: the actual work is performed exactly once via
// sync.Once, so concurrent invocations (e.g. a signal handler racing with
// normal shutdown) do not duplicate removals or produce noisy duplicate-error
// logs. Subsequent calls return the error from the first execution.
func (m *WorktreeManager) Cleanup(ctx context.Context) error {
	m.cleanupOnce.Do(func() {
		m.cleanupErr = m.doCleanup(ctx)
	})
	return m.cleanupErr
}

// doCleanup performs the actual worktree removal. It is called at most once,
// guarded by Cleanup's sync.Once.
func (m *WorktreeManager) doCleanup(ctx context.Context) error {
	var errs []error

	// Remove orphaned worktrees first so they are not missed.
	orphans, err := m.ScanOrphans()
	if err != nil {
		errs = append(errs, fmt.Errorf("worktree: scan orphans: %w", err))
	} else {
		for _, p := range orphans {
			if err := m.RemoveOrphan(ctx, p); err != nil {
				errs = append(errs, err)
			}
		}
	}

	// Remove all registered sessions.
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		if err := m.RemoveForSession(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// StartSignalCleanup launches a goroutine that calls Cleanup when sigCh
// receives a value (e.g. SIGINT/SIGTERM delivered via signal.Notify). The
// goroutine exits without cleaning up when done is closed, which is the normal
// shutdown path; the caller is then responsible for invoking Cleanup. Because
// Cleanup is idempotent (guarded by sync.Once), a concurrent signal during
// shutdown is safe — cleanup runs at most once.
func (m *WorktreeManager) StartSignalCleanup(sigCh chan os.Signal, done chan struct{}) {
	go func() {
		select {
		case <-sigCh:
			_ = m.Cleanup(context.Background())
		case <-done:
		}
	}()
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
