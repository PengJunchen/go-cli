package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sanitizeMockModel returns a fixed Generate response and a configurable
// sequence of Stream chunks. Used by sanitize middleware tests.
type sanitizeMockModel struct {
	genContent   string
	streamChunks []MessageChunk
}

func (m *sanitizeMockModel) Generate(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
	return &Message{Role: RoleAssistant, Content: m.genContent}, nil
}

func (m *sanitizeMockModel) Stream(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk, len(m.streamChunks))
	for _, c := range m.streamChunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

var _ BaseChatModel = (*sanitizeMockModel)(nil)

// TestSanitizeModelMiddleware_Name verifies the middleware identifier.
func TestSanitizeModelMiddleware_Name(t *testing.T) {
	mw := NewSanitizeModelMiddleware()
	assert.Equal(t, "sanitize", mw.Name())
}

// TestSanitizeModelMiddleware_ClaudeArtifactRemoved verifies that Claude
// <antThinking>...</antThinking> blocks are stripped from the response.
func TestSanitizeModelMiddleware_ClaudeArtifactRemoved(t *testing.T) {
	base := &sanitizeMockModel{genContent: "<antThinking>secret thoughts</antThinking>Hello world"}
	mw := NewSanitizeModelMiddleware()
	wrapped := mw.WrapModel(base)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello world", resp.Content)
}

// TestSanitizeModelMiddleware_GPTMarkerRemoved verifies that GPT watermark
// markers such as 【6bc68d3a】 are stripped from the response.
func TestSanitizeModelMiddleware_GPTMarkerRemoved(t *testing.T) {
	base := &sanitizeMockModel{genContent: "Hello【6bc68d3a】 world"}
	mw := NewSanitizeModelMiddleware()
	wrapped := mw.WrapModel(base)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello world", resp.Content)
}

// TestSanitizeModelMiddleware_GeminiBlockRemoved verifies that Gemini
// ```thinking ... ``` blocks are stripped from the response.
func TestSanitizeModelMiddleware_GeminiBlockRemoved(t *testing.T) {
	base := &sanitizeMockModel{genContent: "Final answer```thinking\ninternal reasoning\n```"}
	mw := NewSanitizeModelMiddleware()
	wrapped := mw.WrapModel(base)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "Final answer", resp.Content)
}

// TestSanitizeModelMiddleware_CleanContentUnchanged verifies that content
// without any artifacts is returned verbatim.
func TestSanitizeModelMiddleware_CleanContentUnchanged(t *testing.T) {
	base := &sanitizeMockModel{genContent: "The quick brown fox jumps over the lazy dog."}
	mw := NewSanitizeModelMiddleware()
	wrapped := mw.WrapModel(base)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "The quick brown fox jumps over the lazy dog.", resp.Content)
}

// TestSanitizeModelMiddleware_StreamSanitized verifies that artifacts are
// stripped from each streamed chunk while inter-chunk spacing is preserved.
func TestSanitizeModelMiddleware_StreamSanitized(t *testing.T) {
	base := &sanitizeMockModel{streamChunks: []MessageChunk{
		{Role: RoleAssistant, Content: "<antThinking>x</antThinking>Hello ", Final: false},
		{Role: RoleAssistant, Content: "World", Final: true},
	}}
	mw := NewSanitizeModelMiddleware()
	wrapped := mw.WrapModel(base)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	var got string
	for chunk := range ch {
		got += chunk.Content
	}
	assert.Equal(t, "Hello World", got)
}
