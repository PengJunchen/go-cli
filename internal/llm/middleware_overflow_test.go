package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOverflowRecoveryMiddleware_Name(t *testing.T) {
	m := NewOverflowRecoveryMiddleware()
	assert.Equal(t, "overflow", m.Name())
}

func TestOverflowRecoveryMiddleware_NoOverflow_Success(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return &Message{Role: "assistant", Content: "ok"}, nil
		},
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			return nil, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware()
	wrapped := mw.WrapModel(model)

	result, err := wrapped.Generate(t.Context(), []Message{{Role: "user", Content: "hi"}})
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Content)
}

func TestOverflowRecoveryMiddleware_Overflow_RetrySuccess(t *testing.T) {
	calls := 0
	model := &mockModel{
		generateFn: func(_ context.Context, msgs []Message, _ ...Option) (*Message, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("context_length_exceeded: too many tokens")
			}
			return &Message{Role: "assistant", Content: "recovered"}, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(2), WithOverflowTrimRatio(0.5))
	wrapped := mw.WrapModel(model)

	// 4 messages: system + 3 user messages.
	msgs := []Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "msg1"},
		{Role: "user", Content: "msg2"},
		{Role: "user", Content: "msg3"},
	}

	result, err := wrapped.Generate(t.Context(), msgs)
	require.NoError(t, err)
	assert.Equal(t, "recovered", result.Content)
	assert.Equal(t, 2, calls, "should have retried once")

	// Verify the retry had fewer messages (system + 1 trimmed).
	// With trimRatio 0.5 and 3 non-system msgs, trimCount=1.
}

func TestOverflowRecoveryMiddleware_Overflow_AllRetriesFail(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return nil, errors.New("context_length_exceeded")
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(1))
	wrapped := mw.WrapModel(model)

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "1"},
		{Role: "user", Content: "2"},
		{Role: "user", Content: "3"},
		{Role: "user", Content: "4"},
	}

	_, err := wrapped.Generate(t.Context(), msgs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overflow")
	assert.Contains(t, err.Error(), "max retries")
}

func TestOverflowRecoveryMiddleware_NonOverflowError_NoRetry(t *testing.T) {
	calls := 0
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			calls++
			return nil, errors.New("internal server error")
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(3))
	wrapped := mw.WrapModel(model)

	_, err := wrapped.Generate(t.Context(), []Message{{Role: "user", Content: "hi"}})
	require.Error(t, err)
	assert.Equal(t, "internal server error", err.Error())
	assert.Equal(t, 1, calls, "should not retry non-overflow errors")
}

func TestOverflowRecoveryMiddleware_Stream_Overflow_RetrySuccess(t *testing.T) {
	calls := 0
	model := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("context_length_exceeded")
			}
			ch := make(chan MessageChunk, 1)
			ch <- MessageChunk{Content: "recovered", Final: true}
			close(ch)
			return ch, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(2))
	wrapped := mw.WrapModel(model)

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "1"},
		{Role: "user", Content: "2"},
	}

	ch, err := wrapped.Stream(t.Context(), msgs)
	require.NoError(t, err)

	var chunks []string
	for chunk := range ch {
		chunks = append(chunks, chunk.Content)
	}
	assert.Contains(t, strings.Join(chunks, ""), "recovered")
	assert.Equal(t, 2, calls)
}

func TestOverflowRecoveryMiddleware_TrimMessages(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "1"},
		{Role: "user", Content: "2"},
		{Role: "user", Content: "3"},
		{Role: "user", Content: "4"},
	}

	// Trim 50% of non-system messages (2 of 4).
	trimmed := trimMessages(msgs, 0.5)
	assert.Equal(t, 3, len(trimmed), "should have system + 2 remaining")
	assert.Equal(t, "sys", trimmed[0].Content)

	// Trim with single message (no trimming).
	single := []Message{{Role: "user", Content: "only"}}
	assert.Equal(t, single, trimMessages(single, 0.5))

	// Trim with only system message.
	sysOnly := []Message{{Role: "system", Content: "sys"}}
	assert.Equal(t, sysOnly, trimMessages(sysOnly, 0.5))
}

func TestOverflowRecoveryMiddleware_OverflowCount(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return nil, errors.New("context_length_exceeded")
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(1))
	wrapped := mw.WrapModel(model)

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "1"},
		{Role: "user", Content: "2"},
		{Role: "user", Content: "3"},
		{Role: "user", Content: "4"},
	}

	_, _ = wrapped.Generate(t.Context(), msgs) //nolint:errcheck

	mw.mu.Lock()
	count := mw.overflowCount
	mw.mu.Unlock()

	assert.Equal(t, 2, count, "should have detected overflow twice (initial + 1 retry)")
}

func TestIsOverflowError(t *testing.T) {
	assert.True(t, isOverflowError(fmt.Errorf("context_length_exceeded")))
	assert.True(t, isOverflowError(fmt.Errorf("maximum context length exceeded")))
	assert.True(t, isOverflowError(fmt.Errorf("token limit exceeded")))
	assert.False(t, isOverflowError(fmt.Errorf("internal server error")))
	assert.False(t, isOverflowError(nil))
}

func TestOverflowRecoveryMiddleware_AllMessagesTrimmed(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return nil, errors.New("context_length_exceeded")
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(5), WithOverflowTrimRatio(1.0))
	wrapped := mw.WrapModel(model)

	// Single user message (no system to preserve).
	msgs := []Message{{Role: "user", Content: "only"}}

	_, err := wrapped.Generate(t.Context(), msgs)
	require.Error(t, err)
	// Should fail because trimming a single message leaves nothing.
}

func TestOverflowRecoveryMiddleware_Concurrent(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			time.Sleep(10 * time.Millisecond)
			return &Message{Role: "assistant", Content: "ok"}, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware()
	wrapped := mw.WrapModel(model)

	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			_, err := wrapped.Generate(context.Background(), []Message{{Role: "user", Content: "hi"}})
			done <- err
		}()
	}

	for i := 0; i < 10; i++ {
		err := <-done
		assert.NoError(t, err)
	}
}

func TestOverflowLength(t *testing.T) {
	var calls int32
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				return &Message{Role: RoleAssistant, Content: "partial", FinishReason: "length"}, nil
			}
			return &Message{Role: RoleAssistant, Content: " complete", FinishReason: "stop"}, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware()
	wrapped := mw.WrapModel(model)

	result, err := wrapped.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}})
	require.NoError(t, err)
	assert.Equal(t, "partial complete", result.Content)
	assert.Equal(t, "stop", result.FinishReason)
}

func TestOverflowContinue(t *testing.T) {
	var calls int32
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				return &Message{Role: RoleAssistant, Content: "partial", FinishReason: "length"}, nil
			}
			return &Message{Role: RoleAssistant, Content: " complete", FinishReason: "stop"}, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware()
	wrapped := mw.WrapModel(model)

	result, err := wrapped.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}})
	require.NoError(t, err)
	assert.Equal(t, "partial complete", result.Content)
	assert.Equal(t, int32(2), atomic.LoadInt32(&model.generateCalls), "should call Generate twice (initial + 1 continuation)")
}

func TestOverflowMaxContinuation(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return &Message{Role: RoleAssistant, Content: "chunk", FinishReason: "length"}, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware()
	wrapped := mw.WrapModel(model)

	result, err := wrapped.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}})
	require.ErrorContains(t, err, "response remains truncated")
	require.Nil(t, result, "an exhausted length continuation is not a complete response")
	// 1 initial + 3 continuations = 4 total Generate calls.
	assert.Equal(t, int32(4), atomic.LoadInt32(&model.generateCalls))
}

func TestOverflowNoLength(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return &Message{Role: RoleAssistant, Content: "done", FinishReason: "stop"}, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware()
	wrapped := mw.WrapModel(model)

	result, err := wrapped.Generate(context.Background(), []Message{{Role: RoleUser, Content: "hi"}})
	require.NoError(t, err)
	assert.Equal(t, "done", result.Content)
	assert.Equal(t, "stop", result.FinishReason)
	assert.Equal(t, int32(1), atomic.LoadInt32(&model.generateCalls), "should not continue when FinishReason is not length")
}

func TestOverflowLengthStream(t *testing.T) {
	var genCalls int32
	model := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			ch := make(chan MessageChunk, 1)
			ch <- MessageChunk{Role: RoleAssistant, Content: "partial", Final: true, FinishReason: "length"}
			close(ch)
			return ch, nil
		},
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			atomic.AddInt32(&genCalls, 1)
			return &Message{Role: RoleAssistant, Content: " complete", FinishReason: "stop"}, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware()
	wrapped := mw.WrapModel(model)

	ch, err := wrapped.Stream(context.Background(), []Message{{Role: RoleUser, Content: "hi"}})
	require.NoError(t, err)

	var content string
	var finishReason string
	for chunk := range ch {
		content += chunk.Content
		if chunk.Final {
			finishReason = chunk.FinishReason
		}
	}
	assert.Equal(t, "partial complete", content)
	assert.Equal(t, "stop", finishReason)
	assert.Equal(t, int32(1), atomic.LoadInt32(&genCalls), "should call Generate once for continuation")
}

func TestOverflowStreamFragmentationDoesNotConsumePayloadBudget(t *testing.T) {
	logicalText := strings.Repeat("x", 10_000)
	for _, chunked := range []bool{false, true} {
		name := "one_chunk"
		if chunked {
			name = "many_chunks"
		}
		t.Run(name, func(t *testing.T) {
			model := &mockModel{
				streamFn: func(context.Context, []Message, ...Option) (<-chan MessageChunk, error) {
					count := 1
					if chunked {
						count = len(logicalText)
					}
					ch := make(chan MessageChunk, count+1)
					for i := 0; i < count; i++ {
						part := logicalText
						if chunked {
							part = logicalText[i : i+1]
						}
						ch <- MessageChunk{Role: RoleAssistant, Content: part}
					}
					ch <- MessageChunk{Role: RoleAssistant, Final: true, FinishReason: "stop"}
					close(ch)
					return ch, nil
				},
			}
			wrapped := NewOverflowRecoveryMiddleware().WrapModel(model)
			ch, err := wrapped.Stream(context.Background(), nil)
			require.NoError(t, err)
			var content strings.Builder
			var gotErr error
			finalSeen := false
			for chunk := range ch {
				if chunk.Error != nil {
					gotErr = chunk.Error
					continue
				}
				content.WriteString(chunk.Content)
				finalSeen = finalSeen || chunk.Final
			}
			require.NoError(t, gotErr)
			require.True(t, finalSeen)
			require.Equal(t, logicalText, content.String())
		})
	}
}

func TestOverflowStreamCancelsInnerAfterChunkLimitFailure(t *testing.T) {
	innerStopped := make(chan struct{})
	model := &mockModel{
		streamFn: func(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			ch := make(chan MessageChunk)
			go func() {
				defer close(innerStopped)
				defer close(ch)
				for {
					select {
					case ch <- MessageChunk{}:
					case <-ctx.Done():
						return
					}
				}
			}()
			return ch, nil
		},
	}
	wrapped := NewOverflowRecoveryMiddleware().WrapModel(model)
	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	var streamErr error
	for chunk := range ch {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
	}
	require.ErrorContains(t, streamErr, "chunks")
	select {
	case <-innerStopped:
	case <-time.After(time.Second):
		t.Fatal("inner stream did not stop after middleware failure")
	}
}
