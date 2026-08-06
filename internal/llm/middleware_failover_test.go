package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fbMockModel is a configurable BaseChatModel for failover tests. Each field
// controls the corresponding behavior; call counters record how many times
// Generate/Stream were invoked.
type fbMockModel struct {
	genContent  string
	genErr      error
	streamErr   error
	streamText  string
	genCalls    int
	streamCalls int
}

func (m *fbMockModel) Generate(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
	m.genCalls++
	if m.genErr != nil {
		return nil, m.genErr
	}
	return &Message{Role: RoleAssistant, Content: m.genContent}, nil
}

func (m *fbMockModel) Stream(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	m.streamCalls++
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	ch := make(chan MessageChunk, 1)
	ch <- MessageChunk{Role: RoleAssistant, Content: m.streamText, Final: true}
	close(ch)
	return ch, nil
}

var _ BaseChatModel = (*fbMockModel)(nil)

// TestFailoverModelMiddleware_Name verifies the middleware identifier.
func TestFailoverModelMiddleware_Name(t *testing.T) {
	mw := NewFailoverModelMiddleware()
	assert.Equal(t, "failover", mw.Name())
}

// TestFailoverModelMiddleware_PrimarySuccess verifies that when the primary
// model succeeds, no fallback is consulted.
func TestFailoverModelMiddleware_PrimarySuccess(t *testing.T) {
	primary := &fbMockModel{genContent: "primary-ok", streamText: "primary-stream"}
	fb1 := &fbMockModel{genContent: "fb1"}
	mw := NewFailoverModelMiddleware(WithFallbackModels(fb1))
	wrapped := mw.WrapModel(primary)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "primary-ok", resp.Content)
	assert.Equal(t, 1, primary.genCalls)
	assert.Equal(t, 0, fb1.genCalls, "fallback must not be called when primary succeeds")
}

// TestFailoverModelMiddleware_PrimaryFail_FallbackSuccess verifies that a
// failing primary is followed by fallbacks in order until one succeeds.
func TestFailoverModelMiddleware_PrimaryFail_FallbackSuccess(t *testing.T) {
	primary := &fbMockModel{genErr: errors.New("primary down")}
	fb1 := &fbMockModel{genErr: errors.New("fb1 down")}
	fb2 := &fbMockModel{genContent: "fb2-ok"}
	mw := NewFailoverModelMiddleware(WithFallbackModels(fb1, fb2))
	wrapped := mw.WrapModel(primary)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "fb2-ok", resp.Content)
	assert.Equal(t, 1, primary.genCalls)
	assert.Equal(t, 1, fb1.genCalls)
	assert.Equal(t, 1, fb2.genCalls)
}

// TestFailoverModelMiddleware_AllFail verifies that when every model fails the
// last error is returned.
func TestFailoverModelMiddleware_AllFail(t *testing.T) {
	errPrimary := errors.New("primary down")
	errFB := errors.New("fb down")
	primary := &fbMockModel{genErr: errPrimary}
	fb1 := &fbMockModel{genErr: errFB}
	mw := NewFailoverModelMiddleware(WithFallbackModels(fb1))
	wrapped := mw.WrapModel(primary)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, errFB, err, "last error must be returned when all models fail")
}

// TestFailoverModelMiddleware_NoFallbacks verifies that with no fallbacks
// configured, a failing primary simply returns its error.
func TestFailoverModelMiddleware_NoFallbacks(t *testing.T) {
	primary := &fbMockModel{genErr: errors.New("primary down")}
	mw := NewFailoverModelMiddleware()
	wrapped := mw.WrapModel(primary)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, 1, primary.genCalls)
}

// TestFailoverModelMiddleware_StreamFailover verifies that a failing primary
// Stream call falls back to a fallback model's stream.
func TestFailoverModelMiddleware_StreamFailover(t *testing.T) {
	primary := &fbMockModel{streamErr: errors.New("stream primary down")}
	fb1 := &fbMockModel{streamText: "fb-stream-ok"}
	mw := NewFailoverModelMiddleware(WithFallbackModels(fb1))
	wrapped := mw.WrapModel(primary)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	var got string
	for chunk := range ch {
		got += chunk.Content
	}
	assert.Equal(t, "fb-stream-ok", got)
	assert.Equal(t, 1, primary.streamCalls)
	assert.Equal(t, 1, fb1.streamCalls)
}

// TestFailoverModelMiddleware_StreamAllFail verifies that when every Stream
// call fails the last error is returned and no channel is produced.
func TestFailoverModelMiddleware_StreamAllFail(t *testing.T) {
	primary := &fbMockModel{streamErr: errors.New("stream primary down")}
	fb1 := &fbMockModel{streamErr: errors.New("stream fb down")}
	mw := NewFailoverModelMiddleware(WithFallbackModels(fb1))
	wrapped := mw.WrapModel(primary)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, ch)
}
