package cli

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
)

func classifiedDiagnosticCode(t *testing.T, classified *UserFriendlyError) string {
	t.Helper()
	field := reflect.ValueOf(classified).Elem().FieldByName("DiagnosticCode")
	if !field.IsValid() || field.Kind() != reflect.String {
		t.Error("classification must expose a closed diagnostic code")
		return ""
	}
	return field.String()
}

func TestClassifyLocalBudgetErrorsPreserveTypedChainsAndPrivacy(t *testing.T) {
	const canary = "LOCAL-BUDGET-PRIVATE-ERROR-CANARY"
	leaf := errors.New(canary + " leaf")
	envelope := &core.RequestEnvelopeError{Reason: canary + " DNS reason", Cause: leaf}
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{"mandatory", fmt.Errorf("%s: %w", canary, compaction.ErrMandatorySystemPromptOverBudget), "mandatory_system_prompt_over_budget"},
		{"envelope", fmt.Errorf("%s: %w", canary, envelope), "request_envelope_rejected"},
		{"joined prefers mandatory", errors.Join(envelope, compaction.ErrMandatorySystemPromptOverBudget), "mandatory_system_prompt_over_budget"},
	} {
		t.Run(test.name, func(t *testing.T) {
			classified := classifyError(test.err)
			require.NotNil(t, classified)
			require.Same(t, test.err, classified.Err)
			require.ErrorIs(t, classified, test.err)
			require.Equal(t, test.code, classifiedDiagnosticCode(t, classified))
			safe := classified.Action + "\n" + classified.Hint
			require.NotContains(t, safe, canary)
			require.NotContains(t, classified.Hint, "Try again.")
			if test.code == "mandatory_system_prompt_over_budget" {
				require.Contains(t, strings.ToLower(classified.Action), "mandatory")
				require.Contains(t, classified.Hint, "--max-tokens")
				require.Contains(t, classified.Hint, "compaction.max_tokens")
				require.Contains(t, classified.Hint, "provider output")
				require.ErrorIs(t, classified, compaction.ErrMandatorySystemPromptOverBudget)
			} else {
				require.Contains(t, classified.Action, "before provider")
				require.ErrorIs(t, classified, core.ErrRequestEnvelopeRejected)
				require.ErrorIs(t, classified, leaf)
				var actual *core.RequestEnvelopeError
				require.ErrorAs(t, classified, &actual)
				require.Same(t, envelope, actual)
			}
		})
	}
}

func TestClassifyLocalBudgetTextDoesNotInventTypedCode(t *testing.T) {
	err := errors.New("core: request envelope preflight rejected: budget PRIVATE-TEXT-CANARY")
	classified := classifyError(err)
	require.NotNil(t, classified)
	require.Equal(t, "operation failed", classified.Action)
	require.Empty(t, classifiedDiagnosticCode(t, classified))
}
