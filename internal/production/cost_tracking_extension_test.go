package production

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestCostTrackerAcceptsExtensionRPCOriginOnlyWithMatchingProtocol(t *testing.T) {
	tracker := NewCostTracker(nil)
	result := attemptResult("sdk-operation", "sdk-attempt", "sdk-model", llm.UnknownUsageEvidence())
	result.Identity.OriginKind = llm.OriginExtensionProcess
	result.Identity.ProtocolID = llm.ProtocolExtensionRPC
	record, err := tracker.RecordAttempt(result)
	require.NoError(t, err)
	require.Equal(t, llm.OriginExtensionProcess, record.Result.Identity.OriginKind)
	require.Equal(t, AttemptCostUnknown, record.Status)
	for _, origin := range []llm.OriginKind{llm.OriginKind(0), llm.OriginKind(255)} {
		invalid := result
		invalid.Identity.OriginKind = origin
		_, err = tracker.RecordAttempt(invalid)
		require.ErrorIs(t, err, ErrInvalidAttemptResult)
	}
	result.Identity.ProtocolID = llm.ProtocolOpenAICompatible
	_, err = tracker.RecordAttempt(result)
	require.ErrorIs(t, err, ErrInvalidAttemptResult)
}
