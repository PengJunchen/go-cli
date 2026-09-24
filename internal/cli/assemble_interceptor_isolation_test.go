package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
)

func TestAssemblyInterceptorsAreIsolated(t *testing.T) {
	core.ClearToolInterceptors()
	t.Cleanup(core.ClearToolInterceptors)

	newAssembly := func(name, workdir string) *AgentAssembly {
		t.Helper()
		cfg := newAssembleTestConfig()
		cfg.Git.WorkDir = workdir
		assembly, err := AssembleAgent(
			context.Background(),
			cfg,
			"openai",
			"test-model",
			io.Discard,
			WithAgentName(name),
			WithApproveMode(ApproveAuto),
		)
		require.NoError(t, err)
		t.Cleanup(assembly.Cleanup)
		return assembly
	}

	workdirA := t.TempDir()
	workdirB := t.TempDir()
	assemblyA := newAssembly("plan-owner", workdirA)
	assemblyB := newAssembly("independent-agent", workdirB)
	require.NoError(t, assemblyA.PlanCtrl.Enter(context.Background(), "assembly A is planning"))
	assert.False(t, assemblyB.PlanCtrl.IsActive(), "assembly B must retain its own inactive Plan Mode state")

	assemblyA.LoopAgent.SetModel(mock.NewMockLLMServer(mock.NewConversationTemplate(
		"R1", "assembly-a-plan-interceptor",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "assembly-a-call", Name: "write", Args: map[string]any{"path": filepath.Join(workdirA, "blocked.txt"), "content": "must not appear"},
		}}},
		mock.ConversationTurn{AssistantContent: "plan continues"},
	)))
	assemblyB.LoopAgent.SetModel(mock.NewMockLLMServer(mock.NewConversationTemplate(
		"R1", "assembly-b-independent-interceptor",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "assembly-b-call", Name: "write", Args: map[string]any{"path": filepath.Join(workdirB, "allowed.txt"), "content": "assembly B write"},
		}}},
		mock.ConversationTurn{AssistantContent: "done"},
	)))

	var wg sync.WaitGroup
	type runResult struct {
		name   string
		events []core.AgentEvent
		err    error
	}
	results := make(chan runResult, 2)
	for _, item := range []struct {
		name     string
		assembly *AgentAssembly
		content  string
	}{
		{name: "A", assembly: assemblyA, content: "run assembly A's write while planning"},
		{name: "B", assembly: assemblyB, content: "run assembly B's write"},
	} {
		item := item
		wg.Add(1)
		go func() {
			defer wg.Done()
			events, err := item.assembly.LoopAgent.Run(context.Background(), core.Submission{
				Type:    core.SubmissionUserMessage,
				Content: item.content,
			})
			results <- runResult{name: item.name, events: events, err: err}
		}()
	}
	wg.Wait()
	close(results)
	var resultA, resultB runResult
	for result := range results {
		require.NoError(t, result.err)
		if result.name == "A" {
			resultA = result
		} else {
			resultB = result
		}
	}

	_, aStatErr := os.Stat(filepath.Join(workdirA, "blocked.txt"))
	assert.ErrorIs(t, aStatErr, os.ErrNotExist, "Plan Mode must cancel assembly A's actual write before execution")
	var aCanceledCallIDs []string
	for _, event := range resultA.events {
		if event.Kind == "tool_canceled" {
			aCanceledCallIDs = append(aCanceledCallIDs, event.ToolCallID)
		}
	}
	assert.Equal(t, []string{"assembly-a-call"}, aCanceledCallIDs,
		"the local Plan interceptor must preserve the tool_canceled event")

	bFile, bReadErr := os.ReadFile(filepath.Join(workdirB, "allowed.txt"))
	require.NoError(t, bReadErr, "assembly B's write must execute under its own inactive Plan Mode")
	assert.Equal(t, "assembly B write", string(bFile))
	assert.NotContains(t, eventKinds(resultB.events), "tool_canceled",
		"assembly A's Plan Mode must not cancel assembly B's tool call")
}

func eventKinds(events []core.AgentEvent) []string {
	kinds := make([]string, len(events))
	for i, event := range events {
		kinds[i] = event.Kind
	}
	return kinds
}
