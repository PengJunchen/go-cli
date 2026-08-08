package core

import (
	"context"
	"log/slog"
	"testing"
)

// recordingHandler counts slog records for testing pure functions.
type recordingHandler struct {
	records int
}

func (h *recordingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, _ slog.Record) error {
	h.records++
	return nil
}

func (h *recordingHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(_ string) slog.Handler      { return h }

// TestStringerMethodsArePure verifies that String() methods produce no slog
// side effects (they must be pure functions).
func TestStringerMethodsArePure(t *testing.T) {
	h := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	defer slog.SetDefault(prev)

	// Exercise all String() methods
	_ = SubmissionUserMessage.String()
	_ = SubmissionSteering.String()
	_ = SubmissionFollowUp.String()
	_ = ClassificationAllow.String()
	_ = ClassificationDeny.String()
	_ = ClassificationRequireApproval.String()
	_ = TurnPending.String()
	_ = TurnRunning.String()
	_ = TurnCompleted.String()
	_ = TurnCanceled.String()
	_ = TurnFailed.String()

	if h.records != 0 {
		t.Errorf("String() methods produced %d slog records; expected 0 (pure functions)", h.records)
	}
}

// TestStringerResultsUnchanged verifies that String() return values match
// expected constants.
func TestStringerResultsUnchanged(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"SubmissionUserMessage", SubmissionUserMessage.String(), "user"},
		{"SubmissionSteering", SubmissionSteering.String(), "steering"},
		{"SubmissionFollowUp", SubmissionFollowUp.String(), "followup"},
		{"ClassificationAllow", ClassificationAllow.String(), "allow"},
		{"ClassificationDeny", ClassificationDeny.String(), "deny"},
		{"ClassificationRequireApproval", ClassificationRequireApproval.String(), "require_approval"},
		{"TurnPending", TurnPending.String(), "pending"},
		{"TurnRunning", TurnRunning.String(), "running"},
		{"TurnCompleted", TurnCompleted.String(), "completed"},
		{"TurnCanceled", TurnCanceled.String(), "canceled"},
		{"TurnFailed", TurnFailed.String(), "failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s.String() = %q; want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}
