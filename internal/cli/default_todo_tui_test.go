package cli

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestDefaultAssemblyTodoMutationBetweenRenderAndBindIsRecovered(t *testing.T) {
	ctx := context.Background()
	assembly, err := AssembleAgent(ctx, newAssembleTestConfig(t), "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	tool, err := assembly.ToolRegistry.Get(ctx, "todo_write")
	require.NoError(t, err)

	repl := &REPLSession{assembly: assembly, spanCtx: ctx, logger: slog.Default()}
	repl.turnCtx, repl.cancelTurn = context.WithCancel(ctx)
	repl.stream = core.NewEventStream(8)
	require.NoError(t, repl.renderTUI(nil))
	t.Cleanup(repl.stopTUI)
	_, err = tool.Execute(ctx, tools.ToolCall{Name: "todo_write", Args: map[string]any{
		"action": "add", "content": "mutation in startup gap",
	}})
	require.NoError(t, err)

	repl.bindTodoUpdates()
	require.Eventually(t, func() bool {
		return strings.Contains(repl.app.View(), "mutation in startup gap") && strings.Contains(repl.app.View(), "Todos [0/1 completed]")
	}, 2*time.Second, 5*time.Millisecond, "binding did not project the latest snapshot")
}

func TestDefaultAssemblyTodoWriteUpdatesInteractiveTUIAndNextTurn(t *testing.T) {
	ctx := context.Background()
	assembly, err := AssembleAgent(ctx, newAssembleTestConfig(t), "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	tool, err := assembly.ToolRegistry.Get(ctx, "todo_write")
	require.NoError(t, err)

	repl := &REPLSession{
		assembly: assembly,
		spanCtx:  ctx,
		logger:   slog.Default(),
	}
	startTurn := func() {
		repl.turnCtx, repl.cancelTurn = context.WithCancel(ctx)
		repl.stream = core.NewEventStream(8)
		require.NoError(t, repl.renderTUI(nil))
		repl.bindTodoUpdates()
	}
	stopTurn := func() {
		repl.stopTUI()
		if assembly.HITLEmitter != nil {
			assembly.HITLEmitter.SetProgram(nil)
		}
	}
	waitForContains := func(contains string) {
		t.Helper()
		require.Eventually(t, func() bool {
			return strings.Contains(repl.app.View(), contains)
		}, 2*time.Second, 5*time.Millisecond, "TUI view did not contain %q", contains)
	}

	startTurn()
	_, err = tool.Execute(ctx, tools.ToolCall{Name: "todo_write", Args: map[string]any{
		"action": "add", "content": "write a clear plan",
	}})
	require.NoError(t, err)
	waitForContains("write a clear plan")
	waitForContains("Todos [0/1 completed]")
	stopTurn()

	startTurn()
	waitForContains("write a clear plan")
	_, err = tool.Execute(ctx, tools.ToolCall{Name: "todo_write", Args: map[string]any{
		"action": "update", "id": "todo-1", "status": "completed",
	}})
	require.NoError(t, err)
	waitForContains("Todos [1/1 completed]")
	waitForContains("☑")
	stopTurn()
	select {
	case <-repl.app.Done():
	default:
		t.Fatal("TUI app did not restore its run after shutdown")
	}
}
