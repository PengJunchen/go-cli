package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/pengjunchen/go-cli/internal/llm"
)

type durableCompactionAgentLoop struct{}

func (durableCompactionAgentLoop) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	return nil, nil
}

func (durableCompactionAgentLoop) RunCanonical(_ context.Context, submission Submission, _ ...EventStream) (CanonicalRunResult, error) {
	input := submission.History[len(submission.History)-1]
	return CanonicalRunResult{Transcript: []TranscriptEntry{
		{Message: input},
		{Message: AgentMessage{Role: "assistant", Content: "done"}},
	}}, nil
}

func TestSetHistoryFromSourceSnapshotCarriesExactPublishedSourcesAndReceipts(t *testing.T) {
	agent := NewAgentImpl("durable", durableCompactionAgentLoop{})
	snapshot := CompactionSourceSnapshot{
		Revision: "revision-4", BranchID: "branch-2",
		Messages: []AgentMessage{
			{Role: "user", Content: "prior"},
			{Role: "tool", Content: "uncertain", ToolCallID: "call-1", ToolName: "read", ReceiptOutcome: ReceiptOutcomeUnknown,
				Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: "stale-run", Through: llm.SourceThrough{PriorHistoryCount: 1}}}},
		},
		SourceEntryIDs: []string{"entry-4", "entry-9"},
	}
	if err := agent.SetHistoryFromSourceSnapshot(snapshot); err != nil {
		t.Fatalf("hydrate durable source snapshot: %v", err)
	}
	history := agent.Messages()
	if len(history) != 2 {
		t.Fatalf("history length = %d, want 2", len(history))
	}
	for i, wantID := range snapshot.SourceEntryIDs {
		source := history[i].Runtime.Source
		if source.Kind != llm.SourcePriorPublished || source.HistoryRevision != snapshot.Revision || source.BranchID != snapshot.BranchID || source.SourceEntryID != wantID || source.PriorOrdinal != uint64(i) {
			t.Fatalf("history[%d] source = %#v", i, source)
		}
		if source.RunID != "" || source.Through != (llm.SourceThrough{}) {
			t.Fatalf("history[%d] retained stale run cursor: %#v", i, source)
		}
	}
	if history[1].Runtime.ReceiptOutcome != ReceiptOutcomeUnknown || history[1].ReceiptOutcome != ReceiptOutcomeUnknown {
		t.Fatalf("canonical unknown receipt was not preserved: %#v", history[1])
	}
	if history[0].Runtime.Source.SourceEntryID == "" {
		t.Fatal("defensive Agent history clone dropped per-item source metadata")
	}
}

func TestBoundDurableAgentRunSkipsLegacyCompactionHook(t *testing.T) {
	hookCalls := 0
	agent := NewAgentImpl("durable", durableCompactionAgentLoop{}, WithCompactionHook(func(_ context.Context, history []AgentMessage) ([]AgentMessage, error) {
		hookCalls++
		return append(history, AgentMessage{Role: "user", Content: "hook mutation"}), nil
	}))
	if err := agent.SetHistoryFromSourceSnapshot(CompactionSourceSnapshot{Revision: "r1", BranchID: "b1"}); err != nil {
		t.Fatalf("bind empty durable snapshot: %v", err)
	}
	if _, err := agent.Run(withRunID(context.Background(), "run-1"), Submission{Content: "hello"}); err != nil {
		t.Fatalf("run bound durable agent: %v", err)
	}
	if hookCalls != 0 {
		t.Fatalf("legacy hook calls = %d, want 0 for bound durable run", hookCalls)
	}
}

func TestBoundDurableAgentCompactFailsBeforeHookOrMutation(t *testing.T) {
	hookCalls := 0
	agent := NewAgentImpl("durable", durableCompactionAgentLoop{}, WithCompactionHook(func(_ context.Context, history []AgentMessage) ([]AgentMessage, error) {
		hookCalls++
		return nil, errors.New("hook must not run")
	}))
	if err := agent.SetHistoryFromSourceSnapshot(CompactionSourceSnapshot{
		Revision: "r1", BranchID: "b1", Messages: []AgentMessage{{Role: "user", Content: "unchanged"}}, SourceEntryIDs: []string{"entry-1"},
	}); err != nil {
		t.Fatalf("bind durable snapshot: %v", err)
	}
	before := agent.Messages()
	err := agent.Compact(context.Background())
	if !errors.Is(err, ErrDurableCompactionUnsupported) {
		t.Fatalf("Compact error = %v, want ErrDurableCompactionUnsupported", err)
	}
	if hookCalls != 0 {
		t.Fatalf("legacy hook calls = %d, want 0", hookCalls)
	}
	if after := agent.Messages(); !reflect.DeepEqual(after, before) {
		t.Fatalf("manual compact mutated bound history:\nbefore: %#v\nafter: %#v", before, after)
	}
}
