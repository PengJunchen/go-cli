package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// recordingEmitter is a core.HITLQuestionEmitter that records the event and
// optionally answers it.
type recordingEmitter struct {
	mu          sync.Mutex
	events      []HITLQuestionEvent
	answer      HITLAnswer
	autoAnswer  bool
	answerDelay time.Duration
}

func (e *recordingEmitter) Emit(ctx context.Context, event HITLQuestionEvent) error {
	e.mu.Lock()
	e.events = append(e.events, event)
	auto := e.autoAnswer
	ans := e.answer
	delay := e.answerDelay
	e.mu.Unlock()

	if auto {
		go func() {
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return
				}
			}
			select {
			case event.ResponseCh <- ans:
			case <-ctx.Done():
			}
		}()
	}
	return nil
}

func (e *recordingEmitter) lastEvent() HITLQuestionEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.events) == 0 {
		return HITLQuestionEvent{}
	}
	return e.events[len(e.events)-1]
}

func TestHITLTypesRoundTrip(t *testing.T) {
	ch := make(chan HITLAnswer, 1)
	ev := HITLQuestionEvent{QuestionID: "q1", Options: []string{"a", "b"}}
	ch <- HITLAnswer{QuestionID: "q1", Answer: "a"}
	assert.Equal(t, "q1", ev.QuestionID)
	assert.Equal(t, []string{"a", "b"}, ev.Options)
}

func TestAdaptHITLEmitterDeliversAnswer(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	emitter := &recordingEmitter{
		autoAnswer: true,
		answer:     HITLAnswer{Answer: "yes"},
	}
	tool := NewAskUserQuestionTool(emitter, time.Second)
	require.Equal(t, "ask_user", tool.Name())

	res, err := tool.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"question": "continue?", "options": []any{"yes", "no"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "yes", res.Output)

	ev := emitter.lastEvent()
	assert.NotEmpty(t, ev.QuestionID)
	assert.Equal(t, "continue?", ev.Question)
	assert.Equal(t, []string{"yes", "no"}, ev.Options)
}

func TestAdaptHITLEmitterTimeout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Emitter never answers; the tool must time out and the adapter goroutine
	// must exit via context cancellation.
	emitter := &recordingEmitter{autoAnswer: false}
	tool := NewAskUserQuestionTool(emitter, 50*time.Millisecond)

	_, err := tool.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"question": "stuck?"},
	})
	require.Error(t, err)
}

func TestAdaptHITLEmitterAnswerError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	emitter := &recordingEmitter{
		autoAnswer: true,
		answer:     HITLAnswer{Error: errors.New("user dismissed")},
	}
	tool := NewAskUserQuestionTool(emitter, time.Second)

	_, err := tool.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"question": "ok?"},
	})
	require.Error(t, err)
}

func TestAdaptHITLEmitterEmitError(t *testing.T) {
	emitter := &errorEmitter{}
	tool := NewAskUserQuestionTool(emitter, time.Second)

	_, err := tool.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"question": "ok?"},
	})
	require.Error(t, err)
}

type errorEmitter struct{}

func (errorEmitter) Emit(context.Context, HITLQuestionEvent) error {
	return errors.New("emitter unavailable")
}

func TestAdaptHITLEmitterFreeText(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	emitter := &recordingEmitter{
		autoAnswer: true,
		answer:     HITLAnswer{Answer: "free form text"},
	}
	tool := NewAskUserQuestionTool(emitter, time.Second)

	res, err := tool.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"question": "describe it"}, // no options -> free text
	})
	require.NoError(t, err)
	assert.Equal(t, "free form text", res.Output)

	ev := emitter.lastEvent()
	assert.Empty(t, ev.Options, "free-text question must have no options")
}
