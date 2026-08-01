package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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
}

var _ SessionTree = (*DefaultSessionTree)(nil)

// NewDefaultSessionTree returns an empty in-memory session tree.
func NewDefaultSessionTree() *DefaultSessionTree {
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
// when the leaf id is unknown.
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
	t.leafID = leafID
	t.mu.Unlock()

	logger.Info("session_tree_move", "op", "session.tree.move", "leaf_id", leafID)
	span.SetStatus(tracing.SpanStatusOK, "")
	return nil
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
