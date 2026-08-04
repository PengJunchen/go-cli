package compaction

import (
	"log/slog"
	"math"
	"unicode"
)

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

// UnicodeTokenEstimator estimates token counts using Unicode-aware heuristics.
// CJK characters are weighted at ~2 tokens each, ASCII at ~0.25 tokens each.
type UnicodeTokenEstimator struct{}

// Compile-time assertion that UnicodeTokenEstimator satisfies TokenEstimator.
var _ TokenEstimator = (*UnicodeTokenEstimator)(nil)

// NewUnicodeTokenEstimator returns a default-configured Unicode-aware estimator.
func NewUnicodeTokenEstimator() *UnicodeTokenEstimator {
	return &UnicodeTokenEstimator{}
}

// isCJK reports whether r is a CJK ideograph or Japanese/Korean syllable.
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF: // CJK Unified Ideographs
		return true
	case r >= 0x3040 && r <= 0x30FF: // Hiragana, Katakana
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // Hangul Syllables
		return true
	}
	return false
}

// Estimate approximates the token count by weighting runes by category. CJK
// runes count as 2 tokens, ASCII alphanumerics as 0.25, ASCII whitespace and
// punctuation as 0.5, and any other rune as 1.
func (e *UnicodeTokenEstimator) Estimate(text string) (int, error) {
	var sum float64
	chars := 0
	for _, r := range text {
		chars++
		switch {
		case isCJK(r):
			sum += 2
		case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			sum += 0.25
		case r < 0x80 && (unicode.IsSpace(r) || unicode.IsPunct(r)):
			sum += 0.5
		default:
			sum += 1
		}
	}
	n := int(math.Round(sum))
	slog.Debug("compaction.estimate", "chars", chars, "estimated_tokens", n, "estimator", "unicode")
	return n, nil
}

// CompositeTokenEstimator wraps a primary estimator and optionally a precise
// tokenizer. When a precise tokenizer is available, it is used; otherwise
// the primary (heuristic) estimator is used.
type CompositeTokenEstimator struct {
	primary TokenEstimator
	precise TokenEstimator // optional, nil means use primary
}

// Compile-time assertion that CompositeTokenEstimator satisfies TokenEstimator.
var _ TokenEstimator = (*CompositeTokenEstimator)(nil)

// NewCompositeTokenEstimator returns a composite estimator that delegates to
// primary when no precise tokenizer has been configured.
func NewCompositeTokenEstimator(primary TokenEstimator) *CompositeTokenEstimator {
	return &CompositeTokenEstimator{primary: primary}
}

// SetPrecise installs a precise tokenizer that, when set, takes precedence
// over the primary heuristic estimator.
func (e *CompositeTokenEstimator) SetPrecise(p TokenEstimator) {
	e.precise = p
}

// Estimate delegates to the precise tokenizer when available, otherwise to
// the primary estimator.
func (e *CompositeTokenEstimator) Estimate(text string) (int, error) {
	if e.precise != nil {
		return e.precise.Estimate(text)
	}
	return e.primary.Estimate(text)
}
