package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestAssembledSubAgentModelOverrideUsesPrimaryResolverScope(t *testing.T) {
	const childModel = "unregistered-child-model-name"
	tests := []struct {
		name             string
		responseStatuses []int
		wantAttempts     int
		wantError        bool
	}{
		{name: "successful request", responseStatuses: []int{http.StatusOK}, wantAttempts: 1},
		{name: "transient failure retries then succeeds", responseStatuses: []int{http.StatusServiceUnavailable, http.StatusOK}, wantAttempts: 2},
		{name: "persistent transient failures stop at the operation limit", responseStatuses: []int{http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusServiceUnavailable}, wantAttempts: 3, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primaryCapture := &providerRequestCapture{responseStatuses: tt.responseStatuses}
			primaryServer := httptest.NewServer(http.HandlerFunc(primaryCapture.handler))
			t.Cleanup(primaryServer.Close)
			smallCapture := &providerRequestCapture{}
			smallServer := httptest.NewServer(http.HandlerFunc(smallCapture.handler))
			t.Cleanup(smallServer.Close)

			const primaryProvider = "r3-override-primary"
			const smallProvider = "r3-override-small"
			primaryKey := newTestCredentialCanary(t)
			smallKey := newTestCredentialCanary(t)
			cfg := newAssembleTestConfig(t)
			cfg.Provider = config.ProviderConfig{
				Name:    primaryProvider,
				BaseURL: primaryServer.URL + "/primary",
				APIKey:  primaryKey,
			}
			cfg.SmallModel = config.SmallModelConfig{
				Provider: smallProvider,
				BaseURL:  smallServer.URL + "/small",
				APIKey:   smallKey,
				Model:    "configured-small-model",
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancel)
			assembly, err := AssembleAgent(ctx, cfg, primaryProvider, "assembled-parent-model", io.Discard)
			require.NoError(t, err)
			t.Cleanup(assembly.Cleanup)

			result, err := assembly.Dispatcher.Dispatch(ctx, core.SubagentTask{
				ID:           "r3-override-child",
				Prompt:       "answer using the sub-agent model override",
				SystemPrompt: "r3 assembled override fixture",
				Model:        childModel,
				MaxTurns:     1,
			})
			if tt.wantError {
				require.Error(t, err)
				require.Error(t, result.Error)
			} else {
				require.NoError(t, err)
				require.NoError(t, result.Error)
				assert.Equal(t, "ok", result.Content)
			}

			requests := primaryCapture.snapshotRequests()
			require.Len(t, requests, tt.wantAttempts)
			for _, request := range requests {
				assert.Equal(t, "/primary/chat/completions", request.path)
				assert.Equal(t, "Bearer "+primaryKey, request.authorization)
				assert.Contains(t, request.body, `"model":"`+childModel+`"`)
				assert.NotContains(t, request.body, primaryKey)
				assert.NotContains(t, request.body, smallKey)
			}

			attempts, attemptSummary := assembly.CostTracker.AttemptCostSnapshot()
			require.Len(t, attempts, tt.wantAttempts, "every physical child request must be recorded once")
			assert.Equal(t, tt.wantAttempts, attemptSummary.Attempts)
			assert.Equal(t, tt.wantAttempts, attemptSummary.UnknownUsageAttempts)
			assert.Equal(t, 0, attemptSummary.KnownInputTokens)
			assert.Equal(t, 0, attemptSummary.KnownOutputTokens)
			assert.Equal(t, tt.wantAttempts, attemptSummary.UnknownCostAttempts)

			operationID := attempts[0].Result.OperationID
			require.NotEmpty(t, operationID)
			seenAttemptIDs := make(map[llm.ProviderAttemptID]struct{}, len(attempts))
			successfulAttempts := 0
			for _, attempt := range attempts {
				assert.Equal(t, operationID, attempt.Result.OperationID, "retries belong to the same logical provider operation")
				assert.NotEmpty(t, attempt.Result.AttemptID)
				if _, duplicate := seenAttemptIDs[attempt.Result.AttemptID]; duplicate {
					assert.Fail(t, "physical provider attempt ID must be unique", "attempt ID %q was recorded twice", attempt.Result.AttemptID)
				}
				seenAttemptIDs[attempt.Result.AttemptID] = struct{}{}
				assert.Equal(t, primaryProvider, attempt.Result.Identity.ProviderID)
				assert.Equal(t, childModel, attempt.Result.Identity.ModelID)
				assert.Equal(t, llm.UsageUnknown, attempt.Result.Usage.State, "fixture responses have no provider usage")
				if attempt.Result.Outcome == llm.AttemptSuccess {
					successfulAttempts++
				} else {
					assert.Equal(t, llm.FailureTransientProvider, attempt.Result.Failure)
				}
			}
			if tt.wantError {
				assert.Zero(t, successfulAttempts)
			} else {
				assert.Equal(t, 1, successfulAttempts)
			}

			smallCount, _, smallAuthorization, smallBody := smallCapture.snapshot()
			assert.Zero(t, smallCount, "a bare child model override stays on the Assembly primary provider")
			assert.NotContains(t, smallAuthorization, primaryKey)
			assert.NotContains(t, smallAuthorization, smallKey)
			assert.NotContains(t, smallBody, primaryKey)
			assert.NotContains(t, smallBody, smallKey)
		})
	}
}
