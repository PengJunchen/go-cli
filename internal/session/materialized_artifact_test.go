package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

func TestMaterializedArtifactDigestSurvivesDurableReceiptProjection(t *testing.T) {
	message := core.AgentMessage{Role: "tool", ToolCallID: "load", ToolName: "skill_load", Content: "instructions", ReceiptOutcome: core.ReceiptOutcomeSuccess, MaterializedArtifactDigest: strings.Repeat("b", 64)}
	entry := sessionEntryFromRunCommitMessage(message, EntryTypeTool)
	entry.RecordVersion = 1
	projected, ok := sessionEntryToAgentMessage(*entry.clone())
	require.True(t, ok)
	require.Equal(t, message.MaterializedArtifactDigest, entry.MaterializedArtifactDigest)
	require.Equal(t, message, projected)
	require.True(t, durableRowMatchesMessage(entry, message))
}
