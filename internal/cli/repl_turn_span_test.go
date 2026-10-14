package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func TestDurableTerminalFailureEndsInteractiveTurnSpanAsError(t *testing.T) {
	exporter := &assemblyTraceExporter{}
	tracer := tracing.NewTracer("durable-terminal-failure", exporter)
	tracer.SetEnabled(true)
	_, spanCtx := tracer.Start(context.Background(), "test.root", tracing.SpanKindInternal)
	turnSpan, _ := tracing.SpanFromContext(spanCtx, spanInteractiveTurn, tracing.SpanKindInternal)

	repl := &REPLSession{
		assembly: &AgentAssembly{durableSession: &durableCLISession{}},
		turnResult: core.Result{
			Outcome:        core.RunOutcomeUnknown,
			ExecutionState: core.RunExecutionQuarantined,
			DurablePublication: core.DurablePublication{
				State: core.DurablePublicationPending,
			},
		},
		turnSpan: turnSpan,
	}

	err := repl.finishTurnSpanAfterCompletion(false)
	require.ErrorContains(t, err, "durable run did not reach a known joined terminal state")

	spans := exporter.exportedSpans()
	require.Len(t, spans, 1, "the turn span must be ended exactly once")
	assert.Equal(t, tracing.SpanStatusError, spans[0].Status)
	assert.Contains(t, spans[0].StatusMessage, "durable run did not reach a known joined terminal state")
}

func TestJoinedDurableTurnErrorEndsInteractiveTurnSpanAsError(t *testing.T) {
	exporter := &assemblyTraceExporter{}
	tracer := tracing.NewTracer("joined-durable-turn-error", exporter)
	tracer.SetEnabled(true)
	_, spanCtx := tracer.Start(context.Background(), "test.root", tracing.SpanKindInternal)
	turnSpan, _ := tracing.SpanFromContext(spanCtx, spanInteractiveTurn, tracing.SpanKindInternal)

	repl := &REPLSession{
		assembly: &AgentAssembly{durableSession: &durableCLISession{}},
		turnResult: core.Result{
			Outcome:        core.RunOutcomeError,
			ExecutionState: core.RunExecutionQuiescent,
			DurablePublication: core.DurablePublication{
				State: core.DurablePublicationSynced,
			},
		},
		turnErr:  errors.New("provider failed after durable transcript sync"),
		turnSpan: turnSpan,
	}

	require.NoError(t, repl.finishTurnSpanAfterCompletion(false), "the REPL reports a terminal provider error and continues reading")

	spans := exporter.exportedSpans()
	require.Len(t, spans, 1, "the turn span must be ended exactly once")
	assert.Equal(t, tracing.SpanStatusError, spans[0].Status, "durable refresh does not erase the provider error")
	assert.Equal(t, "provider failed after durable transcript sync", spans[0].StatusMessage)
}
