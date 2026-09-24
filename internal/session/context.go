package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
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

	// Cache for incremental token estimation. When the same leaf is queried
	// again and the branch has only grown, only the delta (new entries) is
	// estimated; the cached total is reused for the already-seen entries.
	cacheMu        sync.Mutex
	cachedLeafID   string
	cachedTokens   int
	cachedEntryIDs []string
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

	// Project the raw branch. Version one compactions use the exact source
	// boundary rather than the physical marker position.
	branchVals := make([]SessionEntry, len(branch))
	for i, e := range branch {
		branchVals[i] = *e
	}
	messages, err := projectSessionEntries(branchVals)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("context_rebuild", "op", "context.rebuild", "error_type", "projection_failed", "leaf_id", leafID, "err", err)
		return nil, err
	}
	slog.Debug("context_rebuild.compaction_point", "message_count", len(messages), "compaction", len(messages) > 0 && messages[0].Type == EntryTypeCompaction)

	messageIDs := make([]string, len(messages))
	for i, entry := range messages {
		messageIDs[i] = entry.ID
	}
	// A changed source boundary changes the projected ID prefix and invalidates
	// the cached token estimate.
	m.cacheMu.Lock()
	cacheValid := m.cachedLeafID == leafID && len(messageIDs) >= len(m.cachedEntryIDs)
	if cacheValid {
		for i := range m.cachedEntryIDs {
			if messageIDs[i] != m.cachedEntryIDs[i] {
				cacheValid = false
				break
			}
		}
	}
	cachedTokens := m.cachedTokens
	cachedMessageCount := len(m.cachedEntryIDs)
	m.cacheMu.Unlock()

	var estimatedTokens int
	if cacheValid {
		estimatedTokens = cachedTokens
	}
	var last time.Time
	for i, entry := range messages {
		if entry.Timestamp.After(last) {
			last = entry.Timestamp
		}
		if cacheValid && i < cachedMessageCount {
			continue
		}
		estimatedTokens += estimateTokensForEntry(entry)
	}

	// Update the cache for the next call.
	m.cacheMu.Lock()
	m.cachedLeafID = leafID
	m.cachedTokens = estimatedTokens
	m.cachedEntryIDs = append(m.cachedEntryIDs[:0], messageIDs...)
	m.cacheMu.Unlock()
	rootID := ""
	if len(messages) > 0 {
		rootID = messages[0].ID
	}

	sc := &SessionContext{
		LeafID:          leafID,
		RootID:          rootID,
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

// EntriesToAgentMessages converts projected SessionEntry values into agent
// history messages. Version one terminal tool receipts are preserved;
// journal-only intent/execution rows and internal checkpoints are excluded.
func EntriesToAgentMessages(entries []SessionEntry) []core.AgentMessage {
	msgs := make([]core.AgentMessage, 0, len(entries))
	for _, e := range entries {
		message, ok := sessionEntryToAgentMessage(e)
		if ok {
			msgs = append(msgs, message)
		}
	}
	return core.CloneAgentMessages(msgs)
}
