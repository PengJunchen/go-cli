package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTruncateHeadShortContent(t *testing.T) {
	s := TruncateHead{}
	assert.Equal(t, "hello", s.Truncate("hello", 100))
}

func TestTruncateHeadLongContent(t *testing.T) {
	s := TruncateHead{}
	result := s.Truncate("hello world", 5)
	assert.Equal(t, "hello... [truncated]", result)
}

func TestTruncateHeadExactFit(t *testing.T) {
	s := TruncateHead{}
	assert.Equal(t, "hello", s.Truncate("hello", 5))
}

func TestTruncateHeadZeroLimit(t *testing.T) {
	s := TruncateHead{}
	assert.Equal(t, "hello", s.Truncate("hello", 0))
}

func TestTruncateHeadNegativeLimit(t *testing.T) {
	s := TruncateHead{}
	assert.Equal(t, "hello", s.Truncate("hello", -1))
}

func TestTruncateTailShortContent(t *testing.T) {
	s := TruncateTail{}
	assert.Equal(t, "hello", s.Truncate("hello", 100))
}

func TestTruncateTailLongContent(t *testing.T) {
	s := TruncateTail{}
	result := s.Truncate("hello world", 5)
	assert.Equal(t, "... [truncated] ...world", result)
}

func TestTruncateTailExactFit(t *testing.T) {
	s := TruncateTail{}
	assert.Equal(t, "hello", s.Truncate("hello", 5))
}

func TestTruncateTailZeroLimit(t *testing.T) {
	s := TruncateTail{}
	assert.Equal(t, "hello", s.Truncate("hello", 0))
}

func TestTruncateLineShortContent(t *testing.T) {
	s := TruncateLine{}
	content := "line1\nline2\nline3"
	assert.Equal(t, content, s.Truncate(content, 10))
}

func TestTruncateLineLongContent(t *testing.T) {
	s := TruncateLine{}
	content := "line1\nline2\nline3\nline4\nline5"
	result := s.Truncate(content, 2)
	assert.Equal(t, "line1\nline2\n... [3 lines truncated]", result)
}

func TestTruncateLineExactFit(t *testing.T) {
	s := TruncateLine{}
	content := "line1\nline2\nline3"
	assert.Equal(t, content, s.Truncate(content, 3))
}

func TestTruncateLineZeroLimit(t *testing.T) {
	s := TruncateLine{}
	assert.Equal(t, "hello", s.Truncate("hello", 0))
}

func TestTruncateLineSingleLine(t *testing.T) {
	s := TruncateLine{}
	assert.Equal(t, "only line", s.Truncate("only line", 1))
}

func TestTruncaterApplyDisabled(t *testing.T) {
	tr := NewTruncater(TruncateConfig{
		Strategy: TruncateHead{},
		Limit:    5,
		Enabled:  false,
	})
	assert.Equal(t, "hello world", tr.Apply("hello world"))
}

func TestTruncaterApplyEnabled(t *testing.T) {
	tr := NewTruncater(TruncateConfig{
		Strategy: TruncateHead{},
		Limit:    5,
		Enabled:  true,
	})
	assert.Equal(t, "hello... [truncated]", tr.Apply("hello world"))
}

func TestTruncaterApplyNilStrategy(t *testing.T) {
	tr := NewTruncater(TruncateConfig{
		Strategy: nil,
		Limit:    5,
		Enabled:  true,
	})
	assert.Equal(t, "hello world", tr.Apply("hello world"))
}

func TestTruncaterApplyZeroLimit(t *testing.T) {
	tr := NewTruncater(TruncateConfig{
		Strategy: TruncateHead{},
		Limit:    0,
		Enabled:  true,
	})
	assert.Equal(t, "hello world", tr.Apply("hello world"))
}

func TestTruncaterApplyNegativeLimit(t *testing.T) {
	tr := NewTruncater(TruncateConfig{
		Strategy: TruncateHead{},
		Limit:    -1,
		Enabled:  true,
	})
	assert.Equal(t, "hello world", tr.Apply("hello world"))
}

func TestTruncaterApplyWithTailStrategy(t *testing.T) {
	tr := NewTruncater(TruncateConfig{
		Strategy: TruncateTail{},
		Limit:    5,
		Enabled:  true,
	})
	assert.Equal(t, "... [truncated] ...world", tr.Apply("hello world"))
}

func TestTruncaterApplyWithLineStrategy(t *testing.T) {
	tr := NewTruncater(TruncateConfig{
		Strategy: TruncateLine{},
		Limit:    2,
		Enabled:  true,
	})
	content := "line1\nline2\nline3\nline4"
	result := tr.Apply(content)
	assert.Equal(t, "line1\nline2\n... [2 lines truncated]", result)
}

func TestTruncaterApplyShortContentUnchanged(t *testing.T) {
	tr := NewTruncater(TruncateConfig{
		Strategy: TruncateHead{},
		Limit:    100,
		Enabled:  true,
	})
	assert.Equal(t, "short", tr.Apply("short"))
}

func TestTruncateStrategiesImplementInterface(t *testing.T) {
	var _ TruncateStrategy = TruncateHead{}
	var _ TruncateStrategy = TruncateTail{}
	var _ TruncateStrategy = TruncateLine{}
}

func TestTruncateLineWithTrailingNewline(t *testing.T) {
	s := TruncateLine{}
	content := "a\nb\nc\nd\n"
	// strings.Split on "a\nb\nc\nd\n" yields ["a","b","c","d",""]
	lines := strings.Split(content, "\n")
	assert.Equal(t, 5, len(lines))

	result := s.Truncate(content, 2)
	assert.Equal(t, "a\nb\n... [3 lines truncated]", result)
}
