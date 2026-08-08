package compaction

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// summaryPlaceholder is the marker text used for the compaction entry content
// when the underlying model returns an empty summary.
const summaryPlaceholder = "[summary ...]"

// fileOpNames are tool names whose results are considered file-operation
// history worth aggregating into the summary prompt.
var fileOpNames = []string{"read", "write", "edit", "grep"}

// errNoSummarizer is the sentinel returned by the fallback Summarizer.
var errNoSummarizer = errors.New("compaction: no summarizer configured")

// SummaryCompactor performs LLM-driven compaction. It cuts the conversation at
// message boundaries (findCutPoint), aggregates the file-operation history of
// the region being summarized, and replaces that region with a single summary
// entry. It only summarizes the not-yet-compacted region, so already-compacted
// entries are preserved rather than re-summarized.
//
// The Summarizer is injected through the constructor; SummaryCompactor never
// depends on a concrete LLM provider.
type SummaryCompactor struct {
	summarizer       Summarizer
	maxSummaryTokens int
}

// Compile-time assertion that SummaryCompactor satisfies Compactor.
var _ Compactor = (*SummaryCompactor)(nil)

// noopSummarizer is the fallback used when no Summarizer is provided. It returns
// an explicit error so wiring mistakes surface loudly instead of silently
// emitting an empty summary.
type noopSummarizer struct{}

// Compile-time assertion that the fallback satisfies Summarizer.
var _ Summarizer = (*noopSummarizer)(nil)

// Summarize always fails with a descriptive error.
func (noopSummarizer) Summarize(_ context.Context, _ string) (string, error) {
	return "", errNoSummarizer
}

// SummaryCompactorOption configures a SummaryCompactor.
type SummaryCompactorOption func(*SummaryCompactor)

// WithMaxSummaryTokens bounds the length of the produced summary entry.
func WithMaxSummaryTokens(n int) SummaryCompactorOption {
	return func(c *SummaryCompactor) { c.maxSummaryTokens = n }
}

// defaultMaxSummaryTokens is applied when no option overrides it.
const defaultMaxSummaryTokens = 2000

// NewSummaryCompactor returns a SummaryCompactor that delegates summarization to
// summarizer. When summarizer is nil, a fallback that errors is used.
func NewSummaryCompactor(summarizer Summarizer, opts ...SummaryCompactorOption) *SummaryCompactor {
	c := &SummaryCompactor{
		summarizer:       summarizer,
		maxSummaryTokens: defaultMaxSummaryTokens,
	}
	if c.summarizer == nil {
		c.summarizer = noopSummarizer{}
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// findCutPoint returns the smallest cut index such that summarizing items[:cut]
// into a single placeholder and keeping items[cut:] whole stays under maxTokens.
// Cuts are only placed on message boundaries: the kept region never begins on a
// dangling tool result. When no cut satisfies the budget, it returns len(items)
// (i.e. summarize everything).
func (c *SummaryCompactor) findCutPoint(items []TurnItem, maxTokens int, estimator TokenEstimator) int {
	placeholder := estimateTokens([]TurnItem{{Content: summaryPlaceholder}}, estimator)
	for cut := 0; cut <= len(items); cut++ {
		if cut < len(items) && items[cut].Role == RoleTool {
			continue
		}
		if placeholder+estimateTokens(items[cut:], estimator) <= maxTokens {
			return cut
		}
	}
	return len(items)
}

// Compact summarizes the oldest region and keeps the newest turns whole.
func (c *SummaryCompactor) Compact(ctx context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator) ([]TurnItem, error) {
	span, _ := tracing.SpanFromContext(ctx, "compaction.summary", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())

	tokensBefore := estimateTokens(items, estimator)
	span.SetAttributes(
		tracing.Attribute{Key: "items_in", Value: len(items)},
		tracing.Attribute{Key: "tokens_before", Value: tokensBefore},
	)

	cut := c.findCutPoint(items, maxTokens, estimator)
	if cut == 0 {
		// Nothing can be summarized without exceeding budget; escalate to the
		// truncating fallback. Returning the input unchanged primitives the
		// unified router to escalate.
		return items, ErrRequiresTruncating
	}

	old := items[:cut]
	keep := items[cut:]

	prompt := c.buildSummaryPrompt(old)
	summary, err := c.summarizer.Summarize(ctx, prompt)
	if err != nil {
		logger.Warn("compaction.summary.failed", "err", err)
		return items, err
	}
	if summary == "" {
		summary = summaryPlaceholder
	}
	if summary = c.clampSummary(summary, estimator); summary == "" {
		summary = summaryPlaceholder
	}

	// Build the compacted list: a single summary entry followed by the intact
	// recent turns. The summary entry replaces the entire old region.
	result := make([]TurnItem, 0, 1+len(keep))
	result = append(result, TurnItem{
		Role:         RoleSystem,
		Content:      summary,
		IsCompaction: true,
	})
	result = append(result, keep...)

	tokensAfter := estimateTokens(result, estimator)
	span.SetAttributes(
		tracing.Attribute{Key: "cut_point", Value: cut},
		tracing.Attribute{Key: "items_out", Value: len(result)},
		tracing.Attribute{Key: "tokens_after", Value: tokensAfter},
	)

	if tokensAfter > maxTokens {
		logger.Warn("compaction.summary.insufficient", "max_tokens", maxTokens, "tokens_after", tokensAfter)
		return result, ErrRequiresTruncating
	}

	logger.Info("compaction.summary.done",
		"cut_point", cut,
		"items_in", len(items),
		"items_out", len(result),
		"tokens_before", tokensBefore,
		"tokens_after", tokensAfter)
	return result, nil
}

// buildSummaryPrompt aggregates file-operation history from the items being
// summarized into a compact instruction for the Summarizer.
func (c *SummaryCompactor) buildSummaryPrompt(items []TurnItem) string {
	var sb strings.Builder
	sb.WriteString("Summarize the following conversation, focusing on the ongoing task, decisions, and file operations. Keep the summary concise and self-contained.\n\n")

	var ops []string
	var messages []string
	for _, it := range items {
		if it.IsCompaction {
			// Preserve an already-compacted entry verbatim; it is part of the
			// conversation history and should not be re-derived.
			messages = append(messages, it.Content)
			continue
		}
		if it.Content != "" {
			messages = append(messages, it.Content)
		}
		if it.ToolResult != "" && isFileOp(it.ToolName) {
			ops = append(ops, it.ToolResult)
		}
	}

	if len(ops) > 0 {
		sb.WriteString("\n[file operations]\n")
		for _, op := range ops {
			sb.WriteString(op)
			sb.WriteString("\n")
		}
	}

	sb.WriteString("\n[messages]\n")
	for _, msg := range messages {
		sb.WriteString(msg)
		sb.WriteString("\n")
	}
	return sb.String()
}

// clampSummary bounds the summary text to maxSummaryTokens using the provided
// estimator, so the compaction entry cannot balloon past the intended size.
// Truncation is performed at rune boundaries to avoid splitting multi-byte
// characters.
func (c *SummaryCompactor) clampSummary(summary string, estimator TokenEstimator) string {
	if c.maxSummaryTokens <= 0 {
		return summary
	}
	if estimateLength(summary, estimator) <= c.maxSummaryTokens {
		return summary
	}
	// Binary search for the longest rune prefix whose estimate fits the budget.
	// Token estimates are monotonically non-decreasing with prefix length (every
	// rune contributes a non-negative weight), so binary search is valid.
	runes := []rune(summary)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if estimateLength(string(runes[:mid]), estimator) <= c.maxSummaryTokens {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo <= 0 {
		return summary
	}
	return string(runes[:lo])
}

// isFileOp reports whether a tool name is a file-operation tool.
func isFileOp(name string) bool {
	for _, n := range fileOpNames {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}
