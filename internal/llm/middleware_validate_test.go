package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateModelMiddleware_Name verifies the middleware identifier.
func TestValidateModelMiddleware_Name(t *testing.T) {
	mw := NewValidateModelMiddleware()
	assert.Equal(t, "validate", mw.Name())
}

// TestValidateModelMiddleware_GenerateNilResponse verifies that a nil message
// returns an error.
func TestValidateModelMiddleware_GenerateNilResponse(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return nil, nil
		},
	}
	mw := NewValidateModelMiddleware()
	wrapped := mw.WrapModel(model)

	_, err := wrapped.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errValidateEmptyResponse)
}

// TestValidateModelMiddleware_GenerateEmptyContent verifies that a message with
// empty content and no tool calls returns an error.
func TestValidateModelMiddleware_GenerateEmptyContent(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return &Message{Role: RoleAssistant}, nil
		},
	}
	mw := NewValidateModelMiddleware()
	wrapped := mw.WrapModel(model)

	_, err := wrapped.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errValidateEmptyContent)
}

// TestValidateModelMiddleware_GenerateValidResponse verifies that a valid
// message with content passes validation.
func TestValidateModelMiddleware_GenerateValidResponse(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return &Message{Role: RoleAssistant, Content: "hello"}, nil
		},
	}
	mw := NewValidateModelMiddleware()
	wrapped := mw.WrapModel(model)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "hello", resp.Content)
}

// TestValidateModelMiddleware_GenerateValidToolCalls verifies that a message
// with only tool calls (no content) passes validation.
func TestValidateModelMiddleware_GenerateValidToolCalls(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return &Message{
				Role:      RoleAssistant,
				ToolCalls: []ToolCall{{ID: "1", Name: "do_thing"}},
			}, nil
		},
	}
	mw := NewValidateModelMiddleware()
	wrapped := mw.WrapModel(model)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Len(t, resp.ToolCalls, 1)
}

// TestValidateModelMiddleware_GeneratePropagatesError verifies that errors from
// the underlying model are passed through unchanged.
func TestValidateModelMiddleware_GeneratePropagatesError(t *testing.T) {
	errSentinel := errors.New("model error")
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return nil, errSentinel
		},
	}
	mw := NewValidateModelMiddleware()
	wrapped := mw.WrapModel(model)

	_, err := wrapped.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errSentinel)
}

// TestValidateModelMiddleware_StreamEmpty verifies that an empty stream returns
// an error.
func TestValidateModelMiddleware_StreamEmpty(t *testing.T) {
	model := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			ch := make(chan MessageChunk)
			close(ch)
			return ch, nil
		},
	}
	mw := NewValidateModelMiddleware()
	wrapped := mw.WrapModel(model)

	_, err := wrapped.Stream(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errValidateEmptyStream)
}

// TestValidateModelMiddleware_StreamValid verifies that a non-empty stream is
// forwarded correctly.
func TestValidateModelMiddleware_StreamValid(t *testing.T) {
	model := &mockModel{}
	mw := NewValidateModelMiddleware()
	wrapped := mw.WrapModel(model)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var content string
	for chunk := range ch {
		content += chunk.Content
	}
	assert.Equal(t, "ok", content)
}

// TestValidateModelMiddleware_StreamMultipleChunks verifies that multiple chunks
// are all forwarded.
func TestValidateModelMiddleware_StreamMultipleChunks(t *testing.T) {
	model := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			ch := make(chan MessageChunk, 3)
			ch <- MessageChunk{Role: RoleAssistant, Content: "a"}
			ch <- MessageChunk{Role: RoleAssistant, Content: "b"}
			ch <- MessageChunk{Role: RoleAssistant, Content: "c", Final: true}
			close(ch)
			return ch, nil
		},
	}
	mw := NewValidateModelMiddleware()
	wrapped := mw.WrapModel(model)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var content string
	for chunk := range ch {
		content += chunk.Content
	}
	assert.Equal(t, "abc", content)
}

// TestValidateModelMiddleware_StreamPropagatesError verifies that errors from
// the initial Stream call are passed through.
func TestValidateModelMiddleware_StreamPropagatesError(t *testing.T) {
	errSentinel := errors.New("stream error")
	model := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			return nil, errSentinel
		},
	}
	mw := NewValidateModelMiddleware()
	wrapped := mw.WrapModel(model)

	_, err := wrapped.Stream(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errSentinel)
}
