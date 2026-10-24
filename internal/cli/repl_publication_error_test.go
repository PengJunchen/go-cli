package cli

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestDurablePublicationFailureKeepsTypedCauseAndFatalBoundary(t *testing.T) {
	dispatchErr := &llm.DispatchDeniedError{Code: llm.DeniedSafetyFact}
	for _, test := range []struct {
		name  string
		cause error
	}{
		{name: "tool denial", cause: approval.ErrToolDenied},
		{name: "deadline", cause: context.DeadlineExceeded},
		{name: "dispatch denial", cause: dispatchErr},
		{name: "publication without underlying error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repl := &REPLSession{
				assembly: &AgentAssembly{durableSession: &durableCLISession{}},
				turnResult: core.Result{
					Outcome:        core.RunOutcomeError,
					ExecutionState: core.RunExecutionQuiescent,
					DurablePublication: core.DurablePublication{
						State: core.DurablePublicationFailed,
					},
				},
				turnErr: test.cause,
			}

			err := repl.finishTurnSpanAfterCompletion(false)
			require.ErrorContains(t, err, "without positive transcript publication")
			if test.cause != nil {
				require.ErrorIs(t, err, test.cause)
			}
			if test.cause == dispatchErr {
				var typed *llm.DispatchDeniedError
				require.ErrorAs(t, err, &typed)
				require.Equal(t, llm.DeniedSafetyFact, typed.Code)
			}
			require.Nil(t, repl.unknownCancelRecovery, "an error-chain change must not grant recovery or continuation")
		})
	}
}

func TestUnjoinedDurableTurnKeepsCauseAndRemainsFatal(t *testing.T) {
	repl := &REPLSession{
		assembly: &AgentAssembly{durableSession: &durableCLISession{}},
		turnResult: core.Result{
			Outcome:        core.RunOutcomeUnknown,
			ExecutionState: core.RunExecutionQuarantined,
			DurablePublication: core.DurablePublication{
				State: core.DurablePublicationPending,
			},
		},
		turnErr: context.DeadlineExceeded,
	}

	err := repl.finishTurnSpanAfterCompletion(true)
	require.ErrorContains(t, err, "did not reach a known joined terminal state")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Nil(t, repl.unknownCancelRecovery)
}

type failedPublicationStatusHandle struct {
	status      core.RunStatus
	publication core.DurablePublication
}

func (h failedPublicationStatusHandle) Snapshot() core.RunStatus { return h.status }
func (h failedPublicationStatusHandle) WaitForCompletion(ctx context.Context) (core.RunStatus, core.DurablePublication, error) {
	return h.status, h.publication, ctx.Err()
}

func TestDurableTranscriptRefreshFailureKeepsCauseAndInitialTranscript(t *testing.T) {
	ctx := t.Context()
	initial := projectionSnapshot(nil, nil, nil, "")
	before := initial.Clone()
	transcript, err := ProjectPreparedTranscript(ctx, initial)
	require.NoError(t, err)
	publication := core.DurablePublication{
		State: core.DurablePublicationFailed, RunID: "failed-publication-run",
		SessionID: initial.SessionID, OwnerGeneration: "failed-publication-owner",
	}
	status := core.RunStatus{RunID: publication.RunID, Outcome: core.RunOutcomeError, ExecutionState: core.RunExecutionQuiescent}
	repl := &REPLSession{
		assembly: &AgentAssembly{durableSession: &durableCLISession{}},
		spanCtx:  ctx,
		logger:   slog.Default(),
		out:      io.Discard,
		slashCtx: slashContext{out: io.Discard},
		turnResult: core.Result{
			RunID: publication.RunID, Outcome: status.Outcome, ExecutionState: status.ExecutionState,
			DurablePublication: publication,
			StatusHandle:       failedPublicationStatusHandle{status: status, publication: publication},
		},
		turnErr: context.DeadlineExceeded,
	}
	repl.turnCtx, repl.cancelTurn = context.WithCancel(ctx)
	repl.stream = core.NewEventStream(8)
	require.NoError(t, repl.renderTUI(&transcript))
	t.Cleanup(repl.stopTUI)

	err = repl.applyDurableTurnRefresh(&initial)
	require.ErrorContains(t, err, "without positive transcript publication")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, before, initial, "failed publication must retain the selected initial transcript")
	require.Nil(t, repl.unknownCancelRecovery)
}
