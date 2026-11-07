package extension_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
)

func TestSDKUsageActualDispatchToTracker(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		known      bool
	}{
		{name: "empty object", body: `{"role":"assistant","content":"ok","usage":{}}`},
		{name: "explicit zero", body: `{"role":"assistant","content":"ok","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`, known: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, result := extension.SDKUsageDispatchFixture(t, tc.body)
			tracker := production.NewCostTracker(nil)
			_, err := tracker.RecordAttempt(result)
			require.NoError(t, err)
			rows, summary := tracker.AttemptCostSnapshot()
			require.Len(t, rows, 1)
			if tc.known {
				require.NotNil(t, message.Usage)
				require.Equal(t, llm.UsageKnown, rows[0].Result.Usage.State)
				require.Equal(t, 1, summary.KnownPrimarySubtotalAttempts)
				require.Zero(t, summary.UnknownUsageAttempts)
				return
			}
			require.Nil(t, message.Usage)
			require.Equal(t, llm.UsageUnknown, rows[0].Result.Usage.State)
			require.Zero(t, summary.KnownPrimarySubtotalAttempts)
			require.Equal(t, 1, summary.UnknownUsageAttempts)
		})
	}
}
