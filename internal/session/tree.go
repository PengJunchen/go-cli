package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// ErrLeafNotFound is returned by MoveTo/GetBranch/BuildContext when the
// referenced leaf id does not exist in the tree.
var ErrLeafNotFound = errors.New("session: leaf entry not found")

// ErrParentNotFound is returned by an Append whose entry references a parent id
// that has not been appended yet.
var ErrParentNotFound = errors.New("session: parent entry not found")

// DefaultSessionTree is an append-only in-memory tree of immutable entries. It
// holds every entry by id and a current-leaf pointer. Branching is expressed by
// entries sharing a common ancestor via ParentID.
type DefaultSessionTree struct {
	mu       sync.RWMutex
	entries  map[string]*SessionEntry
	leafID   string
	branches map[string]BranchMeta

	// branchSummary, when non-nil, is used by MoveTo to append a compact
	// summary entry to the branch being departed on every branch switch.
	branchSummary BranchSummary
	// summarySeq generates unique ids for appended branch-summary entries.
	summarySeq atomic.Uint64
	// gitSwitcher, when non-nil, is used by Branch to create git branches and
	// by MoveTo to checkout git branches on resume.
	gitSwitcher GitBranchSwitcher
	// branchStore, when non-nil, persists branch metadata so it survives
	// process restarts. Set via SetBranchStore.
	branchStore BranchStore
}

var _ SessionTree = (*DefaultSessionTree)(nil)

// NewDefaultSessionTree returns an empty in-memory session tree.
func NewDefaultSessionTree() SessionTree {
	return &DefaultSessionTree{
		entries:  make(map[string]*SessionEntry),
		branches: make(map[string]BranchMeta),
	}
}

// Append adds an immutable entry. A non-empty ParentID must reference an
// already-appended entry. The first appended entry becomes the current leaf.
func (t *DefaultSessionTree) Append(ctx context.Context, entry *SessionEntry) error {
	span, _ := tracing.SpanFromContext(ctx, "session.tree.append", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())

	if entry == nil {
		return errors.New("session: nil entry")
	}
	if entry.ID == "" {
		return errors.New("session: entry id is required")
	}
	if entry.Type == "" {
		return errors.New("session: entry type is required")
	}
	span.SetAttributes(
		tracing.Attribute{Key: "entry_type", Value: string(entry.Type)},
		tracing.Attribute{Key: "entry_id", Value: entry.ID},
	)

	cp := entry.clone()
	t.mu.Lock()
	if _, exists := t.entries[cp.ID]; exists {
		t.mu.Unlock()
		span.SetStatus(tracing.SpanStatusError, "duplicate entry")
		logger.Error("session_tree_append", "op", "session.tree.append", "error_type", "duplicate_entry", "entry_id", cp.ID)
		return fmt.Errorf("session: entry %q already exists", cp.ID)
	}
	if cp.ParentID != "" {
		if _, ok := t.entries[cp.ParentID]; !ok {
			t.mu.Unlock()
			span.SetStatus(tracing.SpanStatusError, "parent not found")
			logger.Error("session_tree_append", "op", "session.tree.append", "error_type", "parent_not_found", "entry_id", cp.ID, "parent_id", cp.ParentID)
			return ErrParentNotFound
		}
	}
	t.entries[cp.ID] = cp
	if t.leafID == "" {
		t.leafID = cp.ID
	}
	t.mu.Unlock()

	logger.Info("session_tree_append", "op", "session.tree.append", "entry_type", string(cp.Type), "entry_id", cp.ID)
	span.SetStatus(tracing.SpanStatusOK, "")
	return nil
}

// MoveTo changes the current leaf pointer to leafID. It returns ErrLeafNotFound
// when the leaf id is unknown. On a genuine branch switch (leafID differs from
// the current leaf), the departed branch's entries are summarized through the
// configured BranchSummary (if any) and the summary is appended as a SessionEntry
// at the end of the departed branch. This is non-destructive: no entries are
// removed, and without a configured BranchSummary behavior is unchanged.
func (t *DefaultSessionTree) MoveTo(ctx context.Context, leafID string) error {
	span, _ := tracing.SpanFromContext(ctx, "session.tree.move", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(tracing.Attribute{Key: "leaf_id", Value: leafID})

	t.mu.Lock()
	if _, ok := t.entries[leafID]; !ok {
		t.mu.Unlock()
		span.SetStatus(tracing.SpanStatusError, "leaf not found")
		logger.Error("session_tree_move", "op", "session.tree.move", "error_type", "leaf_not_found", "leaf_id", leafID)
		return ErrLeafNotFound
	}
	departed := t.leafID
	t.leafID = leafID
	// Snapshot the departed branch and the summary config under lock so the
	// (potentially slow) summarization call happens outside the critical section.
	var departEntries []SessionEntry
	if t.branchSummary != nil && departed != "" && departed != leafID {
		if branch, ok := t.walkBranchLocked(departed); ok {
			departEntries = make([]SessionEntry, 0, len(branch))
			for _, e := range branch {
				departEntries = append(departEntries, *e)
			}
		}
	}
	summarizer := t.branchSummary

	// Look up the git branch associated with the target leaf. A branch's
	// BaseLeafID is the entry the branch was forked from; when we move to
	// that entry, we check out the associated git branch.
	var gitBranch string
	var switcher GitBranchSwitcher
	if t.gitSwitcher != nil {
		switcher = t.gitSwitcher
		for _, meta := range t.branches {
			if meta.BaseLeafID == leafID && meta.GitBranch != "" {
				gitBranch = meta.GitBranch
				break
			}
		}
	}
	t.mu.Unlock()

	// Check out the associated git branch. Failures are logged as warnings
	// but do not fail the session branch switch (graceful degradation).
	if gitBranch != "" && switcher != nil {
		if err := switcher.Checkout(ctx, gitBranch); err != nil {
			logger.Warn("session_tree_move_git_checkout_failed", "op", "session.tree.move", "leaf_id", leafID, "git_branch", gitBranch, "err", err)
		} else {
			logger.Info("session_tree_move_git_checkout", "op", "session.tree.move", "leaf_id", leafID, "git_branch", gitBranch)
		}
	}

	if len(departEntries) > 0 {
		summary, err := summarizer.Summarize(ctx, departEntries)
		if err != nil {
			logger.Warn("session_tree_move", "op", "session.tree.move", "error_type", "branch_summary_failed", "leaf_id", leafID, "departed", departed, "err", err)
		} else if summary != "" {
			entry := &SessionEntry{
				ID:        t.nextSummaryID(),
				ParentID:  departed,
				Type:      EntryTypeSystem,
				Content:   summary,
				Summary:   summary,
				Timestamp: time.Now().UTC(),
				IsSummary: true,
			}
			// Append returns the standard duplicate/parent validation; a fresh
			// generated id cannot collide, and departed is a known leaf.
			_ = t.Append(ctx, entry) //nolint:errcheck // best-effort append of branch summary.
			span.SetAttributes(
				tracing.Attribute{Key: "summary_appended", Value: true},
				tracing.Attribute{Key: "departed_leaf", Value: departed},
			)
		}
	}

	logger.Info("session_tree_move", "op", "session.tree.move", "leaf_id", leafID)
	span.SetStatus(tracing.SpanStatusOK, "")
	return nil
}

// SetBranchSummary configures a BranchSummary used by MoveTo to record a compact
// summary of each branch as it is departed. A nil value disables the behavior.
func (t *DefaultSessionTree) SetBranchSummary(s BranchSummary) {
	t.mu.Lock()
	t.branchSummary = s
	t.mu.Unlock()
	if s != nil {
		slog.Info("session_tree_set_branch_summary", "op", "session.tree.set_branch_summary", "name", s.Name())
	}
}

// SetGitBranchSwitcher wires a GitBranchSwitcher into the tree. When non-nil,
// Branch creates the corresponding git branch (via WithGitBranch) and MoveTo
// checks out the associated git branch when switching to a branch that has one.
func (t *DefaultSessionTree) SetGitBranchSwitcher(g GitBranchSwitcher) {
	t.mu.Lock()
	t.gitSwitcher = g
	t.mu.Unlock()
}

// SetBranchStore wires a BranchStore into the tree. When non-nil, branch
// operations are persisted so they survive process restarts. Existing
// branches are loaded from the store and merged into the in-memory map;
// only branches that don't already exist in the map are added.
func (t *DefaultSessionTree) SetBranchStore(bs BranchStore) {
	t.mu.Lock()
	t.branchStore = bs
	t.mu.Unlock()
	if bs == nil {
		return
	}
	loaded, err := bs.LoadBranches(context.Background())
	if err != nil {
		slog.Warn("session_tree_set_branch_store_load_failed", "err", err)
		return
	}
	t.mu.Lock()
	for _, meta := range loaded {
		if _, exists := t.branches[meta.BranchID]; !exists {
			t.branches[meta.BranchID] = meta
		}
	}
	t.mu.Unlock()
}

// nextSummaryID returns a unique id for a branch-summary entry.
func (t *DefaultSessionTree) nextSummaryID() string {
	return fmt.Sprintf("branch-summary-%d", t.summarySeq.Add(1))
}

// GetBranch returns the entries ordered from root to leafID, or ErrLeafNotFound.
func (t *DefaultSessionTree) GetBranch(ctx context.Context, leafID string) ([]*SessionEntry, error) {
	span, _ := tracing.SpanFromContext(ctx, "session.tree.branch", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(tracing.Attribute{Key: "leaf_id", Value: leafID})

	t.mu.RLock()
	branch, ok := t.walkBranchLocked(leafID)
	t.mu.RUnlock()
	if !ok {
		span.SetStatus(tracing.SpanStatusError, "leaf not found")
		logger.Error("session_tree_branch", "op", "session.tree.branch", "error_type", "leaf_not_found", "leaf_id", leafID)
		return nil, ErrLeafNotFound
	}
	span.SetAttributes(tracing.Attribute{Key: "branch_length", Value: len(branch)})
	logger.Info("session_tree_branch", "op", "session.tree.branch", "leaf_id", leafID, "branch_length", len(branch))
	span.SetStatus(tracing.SpanStatusOK, "")
	return branch, nil
}

// BuildContext reconstructs the effective context for the branch ending at
// leafID, ordered root to leaf.
func (t *DefaultSessionTree) BuildContext(ctx context.Context, leafID string) (*SessionContext, error) {
	span, _ := tracing.SpanFromContext(ctx, "session.tree.build", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(tracing.Attribute{Key: "leaf_id", Value: leafID})

	t.mu.RLock()
	branch, ok := t.walkBranchLocked(leafID)
	if !ok {
		t.mu.RUnlock()
		span.SetStatus(tracing.SpanStatusError, "leaf not found")
		logger.Error("session_tree_build", "op", "session.tree.build", "error_type", "leaf_not_found", "leaf_id", leafID)
		return nil, ErrLeafNotFound
	}
	msgs := make([]SessionEntry, 0, len(branch))
	var last time.Time
	for _, e := range branch {
		msgs = append(msgs, *e)
		if e.Timestamp.After(last) {
			last = e.Timestamp
		}
	}
	t.mu.RUnlock()

	sc := &SessionContext{
		LeafID:     leafID,
		Messages:   msgs,
		EntryCount: len(msgs),
		LastUpdate: last,
	}
	span.SetAttributes(tracing.Attribute{Key: "entry_count", Value: sc.EntryCount})
	logger.Info("session_tree_build", "op", "session.tree.build", "leaf_id", leafID, "entry_count", sc.EntryCount)
	span.SetStatus(tracing.SpanStatusOK, "")
	return sc, nil
}

// CurrentLeaf returns the id of the current leaf, or "" when empty.
func (t *DefaultSessionTree) CurrentLeaf() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.leafID
}

// walkBranchLocked returns entries from root to leaf following ParentID links.
// It must be called with t.mu held (at least a read lock). It returns ok=false
// when leafID (or any ancestor) is unknown.
func (t *DefaultSessionTree) walkBranchLocked(leafID string) ([]*SessionEntry, bool) {
	var reversed []*SessionEntry
	cur := leafID
	for cur != "" {
		e, ok := t.entries[cur]
		if !ok {
			return nil, false
		}
		reversed = append(reversed, e)
		cur = e.ParentID
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed, true
}
