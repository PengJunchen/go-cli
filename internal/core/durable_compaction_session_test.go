package core_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/session"
)

type durableCompactionSummarizer struct{}

func (durableCompactionSummarizer) Summarize(context.Context, string) (string, error) {
	return "raw durable summary", nil
}

type recordingDurableCompactionSummarizer struct {
	calls int
}

func (summarizer *recordingDurableCompactionSummarizer) Summarize(context.Context, string) (string, error) {
	summarizer.calls++
	return "raw durable summary", nil
}

type inPlaceCurrentBlockCompactor struct {
	calls int
}

func (compactor *inPlaceCurrentBlockCompactor) Compact(_ context.Context, items []compaction.TurnItem, _ int, _ compaction.TokenEstimator) (compaction.CompactionResult, error) {
	compactor.calls++
	current := len(items) - 1
	items[current].ContentBlocks[0].Text = "mutated by compactor"
	priorCount := items[current].Runtime.Source.PriorSourceCount
	through := llm.SourceThrough{PriorHistoryCount: priorCount}
	summary := compaction.TurnItem{
		ID: "mutating-compactor-summary", Role: compaction.RoleUser, Content: "summary of prior context", IsCompaction: true,
		Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: items[current].Runtime.Source.RunID, Through: through}},
	}
	return compaction.CompactionResult{
		Items:         []compaction.TurnItem{items[0], summary, items[current]},
		Strategy:      compaction.StrategySummary,
		NewProjection: &compaction.NewProjection{Summary: summary.Content, Through: through},
	}, nil
}

type durableCurrentBlocksAgent struct {
	loop          *core.LoopAgent
	contentBlocks []llm.ContentBlock
}

func (agent *durableCurrentBlocksAgent) Name() string { return "durable-current-blocks" }

func (agent *durableCurrentBlocksAgent) Run(ctx context.Context, submission core.Submission, stream ...core.EventStream) (core.Result, error) {
	sink, _, ok := core.RunCommitFromContext(ctx)
	if !ok {
		return core.Result{}, errors.New("test: durable run sink is unavailable")
	}
	sourceSink, ok := sink.(core.RunCommitCompactionSource)
	if !ok {
		return core.Result{}, errors.New("test: durable source snapshot is unavailable")
	}
	snapshot, err := sourceSink.CompactionSourceSnapshot()
	if err != nil {
		return core.Result{}, err
	}
	history := make([]core.AgentMessage, len(snapshot.Messages), len(snapshot.Messages)+1)
	for index, message := range snapshot.Messages {
		message.Runtime.Source = llm.SourceRef{
			Kind: llm.SourcePriorPublished, HistoryRevision: snapshot.Revision, BranchID: snapshot.BranchID,
			SourceEntryID: snapshot.SourceEntryIDs[index], PriorOrdinal: uint64(index),
		}
		history[index] = message
	}
	history = append(history, core.AgentMessage{
		Role: string(llm.RoleUser), Content: submission.Content, ContentBlocks: agent.contentBlocks, InputKind: core.InputKindUser,
	})
	submission.History = history
	_, err = agent.loop.RunCanonical(ctx, submission, stream...)
	return core.Result{RunID: core.RunIDFromContext(ctx)}, err
}

type oneTokenForNonEmptyText struct{}

func (oneTokenForNonEmptyText) Estimate(text string) (int, error) {
	if strings.TrimSpace(text) == "" {
		return 0, nil
	}
	return 1, nil
}

type durableCharacterEstimator struct{}

func (durableCharacterEstimator) Estimate(text string) (int, error) {
	return len([]rune(text)), nil
}

type snapshotCheckingChatModel struct {
	base  llm.BaseChatModel
	check func([]llm.Message) error
}

type failingDurableCompactionWriter struct {
	base            session.DurableSessionWriter
	compactionErr   error
	compactionCalls int
}

func (writer *failingDurableCompactionWriter) AppendAndSync(ctx context.Context, entries ...session.SessionEntry) ([]session.SessionEntry, error) {
	for _, entry := range entries {
		if entry.Type == session.EntryTypeCompaction {
			writer.compactionCalls++
			return nil, writer.compactionErr
		}
	}
	return writer.base.AppendAndSync(ctx, entries...)
}

type durableWriterOverrideSession struct {
	session.PreparedSession
	writer session.DurableSessionWriter
}

func (prepared *durableWriterOverrideSession) DurableWriter() session.DurableSessionWriter {
	return prepared.writer
}

func (model *snapshotCheckingChatModel) Generate(ctx context.Context, messages []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	if model.check != nil {
		if err := model.check(messages); err != nil {
			return nil, err
		}
	}
	return model.base.Generate(ctx, messages, opts...)
}

func (model *snapshotCheckingChatModel) Stream(ctx context.Context, messages []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	if model.check != nil {
		if err := model.check(messages); err != nil {
			return nil, err
		}
	}
	return model.base.Stream(ctx, messages, opts...)
}

func submitDurableCompactionRun(t *testing.T, agent core.Agent, sink core.RunCommitSink, prepared session.PreparedSession, workspaceID, content string) {
	t.Helper()
	harness := core.NewHarnessImpl(agent)
	stream, err := harness.Submit(core.WithRunCommit(context.Background(), sink, core.RunCommitBinding{
		SessionID: prepared.View().SessionID, WorkspaceID: workspaceID,
	}), content)
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, err = stream.Result()
	require.NoError(t, err)
}

func runDurableCompactionRun(t *testing.T, agent core.Agent, sink core.RunCommitSink, prepared session.PreparedSession, workspaceID, content string) error {
	t.Helper()
	harness := core.NewHarnessImpl(agent)
	stream, err := harness.Submit(core.WithRunCommit(context.Background(), sink, core.RunCommitBinding{
		SessionID: prepared.View().SessionID, WorkspaceID: workspaceID,
	}), content)
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, err = stream.Result()
	return err
}

func TestDurableCompactionRejectsSummaryThatConsumesOversizedCurrentInput(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspaceID := "workspace-v1-durable-current-input"
	index, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "durable current input")
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})

	content := "current input:" + strings.Repeat("x", 1000)
	summarizer := &recordingDurableCompactionSummarizer{}
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("durable-current-input", "durable current input",
		mock.ConversationTurn{AssistantContent: "must not be requested"},
	))
	loop := core.NewLoopAgent(core.WithLLM(model), core.WithSystemPrompt("s"))
	loop.SetMidTurnCompaction(
		compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)),
		compaction.NewSummaryCompactor(summarizer),
		durableCharacterEstimator{},
		104,
	)
	agent := core.NewAgentImpl("durable-current-input", loop)

	runErr := runDurableCompactionRun(t, agent, sink, prepared, workspaceID, content)
	require.ErrorIs(t, runErr, core.ErrDurableCompactionUnsupported,
		"durable preflight must fail closed if the only way to fit the request is to summarize its current input")
	require.Equal(t, 1, summarizer.calls, "the deterministic summary provider proves this reached the cut-through-input path")
	require.Zero(t, model.CallCount(), "an unsupported current-input projection must be rejected before provider dispatch")

	live, err := prepared.Snapshot()
	require.NoError(t, err)
	for _, entry := range live.DurableEntries {
		require.NotEqual(t, session.EntryTypeCompaction, entry.Type, "a rejected input projection must not be committed")
	}
	require.Len(t, live.PublishedView.Messages, 1)
	require.Equal(t, content, live.PublishedView.Messages[0].Content)

	sessionID := live.SessionID
	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareResume(context.Background(), sessionID)
	require.NoError(t, err)
	defer reopened.Close()
	reopenedSnapshot, err := reopened.Snapshot()
	require.NoError(t, err)
	require.Equal(t, live.PublishedView, reopenedSnapshot.PublishedView,
		"the rejected projection must leave the raw user input unchanged after durable reopen")
	require.Len(t, reopenedSnapshot.PublishedView.Messages, 1)
	require.Equal(t, content, reopenedSnapshot.PublishedView.Messages[0].Content)
}

func TestDurableCompactionRejectsInPlaceMutationOfCurrentContentBlockBeforeProviderOrCommit(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspaceID := "workspace-v1-durable-current-block-mutation"
	index, prepared, initialSink := prepareRunCommitSession(t, root, workspaceID, "durable current block mutation")
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})

	priorModel := runDurableCompactionTurn(t, initialSink, prepared, workspaceID, "prior durable turn", nil, 0, false)
	require.Equal(t, 1, priorModel.CallCount())

	currentSink, err := session.NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	mutator := &inPlaceCurrentBlockCompactor{}
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("durable-current-block-mutation", "durable current block mutation",
		mock.ConversationTurn{AssistantContent: "must not be requested"},
	))
	loop := core.NewLoopAgent(core.WithLLM(model), core.WithSystemPrompt("s"))
	loop.SetMidTurnCompaction(
		compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)),
		mutator,
		durableCharacterEstimator{},
		1,
	)
	agent := &durableCurrentBlocksAgent{
		loop:          loop,
		contentBlocks: []llm.ContentBlock{{Type: "text", Text: "original multimodal input"}},
	}
	content := "current block input"

	runErr := runDurableCompactionRun(t, agent, currentSink, prepared, workspaceID, content)
	require.ErrorIs(t, runErr, core.ErrDurableCompactionUnsupported,
		"mutating the returned current item in place must not rewrite the raw current input snapshot")
	require.Equal(t, 1, mutator.calls)
	require.Zero(t, model.CallCount(), "a changed current content block must be rejected before provider dispatch")

	live, err := prepared.Snapshot()
	require.NoError(t, err)
	for _, entry := range live.DurableEntries {
		require.NotEqual(t, session.EntryTypeCompaction, entry.Type, "a rejected in-place mutation must not append a compaction row")
	}
	current := live.PublishedView.Messages[len(live.PublishedView.Messages)-1]
	require.Equal(t, content, current.Content)
	require.Equal(t, "original multimodal input", current.ContentBlocks[0].Text,
		"the already-synced JSONL input must retain its original content block")

	sessionID := live.SessionID
	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareResume(context.Background(), sessionID)
	require.NoError(t, err)
	defer reopened.Close()
	reopenedSnapshot, err := reopened.Snapshot()
	require.NoError(t, err)
	reopenedCurrent := reopenedSnapshot.PublishedView.Messages[len(reopenedSnapshot.PublishedView.Messages)-1]
	require.Equal(t, "original multimodal input", reopenedCurrent.ContentBlocks[0].Text,
		"reopening the JSONL session must not observe the compactor's in-place mutation")
}

func TestDurableCompactionWithoutCutSendsCurrentInputOnceAndReopens(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspaceID := "workspace-v1-durable-no-compaction-input"
	index, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "durable no compaction input")
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})

	content := "keep this current input unchanged"
	summarizer := &recordingDurableCompactionSummarizer{}
	baseModel := mock.NewMockLLMServer(mock.NewConversationTemplate("durable-no-compaction-input", "durable no compaction input",
		mock.ConversationTurn{AssistantContent: "accepted"},
	))
	providerInputCount := 0
	model := &snapshotCheckingChatModel{
		base: baseModel,
		check: func(messages []llm.Message) error {
			providerInputCount = 0
			for _, message := range messages {
				if message.Role != llm.RoleUser || message.Content != content {
					continue
				}
				providerInputCount++
				if message.Runtime.Source.Kind != llm.SourceRunTranscript || message.Runtime.Source.TranscriptIndex != 0 {
					return fmt.Errorf("test: provider received current input with unexpected source identity: %#v", message.Runtime.Source)
				}
			}
			if providerInputCount != 1 {
				return fmt.Errorf("test: provider received current input %d times", providerInputCount)
			}
			return nil
		},
	}
	loop := core.NewLoopAgent(core.WithLLM(model), core.WithSystemPrompt("s"))
	loop.SetMidTurnCompaction(
		compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)),
		compaction.NewSummaryCompactor(summarizer),
		oneTokenForNonEmptyText{},
		10,
	)
	agent := core.NewAgentImpl("durable-no-compaction-input", loop)

	require.NoError(t, runDurableCompactionRun(t, agent, sink, prepared, workspaceID, content))
	require.Equal(t, 1, baseModel.CallCount())
	require.Equal(t, 1, providerInputCount)
	require.Zero(t, summarizer.calls, "an input already under budget must not be summarized")

	live, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, live.PublishedView.Messages, 2)
	require.Equal(t, content, live.PublishedView.Messages[0].Content)
	for _, entry := range live.DurableEntries {
		require.NotEqual(t, session.EntryTypeCompaction, entry.Type)
	}

	sessionID := live.SessionID
	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareResume(context.Background(), sessionID)
	require.NoError(t, err)
	defer reopened.Close()
	reopenedSnapshot, err := reopened.Snapshot()
	require.NoError(t, err)
	require.Equal(t, live.PublishedView, reopenedSnapshot.PublishedView)
	require.Len(t, reopenedSnapshot.PublishedView.Messages, 2)
	require.Equal(t, content, reopenedSnapshot.PublishedView.Messages[0].Content)
}

func TestDurableCompactionCommitFailureBlocksProviderAndPreservesCurrentInput(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspaceID := "workspace-v1-durable-compaction-commit-failure"
	index, prepared, firstSink := prepareRunCommitSession(t, root, workspaceID, "durable compaction commit failure")
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})

	firstModel := runDurableCompactionTurn(t, firstSink, prepared, workspaceID, "repeat text", nil, 0, false)
	require.Equal(t, 1, firstModel.CallCount())
	firstSnapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, firstSnapshot.PublishedView.Messages, 2)
	compactionErr := errors.New("test: compaction projection sync rejected")
	failingWriter := &failingDurableCompactionWriter{
		base:          prepared.DurableWriter(),
		compactionErr: compactionErr,
	}
	preparedWithFailingWriter := &durableWriterOverrideSession{PreparedSession: prepared, writer: failingWriter}
	failingSink, err := session.NewSessionRunCommitSink(preparedWithFailingWriter, index, workspaceID, nil)
	require.NoError(t, err)
	sourceSnapshot, err := failingSink.CompactionSourceSnapshot()
	require.NoError(t, err)

	content := "current turn"
	summarizer := &recordingDurableCompactionSummarizer{}
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("durable-compaction-commit-failure", "durable compaction commit failure",
		mock.ConversationTurn{AssistantContent: "must not be requested"},
	))
	loop := core.NewLoopAgent(core.WithLLM(model), core.WithSystemPrompt("s"))
	loop.SetMidTurnCompaction(
		compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)),
		compaction.NewSummaryCompactor(summarizer),
		oneTokenForNonEmptyText{},
		6,
	)
	agent := core.NewAgentImpl("durable-compaction-commit-failure", loop)
	require.NoError(t, agent.SetHistoryFromSourceSnapshot(sourceSnapshot))

	runErr := runDurableCompactionRun(t, agent, failingSink, prepared, workspaceID, content)
	require.ErrorIs(t, runErr, compactionErr)
	require.Equal(t, 1, summarizer.calls, "the compaction projection must be prepared before its sync failure")
	require.Equal(t, 1, failingWriter.compactionCalls)
	require.Zero(t, model.CallCount(), "a failed durable compaction commit must stop provider dispatch")

	live, err := prepared.Snapshot()
	require.NoError(t, err)
	for _, entry := range live.DurableEntries {
		require.NotEqual(t, session.EntryTypeCompaction, entry.Type, "the failing sink must not append a projection")
	}
	currentCount := 0
	for _, message := range live.PublishedView.Messages {
		if message.Content == content {
			currentCount++
		}
	}
	require.Equal(t, 1, currentCount)
	require.Equal(t, content, live.PublishedView.Messages[len(live.PublishedView.Messages)-1].Content)

	sessionID := live.SessionID
	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareResume(context.Background(), sessionID)
	require.NoError(t, err)
	defer reopened.Close()
	reopenedSnapshot, err := reopened.Snapshot()
	require.NoError(t, err)
	require.Equal(t, live.PublishedView, reopenedSnapshot.PublishedView)
	require.Equal(t, content, reopenedSnapshot.PublishedView.Messages[len(reopenedSnapshot.PublishedView.Messages)-1].Content)
}

func runDurableCompactionTurn(t *testing.T, sink core.RunCommitSink, prepared session.PreparedSession, workspaceID, content string, model llm.BaseChatModel, maxTokens int, withSummary bool) *mock.MockLLMServer {
	t.Helper()
	var recordingModel *mock.MockLLMServer
	if model == nil {
		recordingModel = mock.NewMockLLMServer(mock.NewConversationTemplate("durable-compaction-turn", "durable compaction turn",
			mock.ConversationTurn{AssistantContent: "repeat text"},
		))
		model = recordingModel
	}
	loop := core.NewLoopAgent(core.WithLLM(model))
	if withSummary {
		loop.SetMidTurnCompaction(
			compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)),
			compaction.NewSummaryCompactor(durableCompactionSummarizer{}),
			oneTokenForNonEmptyText{},
			maxTokens,
		)
	}
	agent := core.NewAgentImpl("durable-compaction-test", loop)
	submitDurableCompactionRun(t, agent, sink, prepared, workspaceID, content)
	return recordingModel
}

func TestDurableCompactionSessionProjectionSyncsBeforeRequestAndReopensExactly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspaceID := "workspace-v1-durable-compaction"
	index, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "durable compaction")
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})

	firstModel := runDurableCompactionTurn(t, sink, prepared, workspaceID, "repeat text", nil, 0, false)
	require.Equal(t, 1, firstModel.CallCount())
	firstSnapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, firstSnapshot.PublishedView.Messages, 2)
	require.Equal(t, firstSnapshot.PublishedView.Messages[0].Content, firstSnapshot.PublishedView.Messages[1].Content,
		"the source history deliberately contains repeated text with distinct durable source IDs")
	require.NotEqual(t, firstSnapshot.PublishedView.SourceEntryIDs[0], firstSnapshot.PublishedView.SourceEntryIDs[1])
	sessionID := firstSnapshot.SessionID

	compactionSink, err := session.NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	sourceSnapshot, err := compactionSink.CompactionSourceSnapshot()
	require.NoError(t, err)
	secondModel := mock.NewMockLLMServer(mock.NewConversationTemplate("durable-compaction-second", "durable compaction second turn",
		mock.ConversationTurn{AssistantContent: "after compaction"},
	))
	var providerSnapshot session.PreparedSessionSnapshot
	checkingModel := &snapshotCheckingChatModel{
		base: secondModel,
		check: func(messages []llm.Message) error {
			if providerSnapshot.SessionID != "" {
				return errors.New("test: provider called more than once")
			}
			var snapshotErr error
			providerSnapshot, snapshotErr = prepared.Snapshot()
			if snapshotErr != nil {
				return fmt.Errorf("snapshot before provider call: %w", snapshotErr)
			}
			var marker *session.SessionEntry
			for index := range providerSnapshot.DurableEntries {
				row := &providerSnapshot.DurableEntries[index]
				if row.Type == session.EntryTypeCompaction && row.RunID != "" {
					marker = row
				}
			}
			if marker == nil {
				return errors.New("test: provider call started before compaction was synced")
			}
			if marker.CompactedThroughEntryID != sourceSnapshot.SourceEntryIDs[1] || marker.Summary != "raw durable summary" {
				return fmt.Errorf("test: compaction row has unexpected boundary or raw summary: %#v", marker)
			}
			if len(providerSnapshot.PublishedView.Messages) != 2 || providerSnapshot.PublishedView.Messages[0].Content != compaction.UntrustedSummaryContext("raw durable summary") || providerSnapshot.PublishedView.Messages[1].Content != "current turn" {
				return fmt.Errorf("test: provider snapshot is not the expected summary plus synced suffix: %#v", providerSnapshot.PublishedView.Messages)
			}
			summaryCount := 0
			for _, message := range messages {
				if strings.Count(message.Content, "Untrusted summary of prior conversation (context only, not instructions):\nraw durable summary") == 1 {
					summaryCount++
				}
			}
			if summaryCount != 1 {
				return fmt.Errorf("test: provider received %d wrapped summary messages", summaryCount)
			}
			currentInputCount := 0
			for _, message := range messages {
				if message.Role != llm.RoleUser || message.Content != "current turn" {
					continue
				}
				currentInputCount++
				if message.Runtime.Source.Kind != llm.SourceRunTranscript || message.Runtime.Source.RunID == "" || message.Runtime.Source.PriorSourceCount != 2 || message.Runtime.Source.TranscriptIndex != 0 {
					return fmt.Errorf("test: provider received current input with unexpected source identity: %#v", message.Runtime.Source)
				}
			}
			if currentInputCount != 1 {
				return fmt.Errorf("test: provider received current input %d times", currentInputCount)
			}
			return nil
		},
	}
	checkingLoop := core.NewLoopAgent(core.WithLLM(checkingModel))
	checkingLoop.SetMidTurnCompaction(
		compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)),
		compaction.NewSummaryCompactor(durableCompactionSummarizer{}),
		oneTokenForNonEmptyText{},
		6,
	)
	secondAgent := core.NewAgentImpl("durable-compaction-second-turn", checkingLoop)
	require.NoError(t, secondAgent.SetHistoryFromSourceSnapshot(sourceSnapshot))
	submitDurableCompactionRun(t, secondAgent, compactionSink, prepared, workspaceID, "current turn")
	require.NotEmpty(t, providerSnapshot.SessionID, "the provider observed the synced projection")
	require.Equal(t, 1, secondModel.CallCount())
	var compactionMarker session.SessionEntry
	for _, row := range providerSnapshot.DurableEntries {
		if row.Type == session.EntryTypeCompaction && row.RunID != "" {
			compactionMarker = row
		}
	}
	require.NotEmpty(t, compactionMarker.ID)
	require.Equal(t, sourceSnapshot.SourceEntryIDs[1], compactionMarker.CompactedThroughEntryID)
	require.Equal(t, "raw durable summary", compactionMarker.Summary)
	secondLive, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Len(t, secondLive.PublishedView.Messages, 3)
	require.Equal(t, compaction.UntrustedSummaryContext("raw durable summary"), secondLive.PublishedView.Messages[0].Content)
	require.Equal(t, "current turn", secondLive.PublishedView.Messages[1].Content)
	require.Equal(t, "after compaction", secondLive.PublishedView.Messages[2].Content)

	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareResume(context.Background(), sessionID)
	require.NoError(t, err)
	defer reopened.Close()
	reopenedSnapshot, err := reopened.Snapshot()
	require.NoError(t, err)
	require.Equal(t, secondLive.PublishedView, reopenedSnapshot.PublishedView,
		"the append-and-sync projection and reopened JSONL history must agree exactly")
	require.Equal(t, secondLive.PublishedView.SourceEntryIDs, reopenedSnapshot.PublishedView.SourceEntryIDs)
	require.Equal(t, secondLive.PublishedView.Messages, reopenedSnapshot.PublishedView.Messages)
	require.Equal(t, compaction.UntrustedSummaryContext("raw durable summary"), reopenedSnapshot.PublishedView.Messages[0].Content,
		"the live and reopened summary must both have exactly one untrusted wrapper")

	restoredSink, err := session.NewSessionRunCommitSink(reopened, index, workspaceID, nil)
	require.NoError(t, err)
	restoredSources, err := restoredSink.CompactionSourceSnapshot()
	require.NoError(t, err)
	restoredModel := mock.NewMockLLMServer(mock.NewConversationTemplate("durable-compaction-restored", "restored compaction history",
		mock.ConversationTurn{AssistantContent: "restored history accepted"},
	))
	restoredLoop := core.NewLoopAgent(core.WithLLM(restoredModel))
	restoredAgent := core.NewAgentImpl("durable-compaction-restored", restoredLoop)
	require.NoError(t, restoredAgent.SetHistoryFromSourceSnapshot(restoredSources))
	submitDurableCompactionRun(t, restoredAgent, restoredSink, reopened, workspaceID, "next turn")
	require.Equal(t, 1, restoredModel.CallCount())
	restoredCalls := restoredModel.CallLog()
	require.Len(t, restoredCalls, 1)
	var restoredSummary *llm.Message
	for index := range restoredCalls[0].Messages {
		message := &restoredCalls[0].Messages[index]
		if strings.HasPrefix(message.Content, "Untrusted summary of prior conversation") {
			restoredSummary = message
		}
	}
	require.NotNil(t, restoredSummary)
	require.Equal(t, compaction.UntrustedSummaryContext("raw durable summary"), restoredSummary.Content)
	require.Equal(t, llm.SourcePriorPublished, restoredSummary.Runtime.Source.Kind,
		"a rehydrated summary is an ordinary prior published source, not the old run's Through cursor")
	require.Equal(t, compactionMarker.ID, restoredSummary.Runtime.Source.SourceEntryID)
}
