//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResourceGateParentReadClosedBeforeObserver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits bashResourceSettings
	}{{"unlimited", bashResourceSettings{}}, {"limited", bashResourceSettings{cpuSeconds: 2}}} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", "true")
			cmd.Env = filteredEnv(nil)
			base, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx := WithProcessExecutionIdentityObserver(base, func(_ context.Context, _ ProcessExecutionIdentity) error {
				if len(cmd.ExtraFiles) != 1 {
					return fmt.Errorf("expected the start-gate read descriptor")
				}
				if _, err := cmd.ExtraFiles[0].Stat(); !errors.Is(err, os.ErrClosed) {
					return fmt.Errorf("parent retains the gate read descriptor: %v", err)
				}
				return nil
			})
			managed, err := startManagedLocalBashCommand(ctx, cancel, cmd, tc.limits)
			require.NoError(t, err)
			lifecycle := managed.collect(ctx, func(reader io.Reader, _ string) (bool, error) {
				_, readErr := io.Copy(io.Discard, reader)
				return readErr == nil, readErr
			})
			require.True(t, lifecycle.quiescent)
			require.NoError(t, lifecycle.runErr)
		})
	}
}

func TestResourceGateCancelUnblocksLargeRelease(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	require.NoError(t, err)
	defer readEnd.Close()
	gate := &bashProcessStartGate{readEnd: readEnd, writeEnd: writeEnd, restoreEnv: []string{"LARGE=" + strings.Repeat("x", 4<<20)}}
	defer gate.abort()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- gate.releaseWithContext(ctx) }()
	// Receiving the release token proves the actual writer entered the pipe;
	// the remaining environment exceeds the bounded pipe capacity.
	token := make([]byte, len("go-cli-identity-release\x00"))
	_, err = io.ReadFull(readEnd, token)
	require.NoError(t, err)
	require.Equal(t, "go-cli-identity-release\x00", string(token))
	cancel()
	select {
	case err = <-done:
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, err, os.ErrClosed)
	case <-time.After(3 * time.Second):
		_ = readEnd.Close()
		<-done
		t.Fatal("cancel did not unblock the active gate write")
	}
}

func TestResourceGateCancelStoppedBootstrap(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	entered := make(chan struct{})
	ctx := WithProcessExecutionIdentityObserver(base, func(_ context.Context, identity ProcessExecutionIdentity) error {
		if err := syscall.Kill(identity.LeaderPID, syscall.SIGSTOP); err != nil {
			return err
		}
		var status syscall.WaitStatus
		if _, err := syscall.Wait4(identity.LeaderPID, &status, syscall.WUNTRACED, nil); err != nil {
			return err
		}
		// Darwin's syscall.WaitStatus.Stopped classifies SIGSTOP as continued;
		// inspect the POSIX wait status returned by this explicit WUNTRACED wait.
		if uint32(status)&0xff != 0x7f || uint32(status)>>8&0xff != uint32(syscall.SIGSTOP) {
			return fmt.Errorf("owned bootstrap did not stop: %v", status)
		}
		close(stopped)
		return nil
	})
	ctx = WithBashRuntimeInput(ctx, "", func(_ context.Context, release func() error) error {
		close(entered)
		return release()
	})
	done := make(chan error, 1)
	go func() {
		tool := NewBashTool(WithNoSandbox(), WithResourceLimits(ResourceLimits{MaxCPU: 2 * time.Second}), WithEnv(map[string]string{"LARGE_RESOURCE_ENV": strings.Repeat("x", 4<<20)}))
		_, err := tool.Execute(ctx, ToolCall{ID: "resource-stopped-gate", Name: "bash", Args: map[string]any{"command": "true"}})
		done <- err
	}()
	select {
	case <-stopped:
	case err := <-done:
		t.Fatalf("owned bootstrap returned before stop barrier: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("owned bootstrap did not reach the stop barrier")
	}
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("owned bootstrap returned before release guard: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("owned bootstrap did not reach its release guard")
	}
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled stopped bootstrap did not join")
	}
}
