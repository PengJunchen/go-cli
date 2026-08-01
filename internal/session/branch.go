package session

import (
	"context"
	"log/slog"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// branchConfig carries optional parameters for a Branch operation.
type branchConfig struct {
	branchID string
}

// BranchOption customizes a Branch operation, e.g. choosing the logical id of
// the new branch. The zero value (no options) reuses fromID as the branch id.
type BranchOption func(*branchConfig)

// WithBranchID sets an explicit logical id for the new branch. When unset, the
// Branch operation reuses fromID as the branch id.
func WithBranchID(id string) BranchOption {
	return func(c *branchConfig) { c.branchID = id }
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
}

// hashableBranchID is used as the map key for recent BranchMeta records.
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
	}
	t.branches[branchID] = meta
	t.mu.Unlock()

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

// EntryCount returns the number of entries currently stored in the tree. It is
// unchanged by Branch, which is how zero-copy semantics are verified.
func (t *DefaultSessionTree) EntryCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.entries)
}
