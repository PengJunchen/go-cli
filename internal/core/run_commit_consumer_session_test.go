package core_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type receiptGateSessionWriter struct {
	base          session.DurableSessionWriter
	statusHandles <-chan core.RunStatusHandle
	signals       <-chan tools.PreparedCompletionSignal
	receiptAppend chan struct{}
	receiptOnce   sync.Once
}

func (writer *receiptGateSessionWriter) AppendAndSync(ctx context.Context, entries ...session.SessionEntry) ([]session.SessionEntry, error) {
	for _, entry := range entries {
		if entry.Type != session.EntryTypeTool || entry.ReceiptOutcome == "" {
			continue
		}
		writer.receiptOnce.Do(func() { close(writer.receiptAppend) })
		var status core.RunStatusHandle
		select {
		case status = <-writer.statusHandles:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
			return nil, errors.New("test: run status handle was not attached before receipt sync")
		}
		var signal tools.PreparedCompletionSignal
		select {
		case signal = <-writer.signals:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
			return nil, errors.New("test: prepared completion signal was not captured")
		}
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			if done := signal.Done(); done != nil {
				select {
				case <-done:
					if signal.Proof() != "" && status.Snapshot().PendingWorkers == 0 {
						return writer.base.AppendAndSync(ctx, entries...)
					}
				default:
				}
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-timer.C:
				return nil, errors.New("test: receipt sync passed before local proof became owner-visible")
			case <-ticker.C:
			}
		}
	}
	return writer.base.AppendAndSync(ctx, entries...)
}

type receiptGatedPreparedSession struct {
	session.PreparedSession
	writer session.DurableSessionWriter
}

func (prepared *receiptGatedPreparedSession) DurableWriter() session.DurableSessionWriter {
	return prepared.writer
}

type sessionCapturingToolPreparer struct {
	*tools.MiddlewareToolRegistry
	signals chan tools.PreparedCompletionSignal
}

type r6AskClassifier struct{}

func (r6AskClassifier) Name() string { return "r6_run_commit_ask" }
func (r6AskClassifier) Classify(context.Context, tools.ToolCall) approval.Classification {
	return approval.Ask
}

type r6WaitingApprovalCallback struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (callback r6WaitingApprovalCallback) RequestApproval(ctx context.Context, _ string, _ map[string]any) (approval.ApprovalResult, error) {
	select {
	case callback.entered <- struct{}{}:
	case <-ctx.Done():
		return approval.ApprovalDeny, ctx.Err()
	}
	select {
	case <-callback.release:
		return approval.ApprovalAllow, nil
	case <-ctx.Done():
		return approval.ApprovalDeny, ctx.Err()
	}
}

type r6RunCommitProjectTool struct {
	calls atomic.Int32
}

func (*r6RunCommitProjectTool) Name() string { return "project_custom" }
func (*r6RunCommitProjectTool) Description() string {
	return "R6 project-bound durable receipt fixture"
}
func (tool *r6RunCommitProjectTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	tool.calls.Add(1)
	return &tools.ToolResult{Output: "must not run"}, nil
}

type r6RunCommitBlockingProjectTool struct {
	calls   atomic.Int32
	entered chan<- struct{}
	release <-chan struct{}
}

func (*r6RunCommitBlockingProjectTool) Name() string { return "project_commit_win" }
func (*r6RunCommitBlockingProjectTool) Description() string {
	return "R6 committed action survives a later project trust revoke"
}
func (tool *r6RunCommitBlockingProjectTool) Execute(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	tool.calls.Add(1)
	select {
	case tool.entered <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-tool.release:
		return &tools.ToolResult{Output: "committed project action finished"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newRunCommitStreamingRegistry(ctx context.Context, t *testing.T, policyCalls *atomic.Int32) *tools.MiddlewareToolRegistry {
	t.Helper()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	require.NoError(t, tools.RegisterDefaults(ctx, deferred,
		tools.WithRegisteredBuiltinWhitelist([]string{"bash", "read"}),
		tools.WithRegisteredBashSandbox(tools.AllowAllSandbox{}),
	))
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		if policyCalls != nil {
			policyCalls.Add(1)
		}
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	return registry
}

func collectToolOutputEvents(stream core.EventStream) <-chan []core.AgentEvent {
	collected := make(chan []core.AgentEvent, 1)
	go func() {
		var output []core.AgentEvent
		for event := range stream.Events() {
			if event.Kind == "tool_output" {
				output = append(output, event)
			}
		}
		collected <- output
	}()
	return collected
}

func collectAgentEvents(stream core.EventStream) <-chan []core.AgentEvent {
	collected := make(chan []core.AgentEvent, 1)
	go func() {
		var events []core.AgentEvent
		for event := range stream.Events() {
			events = append(events, event)
		}
		collected <- events
	}()
	return collected
}

type durableConsumerUnattestedEventBus struct {
	toolOutputPublished atomic.Int32
}

func (*durableConsumerUnattestedEventBus) Subscribe(context.Context) <-chan core.AgentEvent {
	return make(chan core.AgentEvent)
}

func (bus *durableConsumerUnattestedEventBus) Publish(event core.AgentEvent) {
	if event.Kind == "tool_output" {
		bus.toolOutputPublished.Add(1)
	}
}

func (*durableConsumerUnattestedEventBus) Close() {}

func prepareRunCommitSession(t *testing.T, root, workspaceID, description string) (*session.WorkspaceQuarantineIndex, session.PreparedSession, core.RunCommitSink) {
	t.Helper()
	ctx := context.Background()
	index, err := session.NewWorkspaceQuarantineIndex()
	require.NoError(t, err)
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	assertWorkspaceIndexUsesTempHome(t, root, workspaceID)
	prepared, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareNew(ctx, description)
	require.NoError(t, err)
	sink, err := session.NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	if err != nil {
		_ = prepared.Close()
	}
	require.NoError(t, err)
	return index, prepared, sink
}

func TestRunCommitProjectRevocationDuringAskWritesOneNotDispatchedReceipt(t *testing.T) {
	const (
		storePathEnv = "GO_CLI_R6_RUN_COMMIT_TRUST_STORE"
		rootPathEnv  = "GO_CLI_R6_RUN_COMMIT_TRUST_ROOT"
	)
	if storePath := os.Getenv(storePathEnv); storePath != "" {
		manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(storePath))
		if err := manager.RevokeTrust(context.Background(), os.Getenv(rootPathEnv)); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(2)
		}
		os.Exit(0)
	}

	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspaceID := "workspace-v1-r6-revoke-during-ask"
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	projectConfig := filepath.Join(workspace, ".go-cli.yaml")
	require.NoError(t, os.WriteFile(projectConfig, []byte("tools: {}\n"), 0o600))
	trustPath := filepath.Join(root, "config", "go-cli", "trust.json")
	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(trustPath))
	snapshot, err := config.CaptureProjectSources(ctx, workspace, nil)
	require.NoError(t, err)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	binding, err := frozen.CheckProjectSourceSnapshot(ctx, snapshot)
	require.NoError(t, err)

	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	target := &r6RunCommitProjectTool{}
	require.NoError(t, base.RegisterProjectSourced(ctx, target, binding))
	fence := base.DispatchFence()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAsk := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAsk)
	middleware := approval.NewApprovalMiddleware(r6AskClassifier{}, approval.NewInMemoryApprovalStore(),
		approval.WithCallback(r6WaitingApprovalCallback{entered: entered, release: release}),
		approval.WithDispatchFence(fence),
		approval.WithPlanModeController(core.NewDefaultPlanModeController(fence)),
	)
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))

	index, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "R6 revoke during Ask")
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("r6-revoke-during-ask", "R6 revoke during Ask",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "project-call-1", Name: target.Name(), Args: map[string]any{"value": "x"},
		}}},
		mock.ConversationTurn{AssistantContent: "The project call was safely rejected."},
	))
	agent := core.NewAgentImpl("r6-revoke-during-ask", core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry)))
	harness := core.NewHarnessImpl(agent, core.WithHarnessEventBus(core.NewMemoryEventBus()))
	stream, err := harness.Submit(core.WithRunCommit(ctx, sink, core.RunCommitBinding{
		SessionID: prepared.View().SessionID, WorkspaceID: workspaceID,
	}), "call the project tool")
	require.NoError(t, err)
	eventsDone := make(chan struct{})
	go func() {
		for range stream.Events() {
		}
		close(eventsDone)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		_, runErr := stream.Result()
		t.Fatalf("project approval callback did not enter its blocked Ask phase: run error %v", runErr)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestRunCommitProjectRevocationDuringAskWritesOneNotDispatchedReceipt$")
	command.Env = append(os.Environ(), storePathEnv+"="+trustPath, rootPathEnv+"="+snapshot.CanonicalRoot())
	if err := command.Start(); err != nil {
		releaseAsk()
		t.Fatal(err)
	}
	revokeDone := make(chan error, 1)
	go func() { revokeDone <- command.Wait() }()
	select {
	case err := <-revokeDone:
		require.NoError(t, err, "the subprocess revoke must finish before approval is released")
	case <-time.After(5 * time.Second):
		releaseAsk()
		_ = <-revokeDone
		t.Fatal("cross-process revoke blocked while the durable run awaited Ask")
	}
	releaseAsk()
	<-eventsDone
	_, runErr := stream.Result()
	require.NoError(t, runErr)
	require.Zero(t, target.calls.Load(), "the project executor must never start after revoke")
	require.Equal(t, 2, model.CallCount(), "the run should persist a terminal result after the known pre-dispatch rejection")

	sessionID := prepared.View().SessionID
	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareResume(ctx, sessionID)
	require.NoError(t, err, "the rejected receipt must survive closing and reopening the physical session")
	t.Cleanup(func() { _ = reopened.Close() })
	state, err := reopened.Snapshot()
	require.NoError(t, err)
	var intents []session.SessionEntry
	var receipts []session.SessionEntry
	for _, row := range state.DurableEntries {
		if row.Type != session.EntryTypeTool || row.ToolCallID != "project-call-1" {
			continue
		}
		if row.ActionState == session.ActionStateIntent {
			intents = append(intents, row)
		}
		if row.ReceiptOutcome != "" {
			receipts = append(receipts, row)
		}
	}
	require.Len(t, intents, 1, "the ActionID is reserved once before final dispatch")
	require.Len(t, receipts, 1, "exactly one terminal receipt is persisted")
	require.Equal(t, intents[0].ActionID, receipts[0].ActionID)
	require.NotEmpty(t, receipts[0].ActionID)
	require.Equal(t, core.DispatchStateNotDispatched, receipts[0].DispatchState)
	require.Equal(t, core.ReceiptOutcomeError, receipts[0].ReceiptOutcome)
}

func TestRunCommitProjectRevocationAfterDispatchKeepsCommittedReceipt(t *testing.T) {
	const (
		storePathEnv = "GO_CLI_R6_RUN_COMMIT_LATE_REVOKE_STORE"
		rootPathEnv  = "GO_CLI_R6_RUN_COMMIT_LATE_REVOKE_ROOT"
	)
	if storePath := os.Getenv(storePathEnv); storePath != "" {
		manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(storePath))
		if err := manager.RevokeTrust(context.Background(), os.Getenv(rootPathEnv)); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(2)
		}
		os.Exit(0)
	}

	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspaceID := "workspace-v1-r6-committed-late-revoke"
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	projectConfig := filepath.Join(workspace, ".go-cli.yaml")
	require.NoError(t, os.WriteFile(projectConfig, []byte("tools: {}\n"), 0o600))
	trustPath := filepath.Join(root, "config", "go-cli", "trust.json")
	manager := approval.NewDefaultTrustManager(approval.NewFileTrustStore(trustPath))
	snapshot, err := config.CaptureProjectSources(ctx, workspace, nil)
	require.NoError(t, err)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	binding, err := frozen.CheckProjectSourceSnapshot(ctx, snapshot)
	require.NoError(t, err)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseExecutor := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseExecutor)
	target := &r6RunCommitBlockingProjectTool{entered: entered, release: release}
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, base.RegisterProjectSourced(ctx, target, binding))
	fence := base.DispatchFence()
	middleware := approval.NewApprovalMiddleware(approval.AllowAllClassifier{}, approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(true),
		approval.WithDispatchFence(fence),
		approval.WithPlanModeController(core.NewDefaultPlanModeController(fence)),
	)
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))

	index, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "R6 committed action before late revoke")
	sessionID := prepared.View().SessionID
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("r6-committed-late-revoke", "R6 committed late revoke",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "project-commit-win-1", Name: target.Name(), Args: map[string]any{"value": "x"},
		}}},
		mock.ConversationTurn{AssistantContent: "The committed project call completed."},
	))
	agent := core.NewAgentImpl("r6-committed-late-revoke", core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry)))
	harness := core.NewHarnessImpl(agent, core.WithHarnessEventBus(core.NewMemoryEventBus()))
	stream, err := harness.Submit(core.WithRunCommit(ctx, sink, core.RunCommitBinding{
		SessionID: sessionID, WorkspaceID: workspaceID,
	}), "call the project tool")
	require.NoError(t, err)
	eventsDone := make(chan struct{})
	go func() {
		for range stream.Events() {
		}
		close(eventsDone)
	}()
	select {
	case <-entered:
		// Execute is entered only after the prepared invocation has set its
		// final dispatch marker; the trust-store lock is no longer held.
	case <-time.After(5 * time.Second):
		_, runErr := stream.Result()
		t.Fatalf("project executor did not enter after the dispatch marker: run error %v", runErr)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestRunCommitProjectRevocationAfterDispatchKeepsCommittedReceipt$")
	command.Env = append(os.Environ(), storePathEnv+"="+trustPath, rootPathEnv+"="+snapshot.CanonicalRoot())
	if err := command.Start(); err != nil {
		releaseExecutor()
		t.Fatal(err)
	}
	revokeDone := make(chan error, 1)
	go func() { revokeDone <- command.Wait() }()
	select {
	case err := <-revokeDone:
		require.NoError(t, err, "a later cross-process revoke must complete while the committed executor is paused")
	case <-time.After(5 * time.Second):
		releaseExecutor()
		_ = <-revokeDone
		t.Fatal("cross-process revoke blocked while the committed executor was running")
	}
	releaseExecutor()
	<-eventsDone
	_, runErr := stream.Result()
	require.ErrorIs(t, runErr, core.ErrRunCommitQuiescenceUnproven, "an extension-style prepared tool has no R1 completion proof")
	require.EqualValues(t, 1, target.calls.Load(), "revocation after dispatch cannot undo the committed tool action")
	require.Equal(t, 1, model.CallCount(), "without an R1 completion proof, the durable run cannot request another model turn")

	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareResume(ctx, sessionID)
	require.NoError(t, err, "the committed receipt must survive closing and reopening the physical session")
	t.Cleanup(func() { _ = reopened.Close() })
	state, err := reopened.Snapshot()
	require.NoError(t, err)
	var intents []session.SessionEntry
	var receipts []session.SessionEntry
	for _, row := range state.DurableEntries {
		if row.Type != session.EntryTypeTool || row.ToolCallID != "project-commit-win-1" {
			continue
		}
		if row.ActionState == session.ActionStateIntent {
			intents = append(intents, row)
		}
		if row.ReceiptOutcome != "" {
			receipts = append(receipts, row)
		}
	}
	require.Len(t, intents, 1, "the ActionID must be reserved once before dispatch")
	require.Len(t, receipts, 1, "exactly one committed terminal receipt must be persisted")
	require.NotEmpty(t, intents[0].ActionID)
	require.Equal(t, intents[0].ActionID, receipts[0].ActionID)
	require.Equal(t, core.DispatchStateDispatched, receipts[0].DispatchState)
	require.Equal(t, core.ReceiptOutcomeSuccess, receipts[0].ReceiptOutcome)
	require.Equal(t, "committed project action finished", receipts[0].Content)
}

func TestRunCommitLoopStreamsPreparedBashAndReopensWithSessionSink(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspaceID := "workspace-v1-streaming-consumer"
	index, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "streaming consumer")
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})

	var policyCalls atomic.Int32
	registry := newRunCommitStreamingRegistry(context.Background(), t, &policyCalls)
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("streaming-consumer", "streaming consumer",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "bash-stream-1", Name: "bash", Args: map[string]any{"command": "printf 'durable stdout\\n'; printf 'durable stderr\\n' >&2"},
		}}},
		mock.ConversationTurn{AssistantContent: "streaming command completed"},
	))
	agent := core.NewAgentImpl("r2-streaming-session-consumer", core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry)))
	harness := core.NewHarnessImpl(agent, core.WithHarnessEventBus(core.NewMemoryEventBus()))
	stream, err := harness.Submit(core.WithRunCommit(context.Background(), sink, core.RunCommitBinding{
		SessionID: prepared.View().SessionID, WorkspaceID: workspaceID,
	}), "run a streaming command")
	require.NoError(t, err)
	events := <-collectAgentEvents(stream)
	result, runErr := stream.Result()
	require.NoError(t, runErr)
	require.Equal(t, "streaming command completed", result.Content)
	require.Equal(t, int32(1), policyCalls.Load())
	require.Equal(t, 2, model.CallCount(), "proof-bearing streaming completion must permit the next provider call")
	require.NotEmpty(t, events)
	var assistantEvent *core.AgentEvent
	var finalAssistantEvent *core.AgentEvent
	var toolCallEvent *core.AgentEvent
	var toolResultEvent *core.AgentEvent
	var toolOutputEvents []core.AgentEvent
	for index := range events {
		event := &events[index]
		if event.Kind == "message" && event.Content == "" && len(event.ToolCalls) == 1 {
			assistantEvent = event
		}
		if event.Kind == "message" && event.Content == "streaming command completed" {
			finalAssistantEvent = event
		}
		if event.Kind == "tool_call" && event.ToolCallID == "bash-stream-1" {
			toolCallEvent = event
		}
		if event.Kind == "tool_result" && event.ToolCallID == "bash-stream-1" {
			toolResultEvent = event
		}
		if event.Kind == "tool_output" {
			toolOutputEvents = append(toolOutputEvents, *event)
		}
	}
	require.NotNil(t, assistantEvent)
	require.NotEmpty(t, assistantEvent.RunID)
	runID := assistantEvent.RunID
	require.NotNil(t, assistantEvent.CanonicalTranscriptIndex)
	require.Equal(t, uint64(1), *assistantEvent.CanonicalTranscriptIndex)
	require.NotNil(t, assistantEvent.AssistantTranscriptOrdinal)
	require.Equal(t, uint64(1), *assistantEvent.AssistantTranscriptOrdinal)
	require.NotNil(t, finalAssistantEvent)
	require.Equal(t, runID, finalAssistantEvent.RunID)
	require.NotNil(t, finalAssistantEvent.CanonicalTranscriptIndex)
	require.Equal(t, uint64(3), *finalAssistantEvent.CanonicalTranscriptIndex)
	require.Nil(t, finalAssistantEvent.AssistantTranscriptOrdinal, "a final answer without tool calls is not a tool-call occurrence")
	require.NotNil(t, toolCallEvent)
	require.Equal(t, runID, toolCallEvent.RunID)
	require.NotNil(t, toolCallEvent.CanonicalTranscriptIndex)
	require.Equal(t, uint64(1), *toolCallEvent.CanonicalTranscriptIndex)
	require.NotNil(t, toolCallEvent.AssistantTranscriptOrdinal)
	require.Equal(t, uint64(1), *toolCallEvent.AssistantTranscriptOrdinal)
	require.NotNil(t, toolResultEvent)
	require.Equal(t, runID, toolResultEvent.RunID)
	require.Equal(t, core.ReceiptOutcomeSuccess, toolResultEvent.ReceiptOutcome)
	require.NotNil(t, toolResultEvent.AssistantTranscriptOrdinal)
	require.Equal(t, uint64(1), *toolResultEvent.AssistantTranscriptOrdinal)
	require.NotNil(t, toolResultEvent.SourceTruncated, "explicit metadata false must survive the producer path")
	require.False(t, *toolResultEvent.SourceTruncated)
	require.Len(t, toolOutputEvents, 2)
	byStream := make(map[string]core.AgentEvent, len(events))
	for _, event := range toolOutputEvents {
		require.Equal(t, "bash-stream-1", event.ToolCallID)
		require.Equal(t, runID, event.RunID)
		require.NotNil(t, event.CanonicalTranscriptIndex)
		require.Equal(t, uint64(1), *event.CanonicalTranscriptIndex)
		require.NotNil(t, event.AssistantTranscriptOrdinal)
		require.Equal(t, uint64(1), *event.AssistantTranscriptOrdinal)
		byStream[event.Stream] = event
	}
	require.Equal(t, "durable stdout", byStream["stdout"].Content)
	require.Equal(t, "durable stderr", byStream["stderr"].Content)

	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var intent session.SessionEntry
	var execution session.SessionEntry
	var receipt session.SessionEntry
	var terminal session.SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateIntent {
			intent = row
		}
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateExecution && row.CompletionProof != "" {
			execution = row
		}
		if row.Type == session.EntryTypeTool && row.ReceiptOutcome == core.ReceiptOutcomeSuccess {
			receipt = row
		}
		if row.CheckpointKind == session.CheckpointRunTerminal {
			terminal = row
		}
	}
	require.NotEmpty(t, intent.ID)
	require.Equal(t, tools.PreparedCompletionProof("owned_process_group_joined"), execution.CompletionProof)
	require.NotEmpty(t, receipt.ID)
	require.NotEmpty(t, terminal.ID)
	require.Equal(t, core.RunOutcomeSuccess, terminal.RunOutcome)
	decision, err := index.InspectDispatchMarkers(context.Background(), []session.PendingActionRef{{
		SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, RunID: intent.RunID,
		ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: *intent.AssistantTranscriptOrdinal,
		ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
	}})
	require.NoError(t, err)
	require.Len(t, decision, 1)
	require.Equal(t, session.DispatchMarkerResolved, decision[0].MarkerState)

	sessionID := prepared.View().SessionID
	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareResume(context.Background(), sessionID)
	require.NoError(t, err)
	defer reopened.Close()
	_, err = session.NewSessionRunCommitSink(reopened, index, workspaceID, nil)
	require.NoError(t, err, "reopen validates the proof-bearing streaming receipt and resolution")
}

func TestRunCommitLoopPublishesStructuredReadTruncationOnToolResult(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspaceID := "workspace-v1-truncated-read"
	_, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "truncated read")
	defer prepared.Close()
	readDir := filepath.Join(root, "read-input")
	require.NoError(t, os.Mkdir(readDir, 0o700))
	for index := 0; index < 1005; index++ {
		name := filepath.Join(readDir, fmt.Sprintf("entry-%04d.txt", index))
		require.NoError(t, os.WriteFile(name, nil, 0o600))
	}
	sourceProbe, err := tools.NewReadTool().Execute(context.Background(), tools.ToolCall{Args: map[string]any{"path": readDir}})
	require.NoError(t, err)
	require.True(t, sourceProbe.Metadata["truncated"].(bool)) //nolint:errcheck

	registry := newRunCommitStreamingRegistry(context.Background(), t, nil)
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("truncated-read", "truncated read",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "read-truncated-1", Name: "read", Args: map[string]any{"path": readDir},
		}}},
		mock.ConversationTurn{AssistantContent: "directory inspected"},
	))
	agent := core.NewAgentImpl("r2-truncated-read", core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry)))
	harness := core.NewHarnessImpl(agent)
	stream, err := harness.Submit(core.WithRunCommit(context.Background(), sink, core.RunCommitBinding{
		SessionID: prepared.View().SessionID, WorkspaceID: workspaceID,
	}), "inspect the directory")
	require.NoError(t, err)
	events := <-collectAgentEvents(stream)
	result, runErr := stream.Result()
	require.NoError(t, runErr)
	require.Equal(t, "directory inspected", result.Content)

	var receipt *core.AgentEvent
	for index := range events {
		if events[index].Kind == "tool_result" && events[index].ToolCallID == "read-truncated-1" {
			receipt = &events[index]
			break
		}
	}
	require.NotNil(t, receipt)
	require.Equal(t, core.ReceiptOutcomeSuccess, receipt.ReceiptOutcome)
	require.NotNil(t, receipt.SourceTruncated)
	require.True(t, *receipt.SourceTruncated, "only structured read metadata should establish source truncation")
}

func TestRunCommitLoopStreamsBashAlongsideNonStreamingToolInCanonicalOrder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspaceID := "workspace-v1-streaming-parallel-consumer"
	index, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "streaming parallel consumer")
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})
	inputPath := filepath.Join(root, "parallel-input.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("parallel read content"), 0o600))

	var policyCalls atomic.Int32
	registry := newRunCommitStreamingRegistry(context.Background(), t, &policyCalls)
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("streaming-parallel-consumer", "streaming parallel consumer",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{
			{ID: "bash-parallel-1", Name: "bash", Args: map[string]any{"command": "printf 'parallel stdout\\n'"}},
			{ID: "read-parallel-1", Name: "read", Args: map[string]any{"path": inputPath}},
		}},
		mock.ConversationTurn{AssistantContent: "parallel calls completed"},
	))
	loop := core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry), core.WithExecutionMode(core.ExecutionModeParallel))
	agent := core.NewAgentImpl("r2-streaming-parallel-session-consumer", loop)
	harness := core.NewHarnessImpl(agent)
	stream, err := harness.Submit(core.WithRunCommit(context.Background(), sink, core.RunCommitBinding{
		SessionID: prepared.View().SessionID, WorkspaceID: workspaceID,
	}), "run a parallel streaming command and read")
	require.NoError(t, err)
	events := <-collectToolOutputEvents(stream)
	result, runErr := stream.Result()
	require.NoError(t, runErr)
	require.Equal(t, "parallel calls completed", result.Content)
	require.Equal(t, int32(2), policyCalls.Load())
	require.Equal(t, 2, model.CallCount())
	require.Len(t, events, 1)
	require.Equal(t, "bash-parallel-1", events[0].ToolCallID)
	require.Equal(t, "parallel stdout", events[0].Content)

	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var receiptOrder []string
	for _, row := range snapshot.DurableEntries {
		if row.Type == session.EntryTypeTool && row.ReceiptOutcome == core.ReceiptOutcomeSuccess {
			receiptOrder = append(receiptOrder, row.ToolCallID)
		}
	}
	require.Equal(t, []string{"bash-parallel-1", "read-parallel-1"}, receiptOrder,
		"parallel receipts stay in the assistant's canonical call order")
	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareResume(context.Background(), prepared.View().SessionID)
	require.NoError(t, err)
	defer reopened.Close()
	_, err = session.NewSessionRunCommitSink(reopened, index, workspaceID, nil)
	require.NoError(t, err)
}

func TestRunCommitLoopRejectsUnattestedStreamingSinkBeforePolicyOrDispatch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspaceID := "workspace-v1-streaming-unattested-bus"
	_, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "unattested streaming bus")
	defer prepared.Close()
	commandPath := filepath.Join(root, "must-not-run.txt")

	var policyCalls atomic.Int32
	registry := newRunCommitStreamingRegistry(context.Background(), t, &policyCalls)
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("streaming-unattested-bus", "streaming unattested bus",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "bash-unattested-1", Name: "bash", Args: map[string]any{"command": "printf ran > " + commandPath},
		}}},
	))
	agent := core.NewAgentImpl("r2-streaming-unattested-consumer", core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry)))
	bus := &durableConsumerUnattestedEventBus{}
	harness := core.NewHarnessImpl(agent, core.WithHarnessEventBus(bus))
	stream, err := harness.Submit(core.WithRunCommit(context.Background(), sink, core.RunCommitBinding{
		SessionID: prepared.View().SessionID, WorkspaceID: workspaceID,
	}), "attempt streaming with an unknown bus")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, runErr := stream.Result()
	require.ErrorIs(t, runErr, core.ErrRunCommitStreamingUnsupported)
	require.Zero(t, policyCalls.Load(), "attestation is rejected before prepared policy side effects")
	require.Zero(t, bus.toolOutputPublished.Load())
	_, err = os.Stat(commandPath)
	require.ErrorIs(t, err, os.ErrNotExist, "unattested streaming is rejected before dispatch")
	require.Equal(t, 1, model.CallCount(), "a known streaming denial stops before another provider request")
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var hasIntent bool
	var receipt session.SessionEntry
	var terminal session.SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateIntent {
			hasIntent = true
		}
		if row.Type == session.EntryTypeTool && row.ReceiptOutcome == core.ReceiptOutcomeError {
			receipt = row
		}
		if row.CheckpointKind == session.CheckpointRunTerminal {
			terminal = row
		}
	}
	require.False(t, hasIntent, "unattested streaming is denied before reserving an action")
	require.Equal(t, "bash-unattested-1", receipt.ToolCallID)
	require.Equal(t, core.DispatchStateNotDispatched, receipt.DispatchState)
	require.Equal(t, core.RunOutcomeError, terminal.RunOutcome)
}

func (registry *sessionCapturingToolPreparer) PrepareToolCall(ctx context.Context, call tools.ToolCall) (tools.PreparedInvocation, error) {
	invocation, err := registry.MiddlewareToolRegistry.PrepareToolCall(ctx, call)
	if err == nil && invocation != nil {
		registry.signals <- invocation.CompletionSignal()
	}
	return invocation, err
}

func TestRunCommitLoopPersistsAndReopensWithSessionSink(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	inputPath := filepath.Join(workspace, "input.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("durable session evidence"), 0o600))

	ctx := context.Background()
	workspaceID := "workspace-v1-core-integration"
	index, err := session.NewWorkspaceQuarantineIndex()
	require.NoError(t, err)
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	assertWorkspaceIndexUsesTempHome(t, root, workspaceID)

	sessionDir := filepath.Join(root, "durable")
	preparer := session.NewDurableJSONLSessionPreparer(sessionDir, index)
	prepared, err := preparer.PrepareNew(ctx, "core consumer integration")
	require.NoError(t, err)
	sessionID := prepared.View().SessionID
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})

	sink, err := session.NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("r2-core-consumer", "R2 core consumer",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "read-session-1", Name: "read", Args: map[string]any{"path": inputPath},
		}}},
		mock.ConversationTurn{AssistantContent: "read persisted content"},
	))
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	require.NoError(t, tools.RegisterDefaults(ctx, deferred, tools.WithRegisteredBuiltinWhitelist([]string{"read"})))
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	agent := core.NewAgentImpl("r2-session-consumer", core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry)))
	harness := core.NewHarnessImpl(agent)
	binding := core.RunCommitBinding{SessionID: sessionID, WorkspaceID: workspaceID}
	stream, err := harness.Submit(core.WithRunCommit(ctx, sink, binding), "read the file")
	require.NoError(t, err)
	for range stream.Events() {
	}
	result, runErr := stream.Result()
	require.NoError(t, runErr)
	require.Equal(t, "read persisted content", result.Content)
	require.Equal(t, 2, model.CallCount(), "the next provider call follows a real durable receipt and resolution")

	snapshot, err := prepared.Snapshot()
	require.NoError(t, err, "owner lease release leaves the prepared session handle open")
	require.Equal(t, sessionID, snapshot.SessionID)
	require.Len(t, snapshot.PublishedView.Messages, 4)
	require.Len(t, snapshot.PublishedView.SourceEntryIDs, 4)
	ids := make(map[string]struct{}, len(snapshot.DurableEntries))
	var intent session.SessionEntry
	var execution session.SessionEntry
	var receipt session.SessionEntry
	var terminal session.SessionEntry
	for _, row := range snapshot.DurableEntries {
		ids[row.ID] = struct{}{}
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateIntent {
			intent = row
		}
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateExecution && row.CompletionProof != "" {
			execution = row
		}
		if row.Type == session.EntryTypeTool && row.ReceiptOutcome == core.ReceiptOutcomeSuccess {
			receipt = row
		}
		if row.CheckpointKind == session.CheckpointRunTerminal {
			terminal = row
		}
	}
	require.NotEmpty(t, intent.ID)
	require.NotEmpty(t, execution.ID)
	require.NotEmpty(t, receipt.ID)
	require.NotEmpty(t, terminal.ID)
	require.NotNil(t, receipt.TranscriptIndex)
	require.Equal(t, uint64(2), *receipt.TranscriptIndex)
	require.Equal(t, core.DispatchStateDispatched, receipt.DispatchState)
	require.Equal(t, core.RunOutcomeSuccess, terminal.RunOutcome)
	for _, sourceID := range snapshot.PublishedView.SourceEntryIDs {
		_, exists := ids[sourceID]
		require.True(t, exists, "each projected message must cite the durable row ID that supplied it")
	}
	decision, err := index.InspectDispatchMarkers(ctx, []session.PendingActionRef{{
		SessionID:                  sessionID,
		WorkspaceID:                workspaceID,
		RunID:                      intent.RunID,
		ActionBatchID:              intent.ActionBatchID,
		AssistantTranscriptOrdinal: *intent.AssistantTranscriptOrdinal,
		ToolCallID:                 intent.ToolCallID,
		ActionID:                   intent.ActionID,
	}})
	require.NoError(t, err)
	require.Len(t, decision, 1)
	require.Equal(t, session.DispatchMarkerResolved, decision[0].MarkerState)

	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := preparer.PrepareResume(ctx, sessionID)
	require.NoError(t, err)
	defer reopened.Close()
	reopenedSnapshot, err := reopened.Snapshot()
	require.NoError(t, err)
	require.Equal(t, snapshot.MaxSeq, reopenedSnapshot.MaxSeq)
	require.Equal(t, snapshot.PublishedView.SourceEntryIDs, reopenedSnapshot.PublishedView.SourceEntryIDs)
	require.Equal(t, snapshot.PublishedView.Messages, reopenedSnapshot.PublishedView.Messages)
	_, err = session.NewSessionRunCommitSink(reopened, index, workspaceID, nil)
	require.NoError(t, err, "the actual sink can validate the recovered terminal journal")
}

func TestRunCommitLoopKeepsMarkerAfterClosedCompletionWithoutProofAndReopen(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	inputPath := filepath.Join(workspace, "input.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte("reader completed before an attested wrapper panicked"), 0o600))

	ctx := context.Background()
	workspaceID := "workspace-v1-proofless-reopen"
	index, err := session.NewWorkspaceQuarantineIndex()
	require.NoError(t, err)
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))
	assertWorkspaceIndexUsesTempHome(t, root, workspaceID)
	sessionDir := filepath.Join(root, "durable")
	preparer := session.NewDurableJSONLSessionPreparer(sessionDir, index)
	prepared, err := preparer.PrepareNew(ctx, "closed completion without proof")
	require.NoError(t, err)
	sessionID := prepared.View().SessionID
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})
	sink, err := session.NewSessionRunCommitSink(prepared, index, workspaceID, nil)
	require.NoError(t, err)

	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	require.NoError(t, tools.RegisterDefaults(ctx, deferred, tools.WithRegisteredBuiltinWhitelist([]string{"read"})))
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	panicWrapper := tools.ToolExecutorWrapper(func(next func(context.Context, tools.ToolCall) (*tools.ToolResult, error)) func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
		return func(callCtx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			result, err := next(callCtx, call)
			if err != nil {
				return result, err
			}
			panic("untrusted completion wrapper panic payload")
		}
	})
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{
		ExecutionWrappers:            []tools.ToolExecutorWrapper{panicWrapper},
		SynchronousExecutionWrappers: true,
	}))

	probe, err := registry.PrepareToolCall(ctx, tools.ToolCall{
		ID: "read-proofless-probe", Name: "read", Args: map[string]any{"path": inputPath},
	})
	require.NoError(t, err)
	signal := probe.CompletionSignal()
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		_, _ = probe.Dispatch(ctx)
	}()
	require.True(t, panicked, "the synchronous wrapper panic must abort prepared dispatch")
	done := signal.Done()
	require.NotNil(t, done)
	select {
	case <-done:
	default:
		t.Fatal("a prepared wrapper panic must close its local execution channel")
	}
	require.Empty(t, signal.Proof(), "a wrapper panic invalidates the closed Done proof")

	model := mock.NewMockLLMServer(mock.NewConversationTemplate("proofless-run", "proofless run",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "read-proofless-1", Name: "read", Args: map[string]any{"path": inputPath},
		}}},
		mock.ConversationTurn{AssistantContent: "must not be requested"},
	))
	agent := core.NewAgentImpl("r2-proofless-session-consumer", core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry)))
	harness := core.NewHarnessImpl(agent)
	stream, err := harness.Submit(core.WithRunCommit(ctx, sink, core.RunCommitBinding{SessionID: sessionID, WorkspaceID: workspaceID}), "read a missing file")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, runErr := stream.Result()
	require.ErrorIs(t, runErr, core.ErrRunCommitQuiescenceUnproven)
	require.Equal(t, 1, model.CallCount(), "closed proofless completion keeps the later provider call fenced")

	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var intent session.SessionEntry
	var execution session.SessionEntry
	var receipt session.SessionEntry
	var terminal session.SessionEntry
	var resolutionCount int
	for _, row := range snapshot.DurableEntries {
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateIntent {
			intent = row
		}
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateExecution {
			execution = row
		}
		if row.Type == session.EntryTypeTool && row.ReceiptOutcome == core.ReceiptOutcomeUnknown {
			receipt = row
		}
		if row.CheckpointKind == session.CheckpointExecutionResolution {
			resolutionCount++
		}
		if row.CheckpointKind == session.CheckpointRunTerminal {
			terminal = row
		}
	}
	require.NotEmpty(t, intent.ID)
	require.NotEmpty(t, execution.ID)
	require.Empty(t, execution.CompletionProof)
	require.Equal(t, core.RunExecutionCleanupPending, execution.ResourceState)
	require.NotEmpty(t, receipt.ID)
	require.Zero(t, resolutionCount, "Done closure with no proof cannot append an execution resolution")
	require.NotEmpty(t, terminal.ID)
	require.Equal(t, core.RunOutcomeUnknown, terminal.RunOutcome)

	ref := session.PendingActionRef{
		SessionID: sessionID, WorkspaceID: workspaceID, RunID: intent.RunID,
		ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: *intent.AssistantTranscriptOrdinal,
		ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
	}
	decisions, err := index.InspectDispatchMarkers(ctx, []session.PendingActionRef{ref})
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.Equal(t, session.DispatchMarkerPresent, decisions[0].MarkerState)

	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := preparer.PrepareResume(ctx, sessionID)
	require.NoError(t, err)
	defer reopened.Close()
	reopenedSnapshot, err := reopened.Snapshot()
	require.NoError(t, err)
	require.Equal(t, core.ReceiptOutcomeUnknown, reopenedSnapshot.PublishedView.Messages[len(reopenedSnapshot.PublishedView.Messages)-1].ReceiptOutcome, "reopen must keep the unresolved occurrence visible only as unknown")
	reopenedDecisions, err := index.InspectDispatchMarkers(ctx, []session.PendingActionRef{ref})
	require.NoError(t, err)
	require.Equal(t, session.DispatchMarkerPresent, reopenedDecisions[0].MarkerState, "restart cannot clear a marker based only on closed Done")
	_, err = session.NewSessionRunCommitSink(reopened, index, workspaceID, nil)
	require.NoError(t, err, "the actual sink must validate and retain the recovered unresolved journal")
}

func TestRunCommitLoopPersistsLateProofDuringReceiptBeforeResolution(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o700))
	outputPath := filepath.Join(workspace, "output.txt")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workspaceID := "workspace-v1-receipt-ready-race"
	index, err := session.NewWorkspaceQuarantineIndex()
	require.NoError(t, err)
	require.NoError(t, index.EnsureWorkspace(context.Background(), workspaceID))
	assertWorkspaceIndexUsesTempHome(t, root, workspaceID)
	preparer := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index)
	prepared, err := preparer.PrepareNew(context.Background(), "receipt ready proof race")
	require.NoError(t, err)
	preparedOpen := true
	t.Cleanup(func() {
		if preparedOpen {
			_ = prepared.Close()
		}
	})
	statusHandles := make(chan core.RunStatusHandle, 1)
	completionSignals := make(chan tools.PreparedCompletionSignal, 1)
	receiptAppend := make(chan struct{})
	writer := &receiptGateSessionWriter{
		base:          prepared.DurableWriter(),
		statusHandles: statusHandles,
		signals:       completionSignals,
		receiptAppend: receiptAppend,
	}
	preparedForSink := &receiptGatedPreparedSession{PreparedSession: prepared, writer: writer}
	sink, err := session.NewSessionRunCommitSink(preparedForSink, index, workspaceID, nil)
	require.NoError(t, err)

	queue := tools.NewDefaultFileMutationQueue().(*tools.DefaultFileMutationQueue) //nolint:errcheck
	t.Cleanup(func() { _ = queue.Close() })
	toolEntered := make(chan struct{}, 1)
	var cancelOnce sync.Once
	toolWrapper := tools.ToolExecutorWrapper(func(next func(context.Context, tools.ToolCall) (*tools.ToolResult, error)) func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
		return func(toolCtx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			select {
			case toolEntered <- struct{}{}:
			default:
			}
			cancelOnce.Do(cancel)
			// This test needs completion to become visible during receipt sync,
			// not before the first execution row is sampled. Cancellation alone
			// does not prevent a fast queued write from finishing immediately.
			select {
			case <-receiptAppend:
			case <-time.After(3 * time.Second):
				return nil, errors.New("test: receipt append did not release the queued write")
			}
			return next(context.WithoutCancel(toolCtx), call)
		}
	})
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	require.NoError(t, tools.RegisterDefaults(ctx, deferred, tools.WithRegisteredBuiltinWhitelist([]string{"write"})))
	middlewareRegistry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, middlewareRegistry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, middlewareRegistry.SetPreparedToolStages(tools.PreparedToolStages{
		MutationQueue:                queue,
		ExecutionWrappers:            []tools.ToolExecutorWrapper{toolWrapper},
		SynchronousExecutionWrappers: true,
	}))
	registry := &sessionCapturingToolPreparer{MiddlewareToolRegistry: middlewareRegistry, signals: completionSignals}
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("receipt-proof-race", "receipt proof race",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "write-receipt-race", Name: "write", Args: map[string]any{"path": outputPath, "content": "queued content"},
		}}},
		mock.ConversationTurn{AssistantContent: "must not be requested after cancellation"},
	))
	agent := core.NewAgentImpl("receipt-proof-race", core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry)))
	harness := core.NewHarnessImpl(agent)
	stream, err := harness.Submit(core.WithRunCommit(ctx, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID}), "write output")
	require.NoError(t, err)
	statusSource, ok := stream.(core.RunStatusSource)
	require.True(t, ok)
	statusHandles <- statusSource.RunStatusHandle()
	eventsDone := make(chan struct{})
	go func() {
		defer close(eventsDone)
		for range stream.Events() {
		}
	}()
	select {
	case <-toolEntered:
	case <-time.After(3 * time.Second):
		<-eventsDone
		_, runErr := stream.Result()
		t.Fatalf("prepared write wrapper did not enter; run error: %v", runErr)
	}
	select {
	case <-receiptAppend:
	case <-time.After(3 * time.Second):
		<-eventsDone
		_, runErr := stream.Result()
		t.Fatalf("session writer did not reach its gated receipt append; run error: %v", runErr)
	}
	<-eventsDone
	_, runErr := stream.Result()
	if errors.Is(runErr, core.ErrRunCommitQuiescenceUnproven) {
		t.Fatalf("known local proof should be persisted before resolution, got: %v", runErr)
	}
	require.Equal(t, 1, model.CallCount(), "cancellation prevents a later provider request")

	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var intent session.SessionEntry
	var initialExecution session.SessionEntry
	var receipt session.SessionEntry
	var proofExecution session.SessionEntry
	var terminal session.SessionEntry
	var resolution session.SessionEntry
	var proofExecutionCount int
	for _, row := range snapshot.DurableEntries {
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateIntent {
			intent = row
		}
		if row.Type == session.EntryTypeTool && row.ReceiptOutcome == core.ReceiptOutcomeUnknown {
			receipt = row
		}
		if row.Type == session.EntryTypeTool && row.ActionState == session.ActionStateExecution {
			if row.CompletionProof == "" {
				initialExecution = row
			} else {
				proofExecution = row
				proofExecutionCount++
			}
		}
		if row.CheckpointKind == session.CheckpointExecutionResolution {
			resolution = row
		}
		if row.CheckpointKind == session.CheckpointRunTerminal {
			terminal = row
		}
	}
	require.NotEmpty(t, intent.ID)
	require.NotEmpty(t, initialExecution.ID)
	require.NotEmpty(t, receipt.ID)
	require.NotEmpty(t, proofExecution.ID)
	require.Equal(t, 1, proofExecutionCount, "the receipt-time recheck writes one positive execution row")
	require.NotEmpty(t, resolution.ID)
	require.NotEmpty(t, terminal.ID)
	require.Equal(t, core.RunExecutionCleanupPending, initialExecution.ResourceState, "the initial execution write precedes actual resource completion")
	require.Empty(t, initialExecution.CompletionProof)
	require.Equal(t, core.ReceiptOutcomeUnknown, receipt.ReceiptOutcome, "late local proof does not rewrite a known-unknown tool outcome")
	require.Less(t, initialExecution.Seq, receipt.Seq)
	require.Less(t, receipt.Seq, proofExecution.Seq, "receipt callback gates until owner readiness; late proof is appended after receipt")
	require.Less(t, proofExecution.Seq, resolution.Seq, "session resolution follows the durable positive proof row")
	require.Equal(t, core.RunExecutionQuiescent, proofExecution.ResourceState)
	require.Equal(t, core.RunOutcomeUnknown, terminal.RunOutcome, "unknown receipt outcome is preserved independently from local proof")
	require.Equal(t, intent.ActionID, proofExecution.ActionID)
	require.Equal(t, intent.ActionID, resolution.ActionID)

	ref := session.PendingActionRef{
		SessionID: prepared.View().SessionID, WorkspaceID: workspaceID, RunID: intent.RunID,
		ActionBatchID: intent.ActionBatchID, AssistantTranscriptOrdinal: *intent.AssistantTranscriptOrdinal,
		ToolCallID: intent.ToolCallID, ActionID: intent.ActionID,
	}
	decisions, err := index.InspectDispatchMarkers(context.Background(), []session.PendingActionRef{ref})
	require.NoError(t, err)
	require.Len(t, decisions, 1)
	require.Equal(t, session.DispatchMarkerResolved, decisions[0].MarkerState)

	require.NoError(t, prepared.Close())
	preparedOpen = false
	reopened, err := preparer.PrepareResume(context.Background(), prepared.View().SessionID)
	require.NoError(t, err)
	defer reopened.Close()
	_, err = reopened.Snapshot()
	require.NoError(t, err, "restart accepts the receipt-time proof row before its resolution")
}

func assertWorkspaceIndexUsesTempHome(t *testing.T, root, workspaceID string) {
	t.Helper()
	configRoot, err := os.UserConfigDir()
	require.NoError(t, err)
	relative, err := filepath.Rel(root, configRoot)
	require.NoError(t, err)
	require.False(t, relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)), "workspace index root must be inside the test's temporary home")
	indexPath := filepath.Join(configRoot, "go-cli", "workspace-quarantine", workspaceID+".jsonl")
	_, err = os.Stat(indexPath)
	require.NoError(t, err, "the synthetic marker index must be rooted in the test's temporary home")
}
