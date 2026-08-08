package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// branchConfig carries optional parameters for a Branch operation.
type branchConfig struct {
	branchID  string
	gitBranch string
}

// BranchOption customizes a Branch operation, e.g. choosing the logical id of
// the new branch. The zero value (no options) reuses fromID as the branch id.
type BranchOption func(*branchConfig)

// WithBranchID sets an explicit logical id for the new branch. When unset, the
// Branch operation reuses fromID as the branch id.
func WithBranchID(id string) BranchOption {
	return func(c *branchConfig) { c.branchID = id }
}

// WithGitBranch associates a git branch name with the new session branch. When
// set and a GitTool is wired into the tree, Branch creates the corresponding
// git branch and MoveTo checks it out on resume.
func WithGitBranch(gitBranch string) BranchOption {
	return func(c *branchConfig) { c.gitBranch = gitBranch }
}

// BranchMeta records the provenance of a branch created by Branch.
type BranchMeta struct {
	// BranchID is the logical id of the branch.
	BranchID string `json:"branch_id"`
	// ParentID is the leaf at which the branch was forked (== BaseLeafID).
	ParentID string `json:"parent_id"`
	// CreatedAt is when the branch was created.
	CreatedAt time.Time `json:"created_at"`
	// BaseLeafID is the entry the branch was based on (zero-copy re-target).
	BaseLeafID string `json:"base_leaf_id"`
	// GitBranch, when non-empty, is the git branch associated with this session
	// branch. MoveTo calls git checkout when switching to a branch with this
	// field set.
	GitBranch string `json:"git_branch,omitempty"`
}

// GitBranchSwitcher is the minimal interface the session tree uses to
// create and switch git branches. It is satisfied by tools.GitTool.
type GitBranchSwitcher interface {
	CreateBranch(ctx context.Context, name string, base string) error
	Checkout(ctx context.Context, branch string) error
}

// BranchStore persists branch metadata so it survives process restarts.
type BranchStore interface {
	AppendBranch(ctx context.Context, meta BranchMeta) error
	LoadBranches(ctx context.Context) ([]BranchMeta, error)
}

// JSONLBranchStore is a file-backed BranchStore. Each BranchMeta is persisted
// as one JSON object per line (JSONL) using append-only writes, and existing
// branches are lazily loaded on first use.
type JSONLBranchStore struct {
	mu      sync.Mutex
	path    string
	file    *os.File
	loaded  bool
	entries []BranchMeta
}

// NewJSONLBranchStore returns a file-backed branch store for the given path.
// The file is not touched until AppendBranch or LoadBranches is first called.
func NewJSONLBranchStore(path string) *JSONLBranchStore {
	return &JSONLBranchStore{path: path}
}

// ensureLoaded opens the backing file and loads existing branches on first
// use. It must be called with s.mu held.
func (s *JSONLBranchStore) ensureLoaded() error {
	if s.loaded {
		return nil
	}
	if err := s.loadLocked(); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("session: open branch store file: %w", err)
	}
	s.file = f
	s.loaded = true
	return nil
}

// loadLocked reads existing JSONL lines into memory. It must be called with
// s.mu held.
func (s *JSONLBranchStore) loadLocked() error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // brand-new store; nothing to load
		}
		return fmt.Errorf("session: open branch store file for reading: %w", err)
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // read-only best-effort close.

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), jsonlScannerMaxBuffer)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var meta BranchMeta
		if err := json.Unmarshal(line, &meta); err != nil {
			continue // skip corrupt lines, keep the rest.
		}
		s.entries = append(s.entries, meta)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("session: read branch store file: %w", err)
	}
	return nil
}

// AppendBranch persists the branch metadata as a new JSONL line.
func (s *JSONLBranchStore) AppendBranch(ctx context.Context, meta BranchMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoaded(); err != nil {
		return err
	}
	// After Close, loaded is still true but file is nil.
	if s.file == nil {
		return fmt.Errorf("session: branch store is closed")
	}
	if err := json.NewEncoder(s.file).Encode(meta); err != nil {
		return fmt.Errorf("session: write branch entry: %w", err)
	}
	s.entries = append(s.entries, meta)
	return nil
}

// LoadBranches returns all previously persisted branch metadata.
func (s *JSONLBranchStore) LoadBranches(_ context.Context) ([]BranchMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoaded(); err != nil {
		return nil, err
	}
	out := make([]BranchMeta, len(s.entries))
	copy(out, s.entries)
	return out, nil
}

// Close closes the backing file. It is safe to call multiple times.
func (s *JSONLBranchStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// keepBranchID is used as the map key for recent BranchMeta records.
// keepBranchID returns branchID when non-empty, otherwise falls back to fromID.
func keepBranchID(branchID, fromID string) string {
	if branchID != "" {
		return branchID
	}
	return fromID
}

// Branch zero-copy points the current leaf at the existing entry fromID,
// establishing a new branch without copying or appending any entries. It
// records the branch provenance in the tree's recent-branch metadata store.
// When WithGitBranch is used and a GitBranchSwitcher is wired, it also creates
// the corresponding git branch.
func (t *DefaultSessionTree) Branch(ctx context.Context, fromID string, opts ...BranchOption) error {
	span, _ := tracing.SpanFromContext(ctx, "session.branch", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(tracing.Attribute{Key: "from_id", Value: fromID})

	cfg := &branchConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	branchID := keepBranchID(cfg.branchID, fromID)
	span.SetAttributes(
		tracing.Attribute{Key: "to_leaf_id", Value: fromID},
		tracing.Attribute{Key: "branch_id", Value: branchID},
		tracing.Attribute{Key: "git_branch", Value: cfg.gitBranch},
	)

	t.mu.Lock()
	if _, ok := t.entries[fromID]; !ok {
		t.mu.Unlock()
		span.SetStatus(tracing.SpanStatusError, "from id not found")
		logger.Error("session_branch", "op", "session.branch", "error_type", "from_not_found", "from_id", fromID)
		return ErrLeafNotFound
	}
	t.leafID = fromID
	meta := BranchMeta{
		BranchID:   branchID,
		ParentID:   fromID,
		CreatedAt:  time.Now().UTC(),
		BaseLeafID: fromID,
		GitBranch:  cfg.gitBranch,
	}
	t.branches[branchID] = meta
	gitSwitcher := t.gitSwitcher
	branchStore := t.branchStore
	t.mu.Unlock()

	// Persist the branch metadata so it survives process restarts. I/O is
	// done outside the lock; failures are logged as warnings.
	if branchStore != nil {
		if err := branchStore.AppendBranch(ctx, meta); err != nil {
			logger.Warn("session_branch_persist_failed", "op", "session.branch", "branch_id", branchID, "err", err)
		}
	}

	// Create the git branch when requested and a switcher is wired. Failures
	// are logged as warnings but do not fail the session branch (graceful
	// degradation).
	if cfg.gitBranch != "" && gitSwitcher != nil {
		if err := gitSwitcher.CreateBranch(ctx, cfg.gitBranch, ""); err != nil {
			logger.Warn("session_branch_git_create_failed", "op", "session.branch", "git_branch", cfg.gitBranch, "err", err)
		} else {
			logger.Info("session_branch_git_created", "op", "session.branch", "git_branch", cfg.gitBranch)
		}
	}

	logger.Info("session_branch", "op", "session.branch", "from_id", fromID, "to_leaf_id", fromID, "branch_id", branchID)
	span.SetStatus(tracing.SpanStatusOK, "")
	return nil
}

// BranchMetaFor returns the branch metadata recorded for branchID, if any.
func (t *DefaultSessionTree) BranchMetaFor(branchID string) (BranchMeta, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	meta, ok := t.branches[branchID]
	return meta, ok
}

// ListBranches returns metadata for all branches recorded in the tree.
func (t *DefaultSessionTree) ListBranches() []BranchMeta {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]BranchMeta, 0, len(t.branches))
	for _, meta := range t.branches {
		out = append(out, meta)
	}
	return out
}

// Clone deep-copies the entries of the source branch (root to fromBranchID)
// into a new branch identified by newBranchID. The new entries get ids of the
// form "{newBranchID}-{originalID}" and their ParentIDs are remapped to the new
// id space. After cloning, the new branch's leaf becomes the current leaf and
// branch metadata is recorded linking the new branch to its source.
func (t *DefaultSessionTree) Clone(ctx context.Context, fromBranchID, newBranchID string) error {
	span, _ := tracing.SpanFromContext(ctx, "session.branch.clone", tracing.SpanKindInternal)
	defer span.End()

	// Get source branch entries (root to leaf).
	branch, err := t.GetBranch(ctx, fromBranchID)
	if err != nil {
		return fmt.Errorf("session: clone: get source branch: %w", err)
	}
	if len(branch) == 0 {
		return fmt.Errorf("session: clone: source branch %q is empty", fromBranchID)
	}

	// Build the old->new id mapping so ParentIDs can be remapped.
	idMap := make(map[string]string, len(branch))
	for _, e := range branch {
		idMap[e.ID] = newBranchID + "-" + e.ID
	}

	// Build the old->new ToolCallID mapping so tool-call references can be
	// remapped, avoiding ID collisions between the original and cloned branches.
	toolCallIDMap := make(map[string]string)
	seq := 0
	for _, e := range branch {
		for _, tc := range e.ToolCalls {
			toolCallIDMap[tc.ID] = fmt.Sprintf("%s-cloned-%d", tc.ID, seq)
			seq++
		}
	}

	for _, e := range branch {
		newEntry := e.clone()
		newEntry.ID = idMap[e.ID]
		newEntry.ParentID = idMap[e.ParentID] // maps to new ID, or "" for the root
		// Remap ToolCall IDs to avoid collisions with the source branch.
		for i := range newEntry.ToolCalls {
			if newID, ok := toolCallIDMap[newEntry.ToolCalls[i].ID]; ok {
				newEntry.ToolCalls[i].ID = newID
			}
		}
		// Remap the tool-result's ToolCallID to the cloned tool-call's new ID.
		if newEntry.ToolCallID != "" {
			if newID, ok := toolCallIDMap[newEntry.ToolCallID]; ok {
				newEntry.ToolCallID = newID
			}
		}
		if err := t.Append(ctx, newEntry); err != nil {
			return fmt.Errorf("session: clone: append entry %q: %w", newEntry.ID, err)
		}
	}

	// Set the current leaf to the new branch's last entry.
	lastEntry := branch[len(branch)-1]
	newLeafID := idMap[lastEntry.ID]
	if err := t.MoveTo(ctx, newLeafID); err != nil {
		return fmt.Errorf("session: clone: move to new leaf: %w", err)
	}

	// Record branch metadata linking the clone to its source.
	cloneMeta := BranchMeta{
		BranchID:   newBranchID,
		ParentID:   fromBranchID,
		CreatedAt:  time.Now().UTC(),
		BaseLeafID: newLeafID,
	}
	t.mu.Lock()
	t.branches[newBranchID] = cloneMeta
	branchStore := t.branchStore
	t.mu.Unlock()

	// Persist the clone's branch metadata. I/O is done outside the lock.
	if branchStore != nil {
		if err := branchStore.AppendBranch(ctx, cloneMeta); err != nil {
			slog.Warn("session_clone_persist_failed", "op", "session.branch.clone", "branch_id", newBranchID, "err", err)
		}
	}

	span.SetAttributes(
		tracing.Attribute{Key: "from_branch", Value: fromBranchID},
		tracing.Attribute{Key: "new_branch", Value: newBranchID},
	)
	return nil
}

// EntryCount returns the number of entries currently stored in the tree. It is
// unchanged by Branch, which is how zero-copy semantics are verified.
func (t *DefaultSessionTree) EntryCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.entries)
}
