package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loopMockModel returns responses in sequence across successive Generate/Stream
// calls. It is used to drive LoopDetectionModelMiddleware tests with either
// varied or repeated content.
type loopMockModel struct {
	responses []string
	idx       int
}

func (m *loopMockModel) nextContent() string {
	content := ""
	if m.idx < len(m.responses) {
		content = m.responses[m.idx]
		m.idx++
	}
	return content
}

func (m *loopMockModel) Generate(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
	return &Message{Role: RoleAssistant, Content: m.nextContent()}, nil
}

func (m *loopMockModel) Stream(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk, 1)
	ch <- MessageChunk{Role: RoleAssistant, Content: m.nextContent(), Final: true}
	close(ch)
	return ch, nil
}

var _ BaseChatModel = (*loopMockModel)(nil)

// TestLoopDetectionModelMiddleware_Name verifies the middleware identifier.
func TestLoopDetectionModelMiddleware_Name(t *testing.T) {
	mw := NewLoopDetectionModelMiddleware()
	assert.Equal(t, "loopdetection", mw.Name())
}

// TestLoopDetectionModelMiddleware_Defaults verifies the default threshold and
// window size are applied.
func TestLoopDetectionModelMiddleware_Defaults(t *testing.T) {
	mw := NewLoopDetectionModelMiddleware()
	assert.Equal(t, 3, mw.threshold)
	assert.Equal(t, 5, mw.windowSize)
}

// TestLoopDetectionModelMiddleware_NoLoop_VariedResponses verifies that varied
// responses never trigger a loop error.
func TestLoopDetectionModelMiddleware_NoLoop_VariedResponses(t *testing.T) {
	base := &loopMockModel{responses: []string{"A", "B", "C", "D", "E"}}
	mw := NewLoopDetectionModelMiddleware()
	wrapped := mw.WrapModel(base)

	for _, expected := range []string{"A", "B", "C", "D", "E"} {
		resp, err := wrapped.Generate(context.Background(), nil)
		require.NoError(t, err)
		assert.Equal(t, expected, resp.Content)
	}
}

// TestLoopDetectionModelMiddleware_LoopDetected_AfterThreshold verifies that
// threshold consecutive identical responses surface an error on the
// threshold-th call.
func TestLoopDetectionModelMiddleware_LoopDetected_AfterThreshold(t *testing.T) {
	base := &loopMockModel{responses: []string{"same", "same", "same"}}
	mw := NewLoopDetectionModelMiddleware() // default threshold=3
	wrapped := mw.WrapModel(base)

	// First two calls succeed.
	for i := 0; i < 2; i++ {
		resp, err := wrapped.Generate(context.Background(), nil)
		require.NoError(t, err, "call %d must not error", i)
		assert.Equal(t, "same", resp.Content)
	}

	// Third consecutive identical response triggers the loop error.
	resp, err := wrapped.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "loopdetection: detected repeated response (count=3)")
}

// TestLoopDetectionModelMiddleware_ResetOnDifferentResponse verifies that a
// different response resets the consecutive counter so no loop is flagged.
func TestLoopDetectionModelMiddleware_ResetOnDifferentResponse(t *testing.T) {
	// A, A, B, A, A: the B resets the run; only two A's follow, below threshold 3.
	base := &loopMockModel{responses: []string{"A", "A", "B", "A", "A"}}
	mw := NewLoopDetectionModelMiddleware() // default threshold=3
	wrapped := mw.WrapModel(base)

	for range []string{"A", "A", "B", "A", "A"} {
		_, err := wrapped.Generate(context.Background(), nil)
		require.NoError(t, err, "no loop should be detected when a different response resets the run")
	}
}

// TestLoopDetectionModelMiddleware_CustomThreshold verifies that a custom
// threshold is honored.
func TestLoopDetectionModelMiddleware_CustomThreshold(t *testing.T) {
	base := &loopMockModel{responses: []string{"x", "x"}}
	mw := NewLoopDetectionModelMiddleware(WithLoopThreshold(2))
	wrapped := mw.WrapModel(base)

	_, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)

	_, err = wrapped.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "count=2")
}

// TestLoopDetectionModelMiddleware_StreamLoopDetected verifies that the stream
// path accumulates chunk content and flags a loop after the threshold.
func TestLoopDetectionModelMiddleware_StreamLoopDetected(t *testing.T) {
	base := &loopMockModel{responses: []string{"same", "same", "same"}}
	mw := NewLoopDetectionModelMiddleware() // default threshold=3
	wrapped := mw.WrapModel(base)

	drain := func() string {
		ch, err := wrapped.Stream(context.Background(), nil)
		require.NoError(t, err)
		var b strings.Builder
		for chunk := range ch {
			b.WriteString(chunk.Content)
		}
		return b.String()
	}

	// First two streams succeed.
	assert.Equal(t, "same", drain())
	assert.Equal(t, "same", drain())

	// Third consecutive stream triggers the loop error from Stream itself.
	ch, err := wrapped.Stream(context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, ch)
	assert.Contains(t, err.Error(), "count=3")
}

// TestLoopDetectionModelMiddleware_ConcurrentSafe verifies the middleware is
// safe under concurrent Generate calls (run with -race).
func TestLoopDetectionModelMiddleware_ConcurrentSafe(t *testing.T) {
	base := &loopMockModel{responses: []string{"z", "z", "z", "z", "z", "z"}}
	mw := NewLoopDetectionModelMiddleware(WithLoopThreshold(100)) // high threshold to avoid early errors
	wrapped := mw.WrapModel(base)

	for i := 0; i < 6; i++ {
		_, _ = wrapped.Generate(context.Background(), nil) //nolint:errcheck
	}
}
