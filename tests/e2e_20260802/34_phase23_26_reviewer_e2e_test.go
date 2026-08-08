//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phases 23-26: multi-agent orchestration, sandbox
// resource limits, FileMutationQueue lifecycle, and steer integration.
package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// Phase 23 E2E: Multi-Agent Orchestration
// =============================================================================

// TestET_Phase23_ParallelDispatch verifies that ParallelDispatch runs multiple
// sub-agent tasks concurrently and returns results in input order.
//
// Test ID: ET-Phase23-S1
// Task ref: tasks.json#23-1
// Feature: multi-agent parallel dispatch
func TestET_Phase23_ParallelDispatch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"p23", "parallel-dispatch",
		mock.ConversationTurn{AssistantContent: "parallel result"},
		mock.ConversationTurn{AssistantContent: "parallel result"},
	))
	registerRealSubAgentFactory(t, model)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dispatcher := core.NewDefaultSubagentDispatcher(nil)
	tasks := []core.SubagentTask{
		{ID: "task-a", Prompt: "do task A", MaxTurns: 1},
		{ID: "task-b", Prompt: "do task B", MaxTurns: 1},
	}

	results, err := dispatcher.ParallelDispatch(ctx, tasks)
	require.NoError(t, err)
	require.Len(t, results, 2)

	// Results must preserve input order.
	assert.Equal(t, "task-a", results[0].TaskID)
	assert.Equal(t, "task-b", results[1].TaskID)

	// Both sub-agents should return real LLM content (not simulated placeholders).
	for i, r := range results {
		assert.NoError(t, r.Error, "task %s should not error", r.TaskID)
		assert.Equal(t, "parallel result", r.Content,
			"task %s should return mock LLM content", r.TaskID)
		assert.NotEqual(t, "response-1", r.Content,
			"task %s must not return simulated placeholder", r.TaskID)
		_ = i
	}
}

// TestET_Phase23_InboxConsumption verifies that a message sent via Send before
// Run is drained from the inbox by the simulated runner and emitted as a "user"
// event in the event stream.
//
// Test ID: ET-Phase23-S2
// Task ref: tasks.json#23-2
// Feature: sub-agent inbox consumption
func TestET_Phase23_InboxConsumption(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub := core.NewDefaultSubAgent(core.SubAgentConfig{
		Name:     "inbox-test",
		MaxTurns: 3,
	})

	// Send a follow-up message before Run. It lands in the inbox buffer and
	// is drained on the first turn iteration via pumpInbox.
	require.NoError(t, sub.Send(ctx, "follow-up message"))

	evCh, err := sub.Run(ctx, "initial prompt")
	require.NoError(t, err)

	var events []core.AgentEvent
	for ev := range evCh {
		events = append(events, ev)
	}

	// The sent message must appear as a "user" event (pumpInbox emits inbox
	// messages as user events).
	var foundFollowUp bool
	for _, ev := range events {
		if ev.Kind == "user" && ev.Content == "follow-up message" {
			foundFollowUp = true
			break
		}
	}
	assert.True(t, foundFollowUp,
		"sent message should be consumed from inbox and emitted as a user event")

	// Received() should include the sent message.
	ds, ok := sub.(*core.DefaultSubAgent)
	require.True(t, ok, "sub should be *DefaultSubAgent")
	assert.Contains(t, ds.Received(), "follow-up message")

	// The runner should have produced response events.
	var responseCount int
	for _, ev := range events {
		if ev.Kind == "message" {
			responseCount++
		}
	}
	assert.Equal(t, 3, responseCount, "should have 3 response messages (MaxTurns=3)")
}

// TestET_Phase23_ToolFiltering verifies that when a sub-agent task specifies
// Tools: ["alpha"], only the "alpha" tool is accessible; the "beta" tool is
// filtered out and cannot be executed.
//
// Test ID: ET-Phase23-S3
// Task ref: tasks.json#23-3
// Feature: sub-agent tool filtering
func TestET_Phase23_ToolFiltering(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Build a registry with two recording tools.
	tr := tools.NewDefaultToolRegistry()
	alphaTool := &recordingTool{name: "alpha"}
	betaTool := &recordingTool{name: "beta"}
	require.NoError(t, tr.Register(ctx, alphaTool))
	require.NoError(t, tr.Register(ctx, betaTool))

	// Mock LLM: turn 1 calls "alpha" (allowed), turn 2 calls "beta" (filtered
	// out), turn 3 finishes with plain text.
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"filter", "tool-filter",
		mock.ConversationTurn{
			AssistantContent: "calling alpha",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "c1", Name: "alpha", Args: map[string]any{}},
			},
		},
		mock.ConversationTurn{
			AssistantContent: "calling beta",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "c2", Name: "beta", Args: map[string]any{}},
			},
		},
		mock.ConversationTurn{AssistantContent: "done"},
	))

	// Register a real factory that uses our tool registry.
	orig := core.GetSubAgentFactory()
	t.Cleanup(func() { core.RegisterSubAgentFactory(orig) })
	core.RegisterSubAgentFactory(core.NewRealSubAgentFactory(model, nil, tr))

	dispatcher := core.NewDefaultSubagentDispatcher(nil)
	result, err := dispatcher.Dispatch(ctx, core.SubagentTask{
		ID:       "filter-test",
		Prompt:   "use tools",
		Tools:    []string{"alpha"}, // only "alpha" is allowed
		MaxTurns: 3,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, result.Content)

	// "alpha" is in the allowed list and should have been executed.
	assert.True(t, alphaTool.executed,
		"alpha tool should be executed (it is in the allowed tools list)")
	// "beta" is NOT in the allowed list; the filtered registry should have
	// rejected the lookup, so beta.Execute was never called.
	assert.False(t, betaTool.executed,
		"beta tool should NOT be executed (it is filtered out)")
}

// =============================================================================
// Phase 24 E2E: Sandbox Resource Limits
// =============================================================================

// TestET_Phase24_WhitelistBlocksOutsidePaths verifies that a BashSandbox with
// WithAllowedPaths rejects commands whose working directory falls outside the
// whitelisted base paths.
//
// Test ID: ET-Phase24-S1
// Task ref: tasks.json#24-1
// Feature: sandbox path whitelist
func TestET_Phase24_WhitelistBlocksOutsidePaths(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	allowedDir := t.TempDir()
	outsideDir := t.TempDir()

	sandbox := tools.NewDefaultBashSandbox(
		tools.WithAllowedPaths([]string{allowedDir}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A command run from outside the whitelist must be rejected.
	err := sandbox.Validate(ctx, "ls", outsideDir)
	assert.Error(t, err, "command in outside directory should be rejected")

	// A command run from within the whitelist must pass.
	err = sandbox.Validate(ctx, "ls", allowedDir)
	assert.NoError(t, err, "command in allowed directory should pass")

	// A subdirectory of the allowed path must also pass.
	subDir := filepath.Join(allowedDir, "sub")
	err = sandbox.Validate(ctx, "ls", subDir)
	assert.NoError(t, err, "command in subdirectory of allowed path should pass")
}

// TestET_Phase24_CommandBlacklist verifies that the default BashSandbox
// blacklist rejects destructive commands like "rm" and "kill" while allowing
// benign commands like "ls".
//
// Test ID: ET-Phase24-S2
// Task ref: tasks.json#24-2
// Feature: sandbox command blacklist
func TestET_Phase24_CommandBlacklist(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sandbox := tools.NewDefaultBashSandbox() // default blacklist

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Destructive commands must be blocked.
	blocked := []string{
		"rm -rf /tmp/test",
		"kill -9 1",
		"pkill bash",
		"rmdir /tmp/test",
	}
	for _, cmd := range blocked {
		err := sandbox.Validate(ctx, cmd, "/tmp")
		assert.Error(t, err, "command %q should be blocked by default blacklist", cmd)
	}

	// Benign commands must pass.
	allowed := []string{
		"ls -la",
		"echo hello",
		"cat /etc/hostname",
	}
	for _, cmd := range allowed {
		err := sandbox.Validate(ctx, cmd, "/tmp")
		assert.NoError(t, err, "command %q should be allowed", cmd)
	}
}

// TestET_Phase24_TimeoutTierClassification verifies that WithTimeoutTier(true)
// enables per-command timeout classification and that commands of different
// tiers (fast, normal) execute successfully.
//
// Test ID: ET-Phase24-S3
// Task ref: tasks.json#24-3
// Feature: bash timeout tier classification
func TestET_Phase24_TimeoutTierClassification(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	workDir := t.TempDir()
	tool := tools.NewBashTool(
		tools.WithTimeoutTier(true),
		tools.WithBashWorkdir(workDir),
	)

	// The option must set the field.
	assert.True(t, tool.TimeoutTier, "WithTimeoutTier(true) should set TimeoutTier field")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// A fast-tier command (echo is in fastCommands) should execute successfully.
	result, err := tool.Execute(ctx, tools.ToolCall{
		ID:   "tc-fast",
		Name: "bash",
		Args: map[string]any{"command": "echo hello"},
	})
	require.NoError(t, err, "fast-tier command should succeed")
	require.NotNil(t, result)
	assert.Contains(t, result.Output, "hello")

	// A normal-tier command (not in fastCommands or slowCommands) should also
	// execute successfully, proving the classifier does not break execution
	// for unrecognised commands.
	result2, err := tool.Execute(ctx, tools.ToolCall{
		ID:   "tc-normal",
		Name: "bash",
		Args: map[string]any{"command": "printf normal"},
	})
	require.NoError(t, err, "normal-tier command should succeed")
	require.NotNil(t, result2)
	assert.Contains(t, result2.Output, "normal")

	// Verify without TimeoutTier the field is false (default).
	plainTool := tools.NewBashTool()
	assert.False(t, plainTool.TimeoutTier, "default BashTool should have TimeoutTier=false")
}

// =============================================================================
// Phase 25 E2E: FileMutationQueue
// =============================================================================

// TestET_Phase25_PerFileSerialization verifies that mutations targeting the
// same file are processed in FIFO order by the per-file worker.
//
// Test ID: ET-Phase25-S1
// Task ref: tasks.json#25-1
// Feature: file mutation queue per-file serialization
func TestET_Phase25_PerFileSerialization(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	filePath := filepath.Join(t.TempDir(), "same.txt")

	var mu sync.Mutex
	var order []int
	handler := func(_ context.Context, m tools.FileMutation) (*tools.ToolResult, error) {
		mu.Lock()
		order = append(order, m.Content.(int))
		mu.Unlock()
		time.Sleep(10 * time.Millisecond) // simulate work
		return nil, nil
	}

	q := tools.NewDefaultFileMutationQueue(tools.WithMutationHandler(handler))
	cq := q.(*tools.DefaultFileMutationQueue)
	defer cq.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Enqueue three mutations to the same file sequentially.
	var resultChans []<-chan tools.FileMutationResult
	for i := 0; i < 3; i++ {
		ch, err := q.Enqueue(ctx, tools.FileMutation{
			FilePath:  filePath,
			Operation: "write",
			Content:   i,
			ToolName:  "test",
		})
		require.NoError(t, err)
		resultChans = append(resultChans, ch)
	}

	// Wait for all results.
	for i, ch := range resultChans {
		res := <-ch
		assert.True(t, res.Success, "mutation %d should succeed", i)
		assert.NoError(t, res.Error)
	}

	// Verify FIFO order.
	mu.Lock()
	assert.Equal(t, []int{0, 1, 2}, order,
		"mutations to the same file should be processed in FIFO order")
	mu.Unlock()
}

// TestET_Phase25_ConfiguredHandlerPreservesFileTracker verifies that a
// FileMutationQueue configured with WithMutationFileTracker creates backup
// checkpoints when applying write mutations.
//
// Test ID: ET-Phase25-S2
// Task ref: tasks.json#25-2
// Feature: mutation queue file tracker integration
func TestET_Phase25_ConfiguredHandlerPreservesFileTracker(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ft := tools.NewFileTracker()
	q := tools.NewDefaultFileMutationQueue(tools.WithMutationFileTracker(ft))
	cq := q.(*tools.DefaultFileMutationQueue)
	defer cq.Close() //nolint:errcheck

	filePath := filepath.Join(t.TempDir(), "tracked.txt")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First write creates the file; the configured handler should create a
	// checkpoint via the FileTracker-backed WriteTool.
	ch, err := q.Enqueue(ctx, tools.FileMutation{
		FilePath:  filePath,
		Operation: "write",
		Content:   "initial content",
		ToolName:  "write",
	})
	require.NoError(t, err)

	res := <-ch
	require.True(t, res.Success, "write mutation should succeed")
	require.NoError(t, res.Error)

	// The FileTracker should have at least one checkpoint.
	checkpoints := ft.ListCheckpoints()
	assert.NotEmpty(t, checkpoints, "FileTracker should have at least one checkpoint")
	if len(checkpoints) > 0 {
		assert.Equal(t, filePath, checkpoints[0].Path)
	}
}

// TestET_Phase25_LifecycleClose verifies that Close shuts down worker
// goroutines, that a second Close returns an error, and that Enqueue on a
// closed queue fails.
//
// Test ID: ET-Phase25-S3
// Task ref: tasks.json#25-3
// Feature: mutation queue lifecycle close
func TestET_Phase25_LifecycleClose(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	filePath := filepath.Join(t.TempDir(), "lifecycle.txt")

	handler := func(_ context.Context, _ tools.FileMutation) (*tools.ToolResult, error) {
		return nil, nil
	}
	q := tools.NewDefaultFileMutationQueue(tools.WithMutationHandler(handler))
	cq := q.(*tools.DefaultFileMutationQueue)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Enqueue a mutation and wait for it to complete (proves the queue works).
	ch, err := q.Enqueue(ctx, tools.FileMutation{
		FilePath:  filePath,
		Operation: "write",
		Content:   "data",
		ToolName:  "write",
	})
	require.NoError(t, err)
	res := <-ch
	require.True(t, res.Success)

	// Close the queue — first close should succeed.
	require.NoError(t, cq.Close(), "first Close should succeed")

	// Enqueue on a closed queue must fail.
	_, err = q.Enqueue(ctx, tools.FileMutation{
		FilePath:  filePath,
		Operation: "write",
		Content:   "more data",
		ToolName:  "write",
	})
	assert.Error(t, err, "Enqueue on closed queue should fail")

	// Second Close should fail.
	err = cq.Close()
	assert.Error(t, err, "second Close should fail")
}

// =============================================================================
// Phase 26 E2E: Steer Integration
// =============================================================================

// etBlockingTurnLoop is a core.AgentLoop that blocks until its release channel
// is closed or the context is cancelled. It signals started before blocking,
// allowing tests to deterministically interact with a running turn.
type etBlockingTurnLoop struct {
	started chan struct{}
	release chan struct{}
}

func (l *etBlockingTurnLoop) Run(ctx context.Context, _ core.Submission, _ ...core.EventStream) ([]core.AgentEvent, error) {
	close(l.started)
	select {
	case <-ctx.Done():
		return []core.AgentEvent{}, ctx.Err()
	case <-l.release:
		return []core.AgentEvent{{Kind: "message", Content: "ok", Timestamp: time.Now()}}, nil
	}
}

var _ core.AgentLoop = (*etBlockingTurnLoop)(nil)

// TestET_Phase26_SteerInjection verifies that EinoTurnRunner.Steer sends a
// steering instruction to the loop's steering channel, which is picked up
// between LLM iterations and injected as a user message. It also verifies the
// steering is recorded on the Turn via Get.
//
// Test ID: ET-Phase26-S1
// Task ref: tasks.json#26-1
// Feature: steer injection into running turn
func TestET_Phase26_SteerInjection(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Mock LLM: turn 1 calls the blocking tool, turn 2 finishes.
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"steer", "steer-injection",
		mock.ConversationTurn{
			AssistantContent: "calling tool",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "c1", Name: "block", Args: map[string]any{}},
			},
		},
		mock.ConversationTurn{AssistantContent: "done"},
	))

	// Blocking tool that signals started and blocks until released.
	started := make(chan struct{})
	release := make(chan struct{})
	blockTool := &recordingTool{
		name: "block",
		execute: func(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
			close(started)
			select {
			case <-release:
				return &tools.ToolResult{Output: "ok"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(ctx, blockTool))

	steerCh := make(chan string, 1)
	loop := core.NewLoopAgent(
		core.WithLLM(model),
		core.WithTools(tr),
		core.WithSteeringChannel(steerCh),
	)
	runner := core.NewEinoTurnRunner(loop)
	runner.SetSteerChannel(steerCh)

	// Start the turn in a goroutine.
	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, runErr = runner.RunTurn(ctx, core.Submission{Content: "go"})
	}()

	// Wait for the blocking tool to start (first LLM iteration is in progress).
	<-started

	// Poll for the running turn ID.
	var turnID string
	require.Eventually(t, func() bool {
		turnID = runner.RunningTurnID()
		return turnID != ""
	}, 2*time.Second, time.Millisecond)
	require.NotEmpty(t, turnID)

	// Steer through the TurnRunner interface.
	require.NoError(t, runner.Steer(ctx, turnID, "change direction"))

	// Release the blocking tool so the loop proceeds to the next iteration.
	close(release)

	// Wait for RunTurn to finish.
	<-done
	require.NoError(t, runErr)

	// The steering message should have been injected into the LLM conversation
	// as a user message before the second LLM call.
	logs := model.CallLog()
	require.Len(t, logs, 2, "model should have been called twice")
	var foundSteer bool
	for _, msg := range logs[1].Messages {
		if msg.Role == llm.RoleUser && msg.Content == "change direction" {
			foundSteer = true
			break
		}
	}
	assert.True(t, foundSteer,
		"steering message should be injected as user message before second LLM call")

	// The turn should also record the steering.
	turn, err := runner.Get(ctx, turnID)
	require.NoError(t, err)
	require.Len(t, turn.Steerings, 1)
	assert.Equal(t, "change direction", turn.Steerings[0].Content)
}

// TestET_Phase26_CancelTurn verifies that EinoTurnRunner.Cancel cancels a
// running turn's context, causing the loop to return with context.Canceled and
// the turn status to become TurnCanceled.
//
// Test ID: ET-Phase26-S2
// Task ref: tasks.json#26-2
// Feature: turn cancellation
func TestET_Phase26_CancelTurn(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	bl := &etBlockingTurnLoop{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	runner := core.NewEinoTurnRunner(bl)

	// Start the turn in a goroutine.
	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, runErr = runner.RunTurn(ctx, core.Submission{Content: "go"})
	}()

	// Wait for the loop to start blocking.
	<-bl.started

	// Poll for the running turn ID.
	var turnID string
	require.Eventually(t, func() bool {
		turnID = runner.RunningTurnID()
		return turnID != ""
	}, 2*time.Second, time.Millisecond)
	require.NotEmpty(t, turnID)

	// Cancel the running turn.
	require.NoError(t, runner.Cancel(ctx, turnID))

	// Wait for RunTurn to finish.
	<-done

	// The run should have failed with context.Canceled.
	require.ErrorIs(t, runErr, context.Canceled)

	// The turn status should be TurnCanceled.
	turn, err := runner.Get(ctx, turnID)
	require.NoError(t, err)
	assert.Equal(t, core.TurnCanceled, turn.Status)
	assert.True(t, turn.Canceled, "turn should be marked as canceled")
}

// TestET_Phase26_InterruptHandlerSendSteer verifies that InterruptHandler.SendSteer
// delivers a steering message to the SteerChannel, which the REPL loop would
// forward to the running agent loop.
//
// Test ID: ET-Phase26-S3
// Task ref: tasks.json#26-3
// Feature: interrupt handler steer delivery
func TestET_Phase26_InterruptHandlerSendSteer(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	handler := cli.NewInterruptHandler(cancel)
	handler.Start(nil)
	defer handler.Stop()

	// Send a steering message.
	require.NoError(t, handler.SendSteer("redirect the agent"))

	// The message must be received on SteerChannel.
	select {
	case msg := <-handler.SteerChannel():
		assert.Equal(t, "redirect the agent", msg)
	case <-time.After(2 * time.Second):
		t.Fatal("steer message not received on SteerChannel")
	}

	// The channel should now be empty.
	select {
	case v, ok := <-handler.SteerChannel():
		if ok {
			t.Fatalf("SteerChannel should be empty after receive, got %q", v)
		}
	default:
		// No message ready — expected.
	}

	_ = ctx // ctx retained for potential future use
}
