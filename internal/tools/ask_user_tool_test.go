package tools

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// fakeEmitter is a tools.HITLQuestionEmitter stub used to drive the tool
// directly without the core adapter.
type fakeEmitter struct {
	answer     HITLAnswer
	autoAnswer bool
	err        error
	captured   HITLQuestionEvent
}

func (e *fakeEmitter) Emit(_ context.Context, event HITLQuestionEvent) error {
	e.captured = event
	if e.err != nil {
		return e.err
	}
	if e.autoAnswer {
		go func() {
			event.ResponseCh <- e.answer
		}()
	}
	return nil
}

func TestAskUserQuestionToolImplementsToolDefinition(t *testing.T) {
	var _ ToolDefinition = (*AskUserQuestionTool)(nil)
}

func TestAskUserQuestionToolNameAndDescription(t *testing.T) {
	tool := NewAskUserQuestionTool(&fakeEmitter{}, 0)
	assert.Equal(t, "ask_user", tool.Name())
	assert.NotEmpty(t, tool.Description())
}

func TestAskUserQuestionToolMultiChoice(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	emitter := &fakeEmitter{
		autoAnswer: true,
		answer:     HITLAnswer{Answer: "red"},
	}
	tool := NewAskUserQuestionTool(emitter, time.Second)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"question": "color?", "options": []any{"red", "green", "blue"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "red", res.Output)
	assert.Equal(t, []string{"red", "green", "blue"}, emitter.captured.Options)
	assert.NotEmpty(t, emitter.captured.QuestionID)
}

func TestAskUserQuestionToolFreeText(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	emitter := &fakeEmitter{
		autoAnswer: true,
		answer:     HITLAnswer{Answer: "because"},
	}
	tool := NewAskUserQuestionTool(emitter, time.Second)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"question": "why?"},
	})
	require.NoError(t, err)
	assert.Equal(t, "because", res.Output)
	assert.Empty(t, emitter.captured.Options)
}

func TestAskUserQuestionToolAnswerError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	emitter := &fakeEmitter{
		autoAnswer: true,
		answer:     HITLAnswer{Error: errors.New("dismissed")},
	}
	tool := NewAskUserQuestionTool(emitter, time.Second)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"question": "ok?"},
	})
	require.Error(t, err)
}

func TestAskUserQuestionToolMissingQuestion(t *testing.T) {
	tool := NewAskUserQuestionTool(&fakeEmitter{}, time.Second)
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	require.Error(t, err)

	_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"question": ""}})
	require.Error(t, err)
}

func TestAskUserQuestionToolEmitError(t *testing.T) {
	emitter := &fakeEmitter{err: errors.New("no ui")}
	tool := NewAskUserQuestionTool(emitter, time.Second)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"question": "ok?"},
	})
	require.Error(t, err)
}

func TestAskUserQuestionToolTimeout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Emitter accepts the event but never answers.
	emitter := &fakeEmitter{autoAnswer: false}
	tool := NewAskUserQuestionTool(emitter, 50*time.Millisecond)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"question": "stuck?"},
	})
	require.Error(t, err)
}

func TestAskUserQuestionToolDefaultTimeout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	emitter := &fakeEmitter{autoAnswer: false}
	// Zero timeout falls back to defaultAskTimeout; use a canceled context to
	// short-circuit rather than waiting the full default.
	tool := NewAskUserQuestionTool(emitter, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"question": "ok?"}})
	require.Error(t, err)
}

func TestToStringSliceAndToInt(t *testing.T) {
	assert.Nil(t, toStringSlice(nil))
	assert.Equal(t, []string{"a", "b"}, toStringSlice([]string{"a", "b"}))
	assert.Equal(t, []string{"a", "b"}, toStringSlice([]any{"a", "b", 3}))
	assert.Equal(t, 5, toInt(5))
	assert.Equal(t, 5, toInt(int64(5)))
	assert.Equal(t, 5, toInt(float64(5)))
	assert.Equal(t, 0, toInt("x"))
}
