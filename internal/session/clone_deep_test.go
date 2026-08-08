package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestCloneDeepCopiesImageURL verifies that clone() deep copies the ImageURL
// pointer inside ContentBlocks, producing a distinct allocation.
func TestCloneDeepCopiesImageURL(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	e := &SessionEntry{
		ID:   "x",
		Type: EntryTypeUser,
		ContentBlocks: []llm.ContentBlock{
			{
				Type:     "image_url",
				ImageURL: &llm.ImageURL{URL: "http://example.com/img.png", Detail: "high"},
			},
		},
	}
	cp := e.clone()
	require.NotNil(t, cp)
	require.Len(t, cp.ContentBlocks, 1)
	require.NotNil(t, cp.ContentBlocks[0].ImageURL)
	// The pointer is a different allocation, not the same as the original.
	assert.NotSame(t, e.ContentBlocks[0].ImageURL, cp.ContentBlocks[0].ImageURL)
	// But the values are equal.
	assert.Equal(t, "http://example.com/img.png", cp.ContentBlocks[0].ImageURL.URL)
	assert.Equal(t, "high", cp.ContentBlocks[0].ImageURL.Detail)
}

// TestCloneImageURLIndependent verifies that mutating the clone's ImageURL
// does not affect the original entry's ImageURL.
func TestCloneImageURLIndependent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	e := &SessionEntry{
		ID:   "x",
		Type: EntryTypeUser,
		ContentBlocks: []llm.ContentBlock{
			{
				Type:     "image_url",
				ImageURL: &llm.ImageURL{URL: "http://example.com/img.png", Detail: "high"},
			},
		},
	}
	cp := e.clone()
	require.NotNil(t, cp)
	require.NotNil(t, cp.ContentBlocks[0].ImageURL)

	// Mutate the clone's ImageURL.
	cp.ContentBlocks[0].ImageURL.URL = "http://example.com/changed.png"
	cp.ContentBlocks[0].ImageURL.Detail = "low"

	// The original entry is unaffected.
	assert.Equal(t, "http://example.com/img.png", e.ContentBlocks[0].ImageURL.URL)
	assert.Equal(t, "high", e.ContentBlocks[0].ImageURL.Detail)
}
