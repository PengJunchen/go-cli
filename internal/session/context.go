package session

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// EstimateTokensPerChar is the heuristic number of tokens attributed to each
// character of entry content. It is intentionally coarse; compaction summaries
// are typically much denser, but no tokenizer is available in this package.
const EstimateTokensPerChar = 4

// ContextManager reconstructs the effective, replayable context for a session
// leaf. It walks from the leaf back to the root, folding Compaction entries
// into a single summary message, and returns an estimate of the token budget.
type ContextManager interface {
	// BuildContext reconstructs the effective context for the branch ending at
	// leafID, ordered root to leaf.
	BuildContext(ctx context.Context, leafID string) (*SessionContext, error)
}

// DefaultContextManager is the default ContextManager. It reads the branch from
// a SessionTree via GetBranch and performs compaction folding on top of the raw
// entries.
type DefaultContextManager struct {
	tree SessionTree
}

var _ ContextManager = (*DefaultContextManager)(nil)

// NewDefaultContextManager returns a ContextManager that reconstructs contexts
// from the given SessionTree. It takes the tree through the SessionTree
// interface so callers may supply any implementation.
func NewDefaultContextManager(tree SessionTree) ContextManager {
	return &DefaultContextManager{tree: tree}
}

// BuildContext reconstructs the effective context for leadID. Entries are read
// via the underlying tree's GetBranch, so unknown leaves surface ErrLeafNotFound.
func (m *DefaultContextManager) BuildContext(ctx context.Context, leafID string) (*SessionContext, error) {
	span, _ := tracing.SpanFromContext(ctx, "context.rebuild", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(tracing.Attribute{Key: "leaf_id", Value: leafID})

	if m.tree == nil {
		span.SetStatus(tracing.SpanStatusError, "nil tree")
		logger.Error("context_rebuild", "op", "context.rebuild", "error_type", "nil_tree", "leaf_id", leafID)
		return nil, errors.New("session: ContextManager has no SessionTree")
	}

	branch, err := m.tree.GetBranch(ctx, leafID)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("context_rebuild", "op", "context.rebuild", "error_type", "get_branch_failed", "leaf_id", leafID, "err", err)
		return nil, err
	}

	// Traversed are the raw entries visited in walk order (leaf to root).
	traversed := make([]SessionEntry, 0, len(branch))
	for i := len(branch) - 1; i >= 0; i-- {
		traversed = append(traversed, *branch[i])
	}

	// Messages are ordered root to leaf, with Compaction entries folded into a
	// single summary message.
	messages := make([]SessionEntry, 0, len(branch))
	var estimatedTokens int
	var last time.Time
	for _, e := range branch {
		if e.Timestamp.After(last) {
			last = e.Timestamp
		}
		if e.Type == EntryTypeCompaction {
			messages = append(messages, SessionEntry{
				ID:        e.ID,
				ParentID:  e.ParentID,
				Type:      EntryTypeCompaction,
				Content:   e.Summary,
				Timestamp: e.Timestamp,
			})
			estimatedTokens += estimateTokens(e.Summary)
			continue
		}
		messages = append(messages, *e)
		estimatedTokens += estimateTokens(e.Content)
	}

	sc := &SessionContext{
		LeafID:          leafID,
		RootID:          branch[0].ID,
		Messages:        messages,
		Traversed:       traversed,
		EntryCount:      len(messages),
		EstimatedTokens: estimatedTokens,
		LastUpdate:      last,
	}
	span.SetAttributes(tracing.Attribute{Key: "entries_traversed", Value: len(traversed)})
	logger.Info("context_rebuild", "op", "context.rebuild", "leaf_id", leafID, "entries_traversed", len(traversed), "entry_count", sc.EntryCount)
	span.SetStatus(tracing.SpanStatusOK, "")
	return sc, nil
}

// estimateTokens returns a coarse token estimate for a piece of content using
// the EstimateTokensPerChar heuristic.
func estimateTokens(content string) int {
	return len(content) / EstimateTokensPerChar
}
