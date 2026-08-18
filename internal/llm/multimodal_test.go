package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDataURI is a minimal base64-encoded PNG used by the image encoding tests.
const testDataURI = "data:image/png;base64,iVBORw0KGgo="

// multimodalMessage builds a user message with text + image content blocks.
func multimodalMessage() Message {
	return Message{
		Role: RoleUser,
		ContentBlocks: []ContentBlock{
			{Type: "text", Text: "What is in this image?"},
			{Type: "image_url", ImageURL: &ImageURL{URL: testDataURI}},
		},
	}
}

func TestEncodeOpenAIImage(t *testing.T) {
	msgs := []Message{multimodalMessage()}
	data, err := encodeOpenAIRequest(ModelConfig{Model: "gpt-4o", MaxTokens: 100}, "gpt-4o", msgs, nil)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))

	messages, ok := raw["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 1)

	first, ok := messages[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", first["role"])

	// Content should be an array of parts, not a plain string.
	parts, ok := first["content"].([]any)
	require.True(t, ok, "content should be an array for multimodal messages")
	require.Len(t, parts, 2)

	// First part: text.
	textPart, ok := parts[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "text", textPart["type"])
	assert.Equal(t, "What is in this image?", textPart["text"])

	// Second part: image_url with the data URI passed as-is.
	imgPart, ok := parts[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "image_url", imgPart["type"])
	imgURL, ok := imgPart["image_url"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, testDataURI, imgURL["url"])
}

func TestEncodeClaudeImage(t *testing.T) {
	msgs := []Message{multimodalMessage()}
	data, err := encodeClaudeRequest(ModelConfig{Model: "claude-3-5-sonnet", MaxTokens: 100}, "claude-3-5-sonnet", msgs, nil)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))

	messages, ok := raw["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 1)

	first, ok := messages[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", first["role"])

	blocks, ok := first["content"].([]any)
	require.True(t, ok)
	require.Len(t, blocks, 2)

	// First block: text.
	textBlock, ok := blocks[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "text", textBlock["type"])
	assert.Equal(t, "What is in this image?", textBlock["text"])

	// Second block: image with base64 source.
	imgBlock, ok := blocks[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "image", imgBlock["type"])
	source, ok := imgBlock["source"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "base64", source["type"])
	assert.Equal(t, "image/png", source["media_type"])
	assert.Equal(t, "iVBORw0KGgo=", source["data"])
}

func TestEncodeGeminiImage(t *testing.T) {
	msgs := []Message{multimodalMessage()}
	data, err := encodeGeminiRequest(ModelConfig{Model: "gemini-1.5-flash", MaxTokens: 100}, msgs, nil)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))

	contents, ok := raw["contents"].([]any)
	require.True(t, ok)
	require.Len(t, contents, 1)

	first, ok := contents[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", first["role"])

	parts, ok := first["parts"].([]any)
	require.True(t, ok)
	require.Len(t, parts, 2)

	// First part: text.
	textPart, ok := parts[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "What is in this image?", textPart["text"])

	// Second part: inline_data with mime_type and data.
	imgPart, ok := parts[1].(map[string]any)
	require.True(t, ok)
	inlineData, ok := imgPart["inline_data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "image/png", inlineData["mime_type"])
	assert.Equal(t, "iVBORw0KGgo=", inlineData["data"])
}

func TestEncodeTextOnlyFallback(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: "Hello, world!"},
	}

	// OpenAI: Content should be a plain string, not an array.
	data, err := encodeOpenAIRequest(ModelConfig{Model: "gpt-4o", MaxTokens: 100}, "gpt-4o", msgs, nil)
	require.NoError(t, err)
	var oaiRaw map[string]any
	require.NoError(t, json.Unmarshal(data, &oaiRaw))
	oaiMsgs := oaiRaw["messages"].([]any)
	oaiFirst := oaiMsgs[0].(map[string]any)
	assert.Equal(t, "Hello, world!", oaiFirst["content"])

	// Claude: single text block with the content.
	data, err = encodeClaudeRequest(ModelConfig{Model: "claude-3-5-sonnet", MaxTokens: 100}, "claude-3-5-sonnet", msgs, nil)
	require.NoError(t, err)
	var claudeRaw map[string]any
	require.NoError(t, json.Unmarshal(data, &claudeRaw))
	claudeMsgs := claudeRaw["messages"].([]any)
	claudeFirst := claudeMsgs[0].(map[string]any)
	claudeBlocks := claudeFirst["content"].([]any)
	require.Len(t, claudeBlocks, 1)
	claudeBlock := claudeBlocks[0].(map[string]any)
	assert.Equal(t, "text", claudeBlock["type"])
	assert.Equal(t, "Hello, world!", claudeBlock["text"])

	// Gemini: single text part with the content.
	data, err = encodeGeminiRequest(ModelConfig{Model: "gemini-1.5-flash", MaxTokens: 100}, msgs, nil)
	require.NoError(t, err)
	var geminiRaw map[string]any
	require.NoError(t, json.Unmarshal(data, &geminiRaw))
	geminiContents := geminiRaw["contents"].([]any)
	geminiFirst := geminiContents[0].(map[string]any)
	geminiParts := geminiFirst["parts"].([]any)
	require.Len(t, geminiParts, 1)
	geminiPart := geminiParts[0].(map[string]any)
	assert.Equal(t, "Hello, world!", geminiPart["text"])
}
