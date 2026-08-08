package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStreamingRenderer creates a StreamingMarkdownRenderer with a real
// MarkdownRenderer for testing.
func newTestStreamingRenderer() *StreamingMarkdownRenderer {
	return NewStreamingMarkdownRenderer(NewMarkdownRenderer())
}

// TestCodeBlockSingleChunkWorks verifies that a complete code block delivered
// in a single RenderIncremental call renders correctly.
func TestCodeBlockSingleChunkWorks(t *testing.T) {
	t.Parallel()

	s := newTestStreamingRenderer()
	ctx := context.Background()
	opts := RenderOpts{ContentType: ContentTypeStreaming}

	content := "Here is some code:\n\n```go\nfmt.Println(\"hello\")\n```\n\nDone."
	out := s.RenderIncremental(ctx, content, opts)

	// The output should contain the rendered code block content.
	assert.NotEmpty(t, out)
	assert.Contains(t, out, "hello")
}

// TestCodeBlockAcrossChunksComplete verifies that a code block that arrives
// across multiple incremental calls renders correctly once complete. While the
// code block is incomplete, the renderer should not split it across the
// stable/unstable boundary.
func TestCodeBlockAcrossChunksComplete(t *testing.T) {
	t.Parallel()

	s := newTestStreamingRenderer()
	ctx := context.Background()
	opts := RenderOpts{ContentType: ContentTypeStreaming}

	// Simulate streaming: the opening fence arrives but the closing fence
	// has not yet been received.
	incomplete := "Here is code:\n\n```go\nfmt.Println(\"partial\")\n"
	out1 := s.RenderIncremental(ctx, incomplete, opts)
	require.NotEmpty(t, out1)

	// While incomplete, inCodeBlock should be true.
	assert.True(t, s.inCodeBlock, "inCodeBlock should be true while code block is incomplete")

	// Now the closing fence arrives.
	complete := incomplete + "```\n\nDone."
	out2 := s.RenderIncremental(ctx, complete, opts)
	require.NotEmpty(t, out2)

	// After completion, inCodeBlock should be false.
	assert.False(t, s.inCodeBlock, "inCodeBlock should be false after code block completes")

	// The final output should contain the code content.
	assert.Contains(t, out2, "partial")
}

// TestTextOutsideCodeBlockImmediate verifies that text without any code blocks
// renders immediately without buffering.
func TestTextOutsideCodeBlockImmediate(t *testing.T) {
	t.Parallel()

	s := newTestStreamingRenderer()
	ctx := context.Background()
	opts := RenderOpts{ContentType: ContentTypeStreaming}

	content := "Just some plain text.\nNo code blocks here.\nMoving on."
	out := s.RenderIncremental(ctx, content, opts)

	assert.NotEmpty(t, out)
	assert.False(t, s.inCodeBlock, "inCodeBlock should be false for text without code blocks")
}

// TestHasIncompleteCodeBlock verifies the helper method.
func TestHasIncompleteCodeBlock(t *testing.T) {
	t.Parallel()

	s := newTestStreamingRenderer()

	assert.False(t, s.hasIncompleteCodeBlock("no fences"))
	assert.False(t, s.hasIncompleteCodeBlock("```\ncode\n```"))
	assert.True(t, s.hasIncompleteCodeBlock("```\ncode"))
	assert.True(t, s.hasIncompleteCodeBlock("text\n```\ncode\n```\nmore\n```go"))
	assert.False(t, s.hasIncompleteCodeBlock("```\n```\n```\n```"))
}

// TestCodeBlockBoundaryAdjustment verifies that when an incomplete code block
// would straddle the stable/unstable boundary, the boundary is moved back so
// the entire code block is in the unstable region.
func TestCodeBlockBoundaryAdjustment(t *testing.T) {
	t.Parallel()

	s := newTestStreamingRenderer()
	ctx := context.Background()
	opts := RenderOpts{ContentType: ContentTypeStreaming}

	// Build content with enough lines that the stable region would normally
	// include the opening fence. unstableCount is 2, so with 10 lines the
	// stable region is lines[0:8]. We put the opening fence at line 3 so it
	// would be in the stable region without the adjustment.
	lines := []string{
		"line0", // 0
		"line1", // 1
		"line2", // 2
		"```go", // 3 — opening fence (incomplete block)
		"code1", // 4
		"code2", // 5
		"code3", // 6
		"code4", // 7
		"code5", // 8
		"code6", // 9
	}
	content := strings.Join(lines, "\n")

	_ = s.RenderIncremental(ctx, content, opts)

	// The stable count should have been adjusted to 3 (before the fence),
	// not 8 (the default).
	assert.True(t, s.inCodeBlock, "should detect incomplete code block")
	assert.Equal(t, 3, s.stableCount, "stable boundary should be moved to before the opening fence")
}
