//go:build darwin || linux

package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

type spoofKnownBashTimeoutError struct{}

func (spoofKnownBashTimeoutError) Error() string        { return "synthetic remote operation deadline" }
func (spoofKnownBashTimeoutError) Unwrap() error        { return context.DeadlineExceeded }
func (spoofKnownBashTimeoutError) OutcomeUnknown() bool { return false }

func TestRunCommitBashTimeoutRejectsGenericKnownMarker(t *testing.T) {
	require.Equal(t, ReceiptOutcomeUnknown, runCommitReceiptOutcome(context.Background(), runCommitDispatchResult{committed: true, isError: true, err: spoofKnownBashTimeoutError{}}))
	require.Equal(t, ReceiptOutcomeUnknown, runCommitReceiptOutcome(context.Background(), runCommitDispatchResult{committed: true, isError: true, err: context.DeadlineExceeded}))
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	require.Equal(t, ReceiptOutcomeUnknown, runCommitReceiptOutcome(parent, runCommitDispatchResult{committed: true, isError: true, err: context.DeadlineExceeded, knownBashOwnTimeout: true}), "caller cancellation wins even over a formerly validated local timeout")
}

func TestLegacyReceiptRejectsBorrowedGenuinePreparedBashTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base := tools.NewDefaultToolRegistry()
	require.NoError(t, tools.RegisterDefaults(ctx, base, tools.WithRegisteredBuiltinWhitelist([]string{"bash"}), tools.WithRegisteredBashSandbox(tools.AllowAllSandbox{}), tools.WithRegisteredBashTimeout(300*time.Millisecond)))
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{ID: "genuine-but-not-legacy-authority", Name: "bash", Args: map[string]any{"command": "printf borrowed-authority-started; exec sleep 5"}})
	require.NoError(t, err)
	result, dispatchErr := invocation.Dispatch(ctx)
	require.NotNil(t, result)
	require.Contains(t, result.Output, "borrowed-authority-started")
	require.ErrorIs(t, dispatchErr, context.DeadlineExceeded)
	require.True(t, tools.PreparedBashOwnTimeoutKnown(invocation, dispatchErr))
	require.NoError(t, ctx.Err())
	require.Equal(t, ReceiptOutcomeUnknown, toolReceiptOutcome(ctx, dispatchErr, true), "legacy has no current sealed invocation/action to bind the borrowed genuine error")
}

func TestFailedReceiptOutputBoundPreservesCauseAndJSONMargin(t *testing.T) {
	for _, partial := range []string{strings.Repeat("\x00", 200000), strings.Repeat("界", 100000)} {
		output, clipped := boundedToolFailureOutput(partial, errors.New("synthetic known failure"))
		require.True(t, clipped)
		require.LessOrEqual(t, len(output), 100000)
		require.True(t, utf8.ValidString(output))
		require.True(t, strings.HasPrefix(output, "Error: synthetic known failure\n"))
		require.Contains(t, output, "[partial output truncated for receipt]")
		encoded, err := json.Marshal(map[string]string{"content": output})
		require.NoError(t, err)
		require.Less(t, len(encoded), 1<<20, "even worst-case six-byte JSON escaping leaves room for ordinary receipt fields; unbounded generic metadata is not guaranteed")
	}
}
