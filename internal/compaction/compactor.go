// Package compaction implements context-window compaction strategies for the
// agent loop. When a conversation grows beyond a token budget, a Compactor
// rewrites the TurnItem list so the next model call fits inside maxTokens.
//
// Three strategies are provided, ordered from cheapest to most expensive:
//
//   - MicroCompactor: zero-LLM compaction. It replaces old tool results with a
//     short placeholder and keeps user/assistant messages intact.
//   - SummaryCompactor: LLM-driven compaction. It semantically cuts at message
//     boundaries, aggregates file-operation history, and replaces the oldest
//     region with a single summary entry.
//   - TruncatingCompactor: fallback. It keeps system entries and drops the
//     oldest non-system entries until the list fits the budget.
//
// The Compactor interface is the single extension point; a UnifiedCompactor
// (a later task) routes between these strategies. Compaction decisions emit
// tracing spans and never depend on a concrete LLM provider, so the package can
// be tested with fakes.
package compaction

import (
	"context"
	"errors"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// Conversation roles assigned to TurnItem entries. These are deliberately
// inline strings so the package stays decoupled from any LLM schema.
const (
	// RoleSystem marks system-prompt or meta entries that must never be dropped.
	RoleSystem = "system"
	// RoleUser marks user-authored messages.
	RoleUser = "user"
	// RoleAssistant marks assistant replies.
	RoleAssistant = "assistant"
	// RoleTool marks tool-call results.
	RoleTool = "tool"
)

// TurnItem is a single, JSON-friendly unit of conversation that can be
// compacted. A turned item may be a message (Content) or a tool result
// (ToolResult), identified by Role.
type TurnItem struct {
	ID           string `json:"id"`
	ParentID     string `json:"parent_id,omitempty"`
	Role         string `json:"role"`
	Content      string `json:"content,omitempty"`
	ToolName     string `json:"tool_name,omitempty"`
	ToolResult   string `json:"tool_result,omitempty"`
	IsCompaction bool   `json:"is_compaction,omitempty"`
	// EstimatedTokens holds a best-effort token estimate for this item, when a
	// caller has already computed one. It is informational and never trusted by
	// the compactors, which re-estimate via TokenEstimator.
	EstimatedTokens int `json:"estimated_tokens,omitempty"`
	// ContentBlocks holds typed content parts for multimodal messages
	// (text + images). When non-nil, it takes precedence over Content.
	ContentBlocks []llm.ContentBlock `json:"content_blocks,omitempty"`
	// ToolCalls holds tool invocations requested by the assistant.
	ToolCalls []llm.ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID associates a tool-result message with the originating tool
	// call.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// Strategy identifies which compaction approach produced or was requested for a
// result. Values are stable and serializable.
type Strategy int

// Compaction strategies.
const (
	// StrategyNone indicates no strategy applied.
	StrategyNone Strategy = iota
	// StrategyMicro uses the zero-LLM placeholder strategy.
	StrategyMicro
	// StrategySummary uses the LLM summarization strategy.
	StrategySummary
	// StrategyTruncating uses the truncation fallback strategy.
	StrategyTruncating
)

// String returns a stable, lowercase identifier for the strategy.
func (s Strategy) String() string {
	switch s {
	case StrategyMicro:
		return "micro"
	case StrategySummary:
		return "summary"
	case StrategyTruncating:
		return "truncating"
	default:
		return "none"
	}
}

// ErrRequiresTruncating is returned by MicroCompactor and SummaryCompactor when
// their output still exceeds the budget and a heavier fallback
// (TruncatingCompactor) is required to satisfy the constraint.
var ErrRequiresTruncating = errors.New("compaction: budget exceeded; truncating required")

// Compactor rewrites a conversation so that its estimated token count does not
// exceed maxTokens.
type Compactor interface {
	// Compact compacts items under the given token budget, returning the
	// rewritten list and any error. Implementations must be safe for concurrent
	// use and must not mutate the input slice's backing array unless they are
	// the sole owner.
	Compact(ctx context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator) ([]TurnItem, error)
}

// Summarizer produces a summary for a given conversation text. It is a narrow,
// package-local interface so SummaryCompactor does not depend on any concrete
// LLM provider (and thus avoids importing internal/llm). Real providers adapt
// their chat models to this interface at the wiring layer.
type Summarizer interface {
	// Summarize returns a concise summary of conversation.
	Summarize(ctx context.Context, conversation string) (string, error)
}

// estimateTokens sums the estimated tokens of every item's content and tool
// result. Estimator errors are ignored because production estimators are
// best-effort by contract.
func estimateTokens(items []TurnItem, estimator TokenEstimator) int {
	total := 0
	for _, it := range items {
		if it.Content != "" {
			total += estimateLength(it.Content, estimator)
		}
		if it.ToolResult != "" {
			total += estimateLength(it.ToolResult, estimator)
		}
		for _, block := range it.ContentBlocks {
			switch block.Type {
			case "text":
				if block.Text != "" {
					total += estimateLength(block.Text, estimator)
				}
			case "image_url":
				total += 85
			default:
				if block.Text != "" {
					total += estimateLength(block.Text, estimator)
				}
			}
		}
	}
	return total
}

// estimateLength estimates the token count of a single text blob.
func estimateLength(text string, estimator TokenEstimator) int {
	if estimator == nil {
		return len(text) / 4
	}
	n, err := estimator.Estimate(text)
	if err != nil {
		// Fall back to the heuristic when an estimator reports an error. This
		// keeps compaction resilient under a failing estimator.
		slog.Warn("compaction.estimator_error", "err", err)
		return len(text) / 4
	}
	return n
}
