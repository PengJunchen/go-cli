package compaction

import "log/slog"

// TokenEstimator estimates the token count of a piece of text. Estimators let
// the compactors reason about context size without binding to any specific
// tokenizer or model family.
type TokenEstimator interface {
	// Estimate returns the approximate number of tokens in text. It is
	// best-effort; callers must tolerate both zero and large estimates.
	Estimate(text string) (int, error)
}

// HeuristicTokenEstimator estimates tokens by dividing the number of characters
// by four, a common rule-of-thumb (roughly four characters per token for
// English prose). It never fails for ordinary input.
type HeuristicTokenEstimator struct{}

// Compile-time assertion that HeuristicTokenEstimator satisfies TokenEstimator.
var _ TokenEstimator = (*HeuristicTokenEstimator)(nil)

// NewHeuristicTokenEstimator returns a default-configured heuristic estimator.
func NewHeuristicTokenEstimator() *HeuristicTokenEstimator {
	return &HeuristicTokenEstimator{}
}

// Estimate returns len(text)/4. The error is always nil for non-negative length
// input, so callers can safely ignore it.
func (e *HeuristicTokenEstimator) Estimate(text string) (int, error) {
	n := len(text) / 4
	slog.Debug("compaction.estimate", "chars", len(text), "tokens", n)
	return n, nil
}
