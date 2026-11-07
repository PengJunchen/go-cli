package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestDurableUsageRoundTripPreservesCoverageAndDetailPresence(t *testing.T) {
	ctx := context.Background()
	preparer := NewDurableJSONLSessionPreparer(t.TempDir(), nil)
	prepared, err := preparer.PrepareNew(ctx, "usage provenance round trip")
	require.NoError(t, err)

	userIndex, assistantIndex, assistantOrdinal := uint64(0), uint64(1), uint64(0)
	user, err := prepared.DurableWriter().AppendAndSync(ctx, durableUserRow("usage-user", "fixture input", userIndex))
	require.NoError(t, err)
	want := llm.Usage{
		InputTokens: 120, OutputTokens: 24, TotalTokens: 144,
		PrimaryCoverage:      llm.UsageCoveragePartial,
		CacheReadInputTokens: 0, CacheReadInputTokensReported: true,
		ReasoningOutputTokens: 7, ReasoningOutputTokensReported: true,
	}
	_, err = prepared.DurableWriter().AppendAndSync(ctx, SessionEntry{
		ID: "usage-assistant", ParentID: user[0].ID, Type: EntryTypeAssistant,
		RunID: "run-usage-roundtrip", AssistantTranscriptOrdinal: &assistantOrdinal,
		TranscriptIndex: &assistantIndex, Content: "fixture response", Usage: &want,
	})
	require.NoError(t, err)
	physicalID := prepared.View().SessionID
	require.NoError(t, prepared.Close())

	reopened, err := preparer.PrepareResume(ctx, physicalID)
	require.NoError(t, err)
	defer reopened.Close()
	snapshot, err := reopened.Snapshot()
	require.NoError(t, err)

	var durableUsage *llm.Usage
	for i := range snapshot.DurableEntries {
		if snapshot.DurableEntries[i].ID == "usage-assistant" {
			durableUsage = snapshot.DurableEntries[i].Usage
			break
		}
	}
	require.NotNil(t, durableUsage)
	require.Equal(t, want, *durableUsage)
	require.Len(t, snapshot.PublishedView.Messages, 2)
	require.NotNil(t, snapshot.PublishedView.Messages[1].Usage)
	require.Equal(t, want, *snapshot.PublishedView.Messages[1].Usage)
}
