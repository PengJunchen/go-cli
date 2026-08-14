package compaction

import (
	"log/slog"
	"math"
	"unicode"
	"unicode/utf8"
)

// TokenEstimator estimates the token count of a piece of text. Estimators let
// the compactors reason about context size without binding to any specific
// tokenizer or model family.
type TokenEstimator interface {
	// Estimate returns the approximate number of tokens in text. It is
	// best-effort; callers must tolerate both zero and large estimates.
	Estimate(text string) (int, error)
}

// HeuristicTokenEstimator estimates tokens using Unicode-aware heuristics. It
// delegates to UnicodeTokenEstimator, which weights CJK characters at ~2 tokens
// each and ASCII at ~0.25 tokens each, so non-English text is not undercounted
// the way a byte-length divisor would. It never fails for ordinary input.
type HeuristicTokenEstimator struct{}

// Compile-time assertion that HeuristicTokenEstimator satisfies TokenEstimator.
var _ TokenEstimator = (*HeuristicTokenEstimator)(nil)

// NewHeuristicTokenEstimator returns a default-configured heuristic estimator.
func NewHeuristicTokenEstimator() *HeuristicTokenEstimator {
	return &HeuristicTokenEstimator{}
}

// Estimate delegates to UnicodeTokenEstimator, which weights CJK runes as 2
// tokens and ASCII alphanumerics as 0.25. The error is always nil.
func (e *HeuristicTokenEstimator) Estimate(text string) (int, error) {
	return NewUnicodeTokenEstimator().Estimate(text)
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
			sum++
		}
	}
	n := int(math.Round(sum))
	slog.Debug("compaction.estimate", "chars", chars, "estimated_tokens", n, "estimator", "unicode")
	return n, nil
}

// FastTokenEstimator provides a fast token estimate using simple heuristics
// instead of a per-character category scan. For ASCII-heavy text it uses
// len(text)/4 (roughly 4 characters per token). For CJK-heavy text it uses
// rune_count*1.5 (each CJK character is roughly 1-2 tokens). Pure-ASCII input
// skips the rune scan entirely via utf8.RuneCountInString.
type FastTokenEstimator struct{}

// Compile-time assertion that FastTokenEstimator satisfies TokenEstimator.
var _ TokenEstimator = (*FastTokenEstimator)(nil)

// NewFastTokenEstimator returns a default-configured fast estimator.
func NewFastTokenEstimator() *FastTokenEstimator {
	return &FastTokenEstimator{}
}

// Estimate approximates the token count using simple length-based heuristics.
// When the text is pure ASCII it returns len(text)/4. When the text contains
// a significant proportion of CJK runes it uses a weighted formula that
// splits CJK and non-CJK characters: CJK runes contribute ~1.5 tokens each
// while non-CJK runes contribute ~0.25 tokens each (≈4 chars per token).
// This avoids the ~50% underestimation that a naive len/4 produces for
// mixed Chinese-English text. The error is always nil.
func (e *FastTokenEstimator) Estimate(text string) (int, error) {
	runeCount := utf8.RuneCountInString(text)
	// Pure-ASCII fast path: no rune iteration needed.
	if len(text) == runeCount {
		n := len(text) / 4
		slog.Debug("compaction.estimate", "chars", runeCount, "estimated_tokens", n, "estimator", "fast")
		return n, nil
	}
	// Multi-byte text: count CJK runes to decide the formula.
	cjk := 0
	for _, r := range text {
		if isCJK(r) {
			cjk++
		}
	}
	var n int
	if cjk > runeCount/3 {
		// CJK-heavy text: weight CJK runes at 1.5 tokens and non-CJK
		// runes at 0.25 tokens (≈4 chars/token for ASCII).
		nonCJK := runeCount - cjk
		n = int(math.Round(float64(cjk)*1.5 + float64(nonCJK)*0.25))
	} else {
		// Mixed or mostly-ASCII text with some multi-byte chars:
		// use byte-length/4 as a reasonable approximation.
		n = len(text) / 4
	}
	slog.Debug("compaction.estimate", "chars", runeCount, "cjk", cjk, "estimated_tokens", n, "estimator", "fast")
	return n, nil
}

// CompositeTokenEstimator combines a fast estimator with a precise one. For
// texts shorter than threshold characters it uses the precise
// UnicodeTokenEstimator; for longer texts it falls back to the
// FastTokenEstimator to avoid the expensive per-character scan.
type CompositeTokenEstimator struct {
	fast      *FastTokenEstimator
	precise   *UnicodeTokenEstimator
	threshold int
}

// Compile-time assertion that CompositeTokenEstimator satisfies TokenEstimator.
var _ TokenEstimator = (*CompositeTokenEstimator)(nil)

// NewCompositeTokenEstimator returns a composite estimator that uses the
// precise UnicodeTokenEstimator for texts shorter than threshold characters
// and the FastTokenEstimator for longer texts. A threshold of zero or less
// defaults to 10000.
func NewCompositeTokenEstimator(threshold int) *CompositeTokenEstimator {
	if threshold <= 0 {
		threshold = 10000
	}
	return &CompositeTokenEstimator{
		fast:      NewFastTokenEstimator(),
		precise:   NewUnicodeTokenEstimator(),
		threshold: threshold,
	}
}

// Estimate delegates to the precise estimator for short texts and to the fast
// estimator for texts at or above the threshold.
func (e *CompositeTokenEstimator) Estimate(text string) (int, error) {
	if len(text) < e.threshold {
		return e.precise.Estimate(text)
	}
	return e.fast.Estimate(text)
}
