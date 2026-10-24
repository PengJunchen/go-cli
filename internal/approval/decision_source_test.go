package approval

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tui"
)

func TestTeaApprovalHonorsActualResponseSource(t *testing.T) {
	for _, source := range []string{"user", "canceled", "", "arbitrary-private-reason"} {
		t.Run(source, func(t *testing.T) {
			ch := make(chan tui.ApprovalRequest, 1)
			callback := NewTeaApprovalCallback(ch)
			provider, ok := any(callback).(approvalChoiceProvider)
			require.True(t, ok, "Tea must distinguish real keys from queue cleanup")
			done := make(chan struct{})
			go func() {
				defer close(done)
				req := <-ch
				response := tui.ApprovalResponse{Decision: tui.ApprovalDeny, DecisionSource: source}
				req.ResponseCh <- response
			}()
			result, choice, resolution, err := provider.requestApprovalWithChoice(context.Background(), "write", nil)
			<-done
			require.NoError(t, err)
			assert.Equal(t, ApprovalDeny, result)
			if source == "user" {
				assert.Equal(t, "deny", choice)
				assert.Equal(t, "user", resolution)
			} else {
				assert.NotEqual(t, "user", resolution)
				assert.NotContains(t, resolution, "arbitrary-private-reason")
			}
		})
	}
}

func TestTeaCanceledContextDoesNotClaimUserChoice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	callback := NewTeaApprovalCallback(make(chan tui.ApprovalRequest, 1))
	result, choice, resolution, err := callback.requestApprovalWithChoice(ctx, "write", nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, ApprovalDeny, result)
	assert.Empty(t, choice)
	assert.Equal(t, "canceled", resolution)
}

func TestTeaInvalidResponseDoesNotInventUserChoice(t *testing.T) {
	ch := make(chan tui.ApprovalRequest, 1)
	callback := NewTeaApprovalCallback(ch)
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := <-ch
		req.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalDecision(99), DecisionSource: "user"}
	}()
	result, choice, resolution, err := callback.requestApprovalWithChoice(context.Background(), "write", nil)
	<-done
	require.NoError(t, err)
	assert.Equal(t, ApprovalDeny, result)
	assert.Empty(t, choice)
	assert.Equal(t, "invalid_response", resolution)
}

func TestPreparedDenialMessageAllowsOnlyFixedProvenance(t *testing.T) {
	for _, source := range []string{"user", "callback", "canceled", "non_tty_deny", "input_ended", "invalid_response", "private-value\nsecret"} {
		err := preparedApprovalDenial("deny", source)
		require.ErrorIs(t, err, ErrToolDenied)
		assert.Contains(t, err.Error(), ErrToolDenied.Error(), "existing E2E consumers recognize the historical denial prefix")
		assert.NotContains(t, err.Error(), "private-value")
		assert.NotContains(t, err.Error(), "\n")
		if source == "user" {
			assert.Contains(t, err.Error(), "rejected by the user")
			assert.Contains(t, err.Error(), "decision_source=user")
		}
		if source == "invalid_response" {
			assert.Contains(t, err.Error(), "decision_source=invalid_response; choice=none")
		}
	}
}

func TestPreparedGenericCallbackDenyDoesNotClaimUser(t *testing.T) {
	callback := &MockApprovalCallback{result: ApprovalDeny}
	registry, _, _, _, targets := newPreparedRuntimeFixture(t, callback, NewApprovalCache(""), "target")
	_, err := registry.PrepareToolCall(context.Background(), tools.ToolCall{Name: "target", Args: map[string]any{"path": "a"}})
	require.ErrorIs(t, err, ErrToolDenied)
	assert.Contains(t, err.Error(), "decision_source=callback")
	assert.NotContains(t, err.Error(), "decision_source=user")
	assert.Zero(t, targets["target"].callCount())
}
