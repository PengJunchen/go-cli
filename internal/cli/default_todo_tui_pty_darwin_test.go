//go:build darwin

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	xterm "golang.org/x/term"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

const (
	todoPTYChildEnv    = "GO_CLI_TODO_PTY_CHILD"
	todoPTYEvidenceEnv = "GO_CLI_TODO_PTY_EVIDENCE"
)

func TestDefaultTodoWriteCompletesInDarwinPTYAndRestoresTerminal(t *testing.T) {
	if os.Getenv(todoPTYChildEnv) == "1" {
		runDefaultTodoPTYChild(t)
		return
	}

	slave, master, cleanup := openPTYWithMaster(t)
	t.Cleanup(cleanup)
	require.NoError(t, unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: 80}))
	before, err := xterm.GetState(int(slave.Fd()))
	require.NoError(t, err)
	evidencePath := filepath.Join(t.TempDir(), "todo-pty-evidence.txt")

	command := exec.Command(os.Args[0], "-test.run=^TestDefaultTodoWriteCompletesInDarwinPTYAndRestoresTerminal$")
	command.Env = append(os.Environ(), todoPTYChildEnv+"=1", todoPTYEvidenceEnv+"="+evidencePath)
	command.Stdin = slave
	command.Stdout = slave
	command.Stderr = slave
	require.NoError(t, unix.SetNonblock(int(master.Fd()), true))
	var output bytes.Buffer
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, readErr := unix.Read(int(master.Fd()), buf)
			if n > 0 {
				_, _ = output.Write(buf[:n])
			}
			if errors.Is(readErr, unix.EAGAIN) || errors.Is(readErr, unix.EWOULDBLOCK) {
				time.Sleep(time.Millisecond)
				continue
			}
			if errors.Is(readErr, unix.EIO) || readErr != nil || n == 0 {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = slave.Close()
		_ = master.Close()
		<-readDone
	})
	require.NoError(t, command.Start())

	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	select {
	case waitErr := <-waitDone:
		require.NoError(t, waitErr)
	case <-time.After(20 * time.Second):
		_ = command.Process.Kill()
		<-waitDone
		t.Fatal("timed out waiting for native PTY todo journey")
	}

	after, err := xterm.GetState(int(slave.Fd()))
	require.NoError(t, err)
	assertTerminalStateRestored(t, before, after)
	_ = slave.Close()
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out draining native PTY output")
	}
	_ = master.Close()

	evidence, err := os.ReadFile(evidencePath)
	require.NoError(t, err)
	assertions := string(evidence)
	require.Contains(t, assertions, "Todos [0/1 completed]")
	require.Contains(t, assertions, "write a clear plan")
	require.Contains(t, assertions, "Todos [1/1 completed]")
	require.Contains(t, assertions, "☑")
	require.Contains(t, output.String(), "R5_TODO_PTY_VERIFIED")
}

func runDefaultTodoPTYChild(t *testing.T) {
	evidencePath := os.Getenv(todoPTYEvidenceEnv)
	require.NotEmpty(t, evidencePath)
	before, err := xterm.GetState(int(os.Stdin.Fd()))
	require.NoError(t, err)

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
	repl.bindTodoUpdates()
	t.Cleanup(repl.stopTUI)
	select {
	case <-repl.app.ProgramReady():
		require.NotNil(t, repl.app.Program())
	case <-time.After(2 * time.Second):
		t.Fatal("TUI program did not start in PTY")
	}

	var raw *xterm.State
	require.Eventually(t, func() bool {
		var stateErr error
		raw, stateErr = xterm.GetState(int(os.Stdin.Fd()))
		return stateErr == nil && !reflect.DeepEqual(raw, before)
	}, 2*time.Second, 5*time.Millisecond, "TUI did not change the terminal state")

	_, err = tool.Execute(ctx, tools.ToolCall{Name: "todo_write", Args: map[string]any{
		"action": "add", "content": "write a clear plan",
	}})
	require.NoError(t, err)
	var pendingView string
	require.Eventually(t, func() bool {
		pendingView = repl.app.View()
		return strings.Contains(pendingView, "Todos [0/1 completed]") && strings.Contains(pendingView, "write a clear plan")
	}, 2*time.Second, 5*time.Millisecond, "pending todo did not reach the TUI")

	_, err = tool.Execute(ctx, tools.ToolCall{Name: "todo_write", Args: map[string]any{
		"action": "update", "id": "todo-1", "status": "completed",
	}})
	require.NoError(t, err)
	var completedView string
	require.Eventually(t, func() bool {
		completedView = repl.app.View()
		return strings.Contains(completedView, "Todos [1/1 completed]") && strings.Contains(completedView, "☑")
	}, 2*time.Second, 5*time.Millisecond, "completed todo did not reach the TUI")

	repl.stopTUI()
	if assembly.HITLEmitter != nil {
		assembly.HITLEmitter.SetProgram(nil)
	}
	after, err := xterm.GetState(int(os.Stdin.Fd()))
	require.NoError(t, err)
	assertTerminalStateRestored(t, before, after)
	evidence := fmt.Sprintf("pending:\n%s\ncompleted:\n%s\nraw_state_changed=%t terminal_restored=%t\n", pendingView, completedView, !reflect.DeepEqual(before, raw), reflect.DeepEqual(before, after))
	require.NoError(t, os.WriteFile(evidencePath, []byte(evidence), 0o600))
	_, err = fmt.Fprintln(os.Stdout, "R5_TODO_PTY_VERIFIED")
	require.NoError(t, err)
}

func assertTerminalStateRestored(t *testing.T, before, after *xterm.State) {
	t.Helper()
	require.True(t, reflect.DeepEqual(before, after), "the complete termios state should be restored")
}
