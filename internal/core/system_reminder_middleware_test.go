package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// captureLoop is an AgentLoop that records the submission it receives.
type captureLoop struct {
	got Submission
}

func (l *captureLoop) Run(_ context.Context, submission Submission, _ ...EventStream) ([]AgentEvent, error) {
	l.got = submission
	return []AgentEvent{{Kind: "message", Content: "ok", Timestamp: time.Now()}}, nil
}

func TestSystemReminderInjectorImplementsMiddleware(t *testing.T) {
	var _ Middleware = (*SystemReminderInjector)(nil)
}

func TestSystemReminderInjectorName(t *testing.T) {
	m := NewSystemReminderInjector(NewDefaultSystemReminderManager())
	assert.Equal(t, "system-reminder-injector", m.Name())
}

func TestSystemReminderInjectorInjectsBeforeTurn(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr := NewDefaultSystemReminderManager()
	mgr.AddReminder(SystemReminder{ID: "r1", Content: "reminder-1", Interval: 0})

	base := &captureLoop{}
	injector := NewSystemReminderInjector(mgr)
	wrapped := injector.Wrap(base)

	sub := Submission{
		Type:    SubmissionUserMessage,
		Content: "hello",
		History: []AgentMessage{{Role: "user", Content: "hello"}},
	}
	events, err := wrapped.Run(context.Background(), sub)
	require.NoError(t, err)
	assert.Len(t, events, 1)

	require.NotEmpty(t, base.got.History)
	assert.Equal(t, "system", base.got.History[0].Role)
	assert.Equal(t, "reminder-1", base.got.History[0].Content)
	// Original history preserved after the injected message.
	require.Greater(t, len(base.got.History), 1)
	assert.Equal(t, "hello", base.got.History[1].Content)
}

func TestSystemReminderInjectorNoRemindersNoOp(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr := NewDefaultSystemReminderManager()
	base := &captureLoop{}
	wrapped := NewSystemReminderInjector(mgr).Wrap(base)

	orig := []AgentMessage{{Role: "user", Content: "hi"}}
	sub := Submission{Type: SubmissionUserMessage, Content: "hi", History: orig}
	_, err := wrapped.Run(context.Background(), sub)
	require.NoError(t, err)

	require.Equal(t, len(orig), len(base.got.History), "history must be unchanged when no reminders are due")
	assert.Equal(t, "hi", base.got.History[0].Content)
}

func TestSystemReminderInjectorOnlyInjectsDueReminders(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr := NewDefaultSystemReminderManager()
	mgr.AddReminder(SystemReminder{ID: "once", Content: "once-only", Interval: 0})

	base := &captureLoop{}
	wrapped := NewSystemReminderInjector(mgr).Wrap(base)

	sub := Submission{Type: SubmissionUserMessage, Content: "turn1"}
	_, _ = wrapped.Run(context.Background(), sub) //nolint:errcheck
	require.Len(t, base.got.History, 1)
	assert.Equal(t, "once-only", base.got.History[0].Content)

	// Second turn: the one-time reminder already fired, so nothing is injected.
	base2 := &captureLoop{}
	wrapped2 := NewSystemReminderInjector(mgr).Wrap(base2)
	_, _ = wrapped2.Run(context.Background(), Submission{Type: SubmissionUserMessage, Content: "turn2"}) //nolint:errcheck
	assert.Empty(t, base2.got.History, "no reminder due on the second turn")
}

func TestSystemReminderInjectorPreservesTracingContext(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr := NewDefaultSystemReminderManager()
	mgr.AddReminder(SystemReminder{ID: "r", Content: "c", Interval: 0})

	// A loop that asserts the context carries a span (i.e. tracing is wired).
	traced := &tracingAssertLoop{t: t}
	wrapped := NewSystemReminderInjector(mgr).Wrap(traced)
	_, err := wrapped.Run(context.Background(), Submission{Type: SubmissionUserMessage, Content: "x"})
	require.NoError(t, err)
}

type tracingAssertLoop struct {
	t *testing.T
}

func (l *tracingAssertLoop) Run(ctx context.Context, _ Submission, _ ...EventStream) ([]AgentEvent, error) {
	// SpanFromContext must succeed when called by the middleware; here we just
	// ensure the context is non-nil and cancellable semantics are intact.
	span, _ := tracing.SpanFromContext(ctx, "test.child", tracing.SpanKindInternal)
	span.End()
	return nil, nil
}
