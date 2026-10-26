//go:build !headless

package tui

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

const (
	testAssistantGenerationStarted = "assistant_generation_started"
	testAssistantGenerationEnded   = "assistant_generation_finished"
	testAssistantCandidateMarker   = "R8_CANDIDATE_MUST_STAY_HIDDEN"
	testAssistantProgressLabel     = "Assistant response is being generated and checked"
)

func TestCoreGenerationLifecycleBridgeDropsContentAndKeepsOccurrence(t *testing.T) {
	index := uint64(17)
	bridged := CoreEventToAgentEvent(core.AgentEvent{
		Kind:                     testAssistantGenerationStarted,
		Content:                  testAssistantCandidateMarker,
		RunID:                    "run-progress-1",
		CanonicalTranscriptIndex: &index,
	})

	require.Equal(t, testAssistantGenerationStarted, bridged.Type)
	require.Empty(t, bridged.Content, "lifecycle events must not carry a provider candidate")
	require.Equal(t, "run-progress-1", bridged.RunID)
	require.NotNil(t, bridged.CanonicalTranscriptIndex)
	require.Equal(t, index, *bridged.CanonicalTranscriptIndex)
	*bridged.CanonicalTranscriptIndex = 99
	require.Equal(t, uint64(17), index, "the bridge must preserve its existing ownership-copy contract")
}

func TestAssistantGenerationProgressIsContentFreeAndRunScoped(t *testing.T) {
	app := NewBubbleteaApp(nil, WithWidth(100), WithTurnCount(1))
	oldIndex := uint64(2)
	newIndex := uint64(5)

	app.model.handleEvent(AgentEvent{
		Type:                     testAssistantGenerationStarted,
		Content:                  testAssistantCandidateMarker,
		RunID:                    "run-old",
		CanonicalTranscriptIndex: &oldIndex,
	})
	require.Contains(t, app.View(), testAssistantProgressLabel)
	require.NotContains(t, app.View(), testAssistantCandidateMarker)

	app.model.handleEvent(AgentEvent{
		Type:                     testAssistantGenerationStarted,
		Content:                  testAssistantCandidateMarker,
		RunID:                    "run-new",
		CanonicalTranscriptIndex: &newIndex,
	})
	require.Contains(t, app.View(), testAssistantProgressLabel)

	app.model.handleEvent(AgentEvent{
		Type:                     testAssistantGenerationEnded,
		Content:                  testAssistantCandidateMarker,
		RunID:                    "run-old",
		CanonicalTranscriptIndex: &oldIndex,
	})
	require.Contains(t, app.View(), testAssistantProgressLabel, "a late older-run terminal event cannot clear a newer run")
	require.NotContains(t, app.View(), testAssistantCandidateMarker)
	require.Zero(t, app.model.accordion.Len(), "progress state must not create a transcript/accordion row")

	app.model.handleEvent(AgentEvent{
		Type:                     testAssistantGenerationEnded,
		Content:                  testAssistantCandidateMarker,
		RunID:                    "run-new",
		CanonicalTranscriptIndex: &newIndex,
	})
	require.NotContains(t, app.View(), testAssistantProgressLabel)
	require.NotContains(t, app.View(), testAssistantCandidateMarker)
	require.Zero(t, app.model.accordion.Len())
}

func TestAssistantGenerationProgressRequiresOccurrenceIdentity(t *testing.T) {
	app := NewBubbleteaApp(nil, WithWidth(100), WithTurnCount(1))
	index := uint64(1)

	for _, event := range []AgentEvent{
		{Type: testAssistantGenerationStarted, Content: testAssistantCandidateMarker},
		{Type: testAssistantGenerationStarted, RunID: "run-without-index"},
		{Type: testAssistantGenerationStarted, CanonicalTranscriptIndex: &index},
	} {
		app.model.handleEvent(event)
		require.NotContains(t, app.View(), testAssistantProgressLabel)
		require.NotContains(t, app.View(), testAssistantCandidateMarker)
		require.Zero(t, app.model.accordion.Len())
	}
}

func TestAssistantGenerationProgressClearsWhenEventSourceCloses(t *testing.T) {
	app := NewBubbleteaApp(nil, WithWidth(100), WithTurnCount(1))
	index := uint64(6)
	app.model.handleEvent(AgentEvent{
		Type:                     testAssistantGenerationStarted,
		RunID:                    "run-source-closed",
		CanonicalTranscriptIndex: &index,
	})
	require.Contains(t, app.View(), testAssistantProgressLabel)

	app.model.Update(agentEventStreamClosedMsg{})

	require.NotContains(t, app.View(), testAssistantProgressLabel)
	require.Zero(t, app.model.accordion.Len(), "event-source cleanup must not promote activity into the transcript")
}

func TestAssistantGenerationProgressRendersInCanonicalTranscriptView(t *testing.T) {
	app, err := NewBubbleteaAppWithTranscript(nil, CanonicalTranscriptSnapshot{
		PhysicalSessionID: "session-progress",
		BranchID:          "branch-progress",
		Revision:          "revision-progress",
	}, WithWidth(80), WithTurnCount(1))
	require.NoError(t, err)
	index := uint64(4)
	app.model.handleEvent(CoreEventToAgentEvent(core.AgentEvent{
		Kind:                     testAssistantGenerationStarted,
		Content:                  testAssistantCandidateMarker,
		RunID:                    "run-progress-canonical",
		CanonicalTranscriptIndex: &index,
	}))

	view := app.View()
	require.Contains(t, view, testAssistantProgressLabel)
	require.NotContains(t, view, testAssistantCandidateMarker)
	require.Empty(t, app.model.transcriptViewport.rows, "ephemeral activity must not create a canonical transcript row")
	app.model.handleEvent(CoreEventToAgentEvent(core.AgentEvent{
		Kind:                     testAssistantGenerationEnded,
		RunID:                    "run-progress-canonical",
		CanonicalTranscriptIndex: &index,
	}))
	require.NotContains(t, app.View(), testAssistantProgressLabel)
}
