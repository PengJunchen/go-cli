package compaction

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitTurnCompactor_NewWithThreshold(t *testing.T) {
	c := NewSplitTurnCompactor(5000)
	assert.Equal(t, 5000, c.Threshold())

	cDefault := NewSplitTurnCompactor(0)
	assert.Equal(t, defaultSplitThreshold, cDefault.Threshold(), "zero should fall back to default")

	cNeg := NewSplitTurnCompactor(-1)
	assert.Equal(t, defaultSplitThreshold, cNeg.Threshold(), "negative should fall back to default")
}

func TestSplitTurnCompactor_SetThreshold(t *testing.T) {
	c := NewSplitTurnCompactor(1000)
	c.SetThreshold(2000)
	assert.Equal(t, 2000, c.Threshold())
}

func TestSplitTurnCompactor_ShouldSplitFalseForEmpty(t *testing.T) {
	c := NewSplitTurnCompactor(100)
	est := NewHeuristicTokenEstimator()
	assert.False(t, c.ShouldSplit("", est))
}

func TestSplitTurnCompactor_ShouldSplitFalseUnderThreshold(t *testing.T) {
	c := NewSplitTurnCompactor(1000)
	est := NewHeuristicTokenEstimator()
	// 400 chars / 4 = 100 tokens < 1000 threshold
	content := strings.Repeat("a", 400)
	assert.False(t, c.ShouldSplit(content, est))
}

func TestSplitTurnCompactor_ShouldSplitTrueOverThreshold(t *testing.T) {
	c := NewSplitTurnCompactor(100)
	est := NewHeuristicTokenEstimator()
	// 800 chars / 4 = 200 tokens > 100 threshold
	content := strings.Repeat("a", 800)
	assert.True(t, c.ShouldSplit(content, est))
}

func TestSplitTurnCompactor_SplitProducesTwoParts(t *testing.T) {
	c := NewSplitTurnCompactor(100)
	est := NewHeuristicTokenEstimator()
	// Create content with a clear midpoint boundary.
	content := strings.Repeat("first half content. ", 20) + "\n" + strings.Repeat("second half content. ", 20)

	result := c.Split(context.Background(), content, est)

	assert.NotEmpty(t, result.FirstPart)
	assert.NotEmpty(t, result.SecondPart)
	assert.Greater(t, result.OriginalTokens, 0)
	assert.Greater(t, result.SplitTokens, 0)
	// With the fallback summarizer (truncate to 500 chars), the split tokens
	// should be less than or equal to the original tokens.
	assert.LessOrEqual(t, result.SplitTokens, result.OriginalTokens)
}

func TestSplitTurnCompactor_SplitShortContent(t *testing.T) {
	c := NewSplitTurnCompactor(100000) // very high threshold, but we call Split directly
	est := NewHeuristicTokenEstimator()
	content := "a"

	result := c.Split(context.Background(), content, est)

	// Content too short to split (mid == 0): first part is the content, second is empty.
	assert.Equal(t, "a", result.FirstPart)
	assert.Empty(t, result.SecondPart)
}

func TestSplitTurnCompactor_SplitAtNewlineBoundary(t *testing.T) {
	first, second := splitAtBoundary("hello\nworld", 5)
	assert.Equal(t, "hello", first)
	assert.Equal(t, "world", second)
}

func TestSplitTurnCompactor_SplitAtSpaceBoundary(t *testing.T) {
	// "aaaaaa bbbbbb" has length 13, mid=6, content[6]=' ' so splits at the space.
	first, second := splitAtBoundary("aaaaaa bbbbbb", 6)
	assert.Equal(t, "aaaaaa", first)
	assert.Equal(t, "bbbbbb", second)
}

func TestSplitTurnCompactor_SplitAtMidpointFallback(t *testing.T) {
	// No boundaries near midpoint.
	content := "abcdefghijklmnopqrstuvwxyz"
	first, second := splitAtBoundary(content, 13)
	assert.Equal(t, content[:13], first)
	assert.Equal(t, content[13:], second)
}

func TestSplitTurnCompactor_WithInjectedSummarizer(t *testing.T) {
	summarizer := &mockSplitSummarizer{}
	c := NewSplitTurnCompactorWithOptions(
		WithSplitThreshold(10),
		WithSplitSummarizer(summarizer),
	)
	est := NewHeuristicTokenEstimator()

	// Use distinguishable content so each half is identifiable after summarization.
	content := strings.Repeat("a", 100) + "\n" + strings.Repeat("b", 100)
	require.True(t, c.ShouldSplit(content, est))

	result := c.Split(context.Background(), content, est)
	// The mock summarizer returns "SUMMARY:" + the text it received.
	assert.Contains(t, result.FirstPart, "SUMMARY:")
	assert.Contains(t, result.FirstPart, "a")
	assert.NotContains(t, result.FirstPart, "b", "first part should only contain a's")
	assert.Contains(t, result.SecondPart, "SUMMARY:")
	assert.Contains(t, result.SecondPart, "b")
	assert.NotContains(t, result.SecondPart, "a", "second part should only contain b's")
	// 200 alphanumerics (0.25 each) + 1 newline (0.5) = 50.5 → rounds to 51.
	assert.Equal(t, 51, result.OriginalTokens)
}

func TestSplitTurnCompactor_SummarizerErrorFallback(t *testing.T) {
	summarizer := &errSplitSummarizer{}
	c := NewSplitTurnCompactorWithOptions(
		WithSplitThreshold(10),
		WithSplitSummarizer(summarizer),
	)
	est := NewHeuristicTokenEstimator()

	content := strings.Repeat("x", 200)
	result := c.Split(context.Background(), content, est)

	// When summarizer fails, the raw half should be used.
	assert.NotEmpty(t, result.FirstPart)
	assert.NotEmpty(t, result.SecondPart)
}

func TestSplitTurnCompactor_Concurrent(t *testing.T) {
	c := NewSplitTurnCompactor(100)
	est := NewHeuristicTokenEstimator()
	content := strings.Repeat("a", 800)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			result := c.Split(context.Background(), content, est)
			assert.NotEmpty(t, result.FirstPart)
		}()
	}
	wg.Wait()
}

// mockSplitSummarizer returns deterministic summaries prefixed by the half.
type mockSplitSummarizer struct{}

func (m *mockSplitSummarizer) Summarize(_ context.Context, text string) (string, error) {
	return "SUMMARY:" + text, nil
}

// errSplitSummarizer always fails, exercising the fallback path.
type errSplitSummarizer struct{}

func (e *errSplitSummarizer) Summarize(_ context.Context, _ string) (string, error) {
	return "", assertError("summarizer unavailable")
}

func assertError(msg string) error {
	return &simpleError{msg: msg}
}

type simpleError struct{ msg string }

func (e *simpleError) Error() string { return e.msg }
