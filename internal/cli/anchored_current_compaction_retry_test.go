package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
)

func TestAnchoredCurrentRunRetryUsesRealUserNotProjection(t *testing.T) {
	agent := core.NewAgentImpl("test", stubLoop{})
	err := agent.SetHistoryFromSourceSnapshot(core.CompactionSourceSnapshot{
		Revision: "fixture-revision", BranchID: "main", SourceEntryIDs: []string{"input", "projection", "assistant"},
		Messages: []core.AgentMessage{
			{Role: "user", Content: "original input", InputKind: core.InputKindUser},
			{Role: "user", Content: compaction.UntrustedSummaryContext(compaction.DurableTruncationMarker), Runtime: llm.RuntimeMetadata{CompactionMarker: true}},
			{Role: "assistant", Content: "done"},
		},
	})
	require.NoError(t, err)
	cmd, output := newTestCmd()
	pending := cmd.handleSlashCommand(context.Background(), session.SlashCommand{Name: "retry"}, &slashContext{out: output, agent: agent})
	require.Equal(t, "original input", pending)

	// Untrusted SetHistory must not let a user-supplied flag hide a real input.
	realUser := compaction.UntrustedSummaryContext("literal user input")
	agent.SetHistory([]core.AgentMessage{
		{Role: "user", Content: realUser, Runtime: llm.RuntimeMetadata{CompactionMarker: true, Source: llm.SourceRef{Kind: llm.SourcePriorPublished}}},
		{Role: "assistant", Content: "done"},
	})
	pending = cmd.handleSlashCommand(context.Background(), session.SlashCommand{Name: "retry"}, &slashContext{out: output, agent: agent})
	require.Equal(t, realUser, pending)
}
