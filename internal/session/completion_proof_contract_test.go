package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestDurableCompletionProofSeparatesEmptyObservationFromPositiveEnum(t *testing.T) {
	require.True(t, validCompletionProof(""), "durable execution observation can omit positive proof")
	require.False(t, tools.IsPositiveCompletionProof(""), "absence must never resolve a pending action")
	for _, proof := range []tools.PreparedCompletionProof{tools.CompletionInlinePipelineJoined, tools.CompletionQueuedJobJoined, tools.CompletionOwnedProcessGroupJoined, tools.CompletionMCPRequestJoined, tools.CompletionWorkerRequestJoined} {
		require.True(t, validCompletionProof(proof), string(proof))
	}
	for _, proof := range []tools.PreparedCompletionProof{"none", "pending", "unknown", "forged-proof", "scoped_search_joined", "local_process_or_prestart_no_process_joined"} {
		require.False(t, validCompletionProof(proof), string(proof))
	}
}
