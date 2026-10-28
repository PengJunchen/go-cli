package tools

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPositiveCompletionProofContractRejectsAbsenceAndInternalModes(t *testing.T) {
	for _, proof := range []PreparedCompletionProof{CompletionInlinePipelineJoined, CompletionQueuedJobJoined, CompletionOwnedProcessGroupJoined, CompletionMCPRequestJoined, CompletionWorkerRequestJoined} {
		require.True(t, IsPositiveCompletionProof(proof), string(proof))
	}
	for _, proof := range []PreparedCompletionProof{"", "none", "pending", "unknown", "forged-proof", "inline_pipeline_joined ", completionScopedSearchJoined, completionLocalProcessOrPrestartNoProcessJoined} {
		require.False(t, IsPositiveCompletionProof(proof), string(proof))
	}
}
