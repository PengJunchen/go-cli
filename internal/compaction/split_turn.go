package compaction

import (
	"context"
	"log/slog"
	"sync"
	"unicode/utf8"
)

// defaultSplitThreshold is the token count above which a single turn is split
// into two summary parts. It is applied when no explicit threshold is provided.
const defaultSplitThreshold = 4000

// SplitTurnResult contains the two summary parts produced by splitting a single
// oversized turn, along with token-count diagnostics.
type SplitTurnResult struct {
	FirstPart      string
	SecondPart     string
	OriginalTokens int
	SplitTokens    int
}

// SplitTurnCompactor handles single turns that exceed the token budget by
// splitting them into two summary parts. It is a heuristic, non-LLM compactor:
// the content is divided at its midpoint and each half is independently
// summarized via the injected Summarizer.
//
// When no Summarizer is configured a fallback that returns the first N
// characters of each half is used, so the compactor remains useful in test and
// offline contexts.
type SplitTurnCompactor struct {
	mu         sync.Mutex
	threshold  int
	summarizer Summarizer
}

// Compile-time assertion that SplitTurnCompactor is usable as a value type.
var _ = (*SplitTurnCompactor)(nil)

// SplitTurnOption configures a SplitTurnCompactor.
type SplitTurnOption func(*SplitTurnCompactor)

// WithSplitThreshold sets the token threshold above which a split is triggered.
func WithSplitThreshold(n int) SplitTurnOption {
	return func(c *SplitTurnCompactor) { c.threshold = n }
}

// WithSplitSummarizer injects a Summarizer used to summarize each half.
func WithSplitSummarizer(s Summarizer) SplitTurnOption {
	return func(c *SplitTurnCompactor) { c.summarizer = s }
}

// splitFallbackSummarizer is the default summarizer used when none is injected.
// It returns a truncated prefix of the text, keeping the compactor functional
// without an LLM dependency.
type splitFallbackSummarizer struct{}

var _ Summarizer = (*splitFallbackSummarizer)(nil)

func (splitFallbackSummarizer) Summarize(_ context.Context, text string) (string, error) {
	const maxRunes = 500
	if utf8.RuneCountInString(text) <= maxRunes {
		return text, nil
	}
	runes := []rune(text)
	return string(runes[:maxRunes]), nil
}

// NewSplitTurnCompactor returns a SplitTurnCompactor with the given threshold.
// When threshold is zero or negative, the defaultSplitThreshold is used.
func NewSplitTurnCompactor(threshold int) *SplitTurnCompactor {
	if threshold <= 0 {
		threshold = defaultSplitThreshold
	}
	return &SplitTurnCompactor{
		threshold:  threshold,
		summarizer: splitFallbackSummarizer{},
	}
}

// NewSplitTurnCompactorWithOptions returns a SplitTurnCompactor configured via
// options. This is the preferred constructor when injecting a summarizer.
func NewSplitTurnCompactorWithOptions(opts ...SplitTurnOption) *SplitTurnCompactor {
	c := &SplitTurnCompactor{
		threshold:  defaultSplitThreshold,
		summarizer: splitFallbackSummarizer{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// SetThreshold updates the token threshold at runtime.
func (c *SplitTurnCompactor) SetThreshold(n int) {
	c.mu.Lock()
	c.threshold = n
	c.mu.Unlock()
	slog.Debug("compaction.split_turn.set_threshold", "threshold", n)
}

// Threshold returns the current token threshold.
func (c *SplitTurnCompactor) Threshold() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.threshold
}

// ShouldSplit reports whether the content's estimated token count exceeds the
// configured threshold.
func (c *SplitTurnCompactor) ShouldSplit(content string, estimator TokenEstimator) bool {
	if content == "" {
		return false
	}
	tokens := estimateLength(content, estimator)
	c.mu.Lock()
	threshold := c.threshold
	c.mu.Unlock()

	slog.Debug("compaction.split_turn.should_split",
		"tokens", tokens,
		"threshold", threshold,
		"will_split", tokens > threshold,
	)
	return tokens > threshold
}

// Split divides the content at its midpoint and produces a summary for each
// half. The result includes the original and post-split token counts so callers
// can verify the reduction.
func (c *SplitTurnCompactor) Split(ctx context.Context, content string, estimator TokenEstimator) SplitTurnResult {
	originalTokens := estimateLength(content, estimator)

	mid := len(content) / 2
	if mid == 0 {
		// Content too short to split meaningfully; return as-is.
		return SplitTurnResult{
			FirstPart:      content,
			SecondPart:     "",
			OriginalTokens: originalTokens,
			SplitTokens:    originalTokens,
		}
	}

	// Try to split on a boundary (newline or space) near the midpoint so the
	// halves are self-contained.
	firstHalf, secondHalf := splitAtBoundary(content, mid)

	c.mu.Lock()
	summarizer := c.summarizer
	c.mu.Unlock()

	firstSummary, err := summarizer.Summarize(ctx, firstHalf)
	if err != nil || firstSummary == "" {
		slog.Warn("compaction.split_turn.first_half_failed", "err", err)
		firstSummary = firstHalf
	}
	secondSummary, err := summarizer.Summarize(ctx, secondHalf)
	if err != nil || secondSummary == "" {
		slog.Warn("compaction.split_turn.second_half_failed", "err", err)
		secondSummary = secondHalf
	}

	splitTokens := estimateLength(firstSummary, estimator) + estimateLength(secondSummary, estimator)

	slog.Info("compaction.split_turn.done",
		"original_tokens", originalTokens,
		"split_tokens", splitTokens,
		"first_part_len", len(firstSummary),
		"second_part_len", len(secondSummary),
	)

	return SplitTurnResult{
		FirstPart:      firstSummary,
		SecondPart:     secondSummary,
		OriginalTokens: originalTokens,
		SplitTokens:    splitTokens,
	}
}

// splitAtBoundary divides content at or near mid, preferring a newline, then a
// space, then the exact midpoint.
func splitAtBoundary(content string, mid int) (string, string) {
	// Search a window around the midpoint for a newline.
	window := len(content) / 10
	if window < 1 {
		window = 1
	}
	start := mid - window
	if start < 0 {
		start = 0
	}
	end := mid + window
	if end > len(content) {
		end = len(content)
	}

	// Prefer newline.
	for i := mid; i < end; i++ {
		if content[i] == '\n' {
			return content[:i], content[i+1:]
		}
	}
	for i := mid; i >= start; i-- {
		if content[i] == '\n' {
			return content[:i], content[i+1:]
		}
	}
	// Then a space.
	for i := mid; i < end; i++ {
		if content[i] == ' ' {
			return content[:i], content[i+1:]
		}
	}
	for i := mid; i >= start; i-- {
		if content[i] == ' ' {
			return content[:i], content[i+1:]
		}
	}
	// Fall back to the exact midpoint, aligned to a rune boundary so we
	// never split a multi-byte UTF-8 character.
	for mid > 0 && !utf8.RuneStart(content[mid]) {
		mid--
	}
	return content[:mid], content[mid:]
}
