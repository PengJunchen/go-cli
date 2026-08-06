package core

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// SystemReminderInjector is an agent-level Middleware that, before each turn,
// collects due system reminders and injects them as leading system messages in
// the submission history.
type SystemReminderInjector struct {
	manager SystemReminderManager
	name    string
}

var _ Middleware = (*SystemReminderInjector)(nil)

// NewSystemReminderInjector builds a middleware backed by manager.
func NewSystemReminderInjector(manager SystemReminderManager) *SystemReminderInjector {
	return &SystemReminderInjector{manager: manager, name: "system-reminder-injector"}
}

// Name returns the middleware identifier.
func (i *SystemReminderInjector) Name() string {
	if i.name == "" {
		return "system-reminder-injector"
	}
	return i.name
}

// Wrap returns a loop-view that injects due reminders before delegating to
// next.
func (i *SystemReminderInjector) Wrap(next AgentLoop) AgentLoop {
	return &reminderInjectorLoop{
		manager: i.manager,
		next:    next,
	}
}

// reminderInjectorLoop is the concrete wrapped loop produced by
// SystemReminderInjector.
type reminderInjectorLoop struct {
	manager SystemReminderManager
	next    AgentLoop
}

// Run collects due reminders, prepends them as system messages to the
// submission history, and delegates to the wrapped loop.
func (l *reminderInjectorLoop) Run(ctx context.Context, submission Submission, stream ...EventStream) ([]AgentEvent, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "middleware.system-reminder-injector", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, nil)

	reminders := l.manager.CheckAndCollect(spanCtx)
	if len(reminders) > 0 {
		sysMsgs := make([]AgentMessage, len(reminders))
		for i, r := range reminders {
			sysMsgs[i] = AgentMessage{Role: "system", Content: r}
		}
		submission.History = append(sysMsgs, submission.History...)
		slog.Info("core.system_reminder.inject", "count", len(reminders))
		logger.Info("system_reminder.inject", "count", len(reminders))
	} else {
		slog.Debug("core.system_reminder.noop", "reminders", 0)
	}

	return l.next.Run(spanCtx, submission, stream...)
}
