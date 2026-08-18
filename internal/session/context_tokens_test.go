package session

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// TestEstimateTokensTextBlocks verifies that text ContentBlocks add their text
// estimate to the entry's token count.
func TestEstimateTokensTextBlocks(t *testing.T) {
	// Content "abcdefgh" = 2 tokens, text block "abcdefgh" = 2 tokens -> 4 total.
	e := SessionEntry{
		Content: "abcdefgh",
		ContentBlocks: []llm.ContentBlock{
			{Type: "text", Text: "abcdefgh"},
		},
	}
	assert.Equal(t, 4, estimateTokensForEntry(e))
}

// TestEstimateTokensImageBlocks verifies that image_url blocks add a fixed 85
// tokens to the entry's token count.
func TestEstimateTokensImageBlocks(t *testing.T) {
	// Content "abcdefgh" = 2 tokens, image block = 85 tokens -> 87 total.
	e := SessionEntry{
		Content: "abcdefgh",
		ContentBlocks: []llm.ContentBlock{
			{Type: "image_url", ImageURL: &llm.ImageURL{URL: "http://example.com/img.png"}},
		},
	}
	assert.Equal(t, 87, estimateTokensForEntry(e))
}

// TestEstimateTokensMixedBlocks verifies that a mix of text and image blocks
// are all accounted for.
func TestEstimateTokensMixedBlocks(t *testing.T) {
	// Content "abcd" = 1 token, text block "abcd" = 1 token, image = 85 -> 87 total.
	e := SessionEntry{
		Content: "abcd",
		ContentBlocks: []llm.ContentBlock{
			{Type: "text", Text: "abcd"},
			{Type: "image_url", ImageURL: &llm.ImageURL{URL: "http://example.com/a.png"}},
			{Type: "image_url", ImageURL: &llm.ImageURL{URL: "http://example.com/b.png"}},
		},
	}
	assert.Equal(t, 1+1+85+85, estimateTokensForEntry(e))
}

// TestEstimateTokensNoBlocksSameAsBefore verifies that an entry without
// ContentBlocks produces the same estimate as the plain estimateTokens function.
func TestEstimateTokensNoBlocksSameAsBefore(t *testing.T) {
	content := "abcdefgh"
	e := SessionEntry{Content: content}
	assert.Equal(t, estimateTokens(content), estimateTokensForEntry(e))
}
