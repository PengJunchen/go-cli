package session

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// defaultEstimator is the Unicode-aware estimator used for session token
// estimates. It is stateless, so a single shared instance suffices.
var defaultEstimator = compaction.NewHeuristicTokenEstimator()

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

	// Find the last compaction point; entries before it are replaced by the
	// compaction summary. findLastCompactionPoint operates on []SessionEntry,
	// so dereference the pointer slice once.
	branchVals := make([]SessionEntry, len(branch))
	for i, e := range branch {
		branchVals[i] = *e
	}
	startIdx := findLastCompactionPoint(branchVals)
	slog.Debug("context_rebuild.compaction_point", "start_idx", startIdx, "compaction", startIdx > 0)

	// Messages are ordered root to leaf, with Compaction entries folded into a
	// single summary message.
	messages := make([]SessionEntry, 0, len(branch)-startIdx)
	var estimatedTokens int
	var last time.Time
	for _, e := range branch[startIdx:] {
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
		if !e.SurfaceVisible() {
			continue
		}
		messages = append(messages, *e)
		estimatedTokens += estimateTokensForEntry(*e)
	}

	sc := &SessionContext{
		LeafID:          leafID,
		RootID:          branch[startIdx].ID,
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

// estimateTokens returns a Unicode-aware token estimate for a piece of content
// using the compaction package's HeuristicTokenEstimator, which weights CJK
// runes higher than ASCII to avoid underestimating non-English text.
func estimateTokens(content string) int {
	n, _ := defaultEstimator.Estimate(content)
	return n
}

// estimateTokensForEntry estimates tokens for an entry's Content plus its
// ContentBlocks. Text blocks add their own text estimate; image_url blocks add
// a fixed 85 tokens; other block types fall back to estimating by their text
// length.
func estimateTokensForEntry(e SessionEntry) int {
	tokens := estimateTokens(e.Content)
	for _, block := range e.ContentBlocks {
		switch block.Type {
		case "text":
			tokens += estimateTokens(block.Text)
		case "image_url":
			tokens += 85
		default:
			// Estimate by JSON length for other block types.
			tokens += estimateTokens(block.Text)
		}
	}
	return tokens
}

// EntriesToAgentMessages converts a slice of SessionEntry values into
// core.AgentMessage values suitable for restoring agent history. Tool and
// compaction entries are skipped because they are not part of the replayable
// conversation history.
func EntriesToAgentMessages(entries []SessionEntry) []core.AgentMessage {
	msgs := make([]core.AgentMessage, 0, len(entries))
	for _, e := range entries {
		var role string
		switch e.Type {
		case EntryTypeUser:
			role = "user"
		case EntryTypeAssistant:
			role = "assistant"
		case EntryTypeSystem:
			role = "system"
		default:
			continue // skip tool/compaction entries
		}
		msgs = append(msgs, core.AgentMessage{
			Role:          role,
			Content:       e.Content,
			ContentBlocks: e.ContentBlocks,
			ToolCalls:     e.ToolCalls,
			ToolCallID:    e.ToolCallID,
			ToolName:      e.ToolName,
		})
	}
	return msgs
}
