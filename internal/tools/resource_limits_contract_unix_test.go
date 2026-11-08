//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var resourceLimitModes = []string{"ordinary", "stream-execute", "stream-sink", "prepared", "prepared-stream"}

func runResourceLimitMode(ctx context.Context, t *testing.T, mode string, call ToolCall, opts ...BashToolOption) (*ToolResult, PreparedInvocation, error) {
	t.Helper()
	if mode == "ordinary" {
		result, err := NewBashTool(opts...).Execute(ctx, call)
		return result, nil, err
	}
	if mode == "stream-execute" {
		result, err := NewStreamingBashTool(opts...).Execute(ctx, call)
		return result, nil, err
	}
	if mode == "stream-sink" {
		result, err := NewStreamingBashTool(opts...).ExecuteStreaming(ctx, call, &mockStreamSink{})
		return result, nil, err
	}

	var definition ToolDefinition = NewBashTool(opts...)
	if mode == "prepared-stream" {
		definition = NewStreamingBashTool(opts...)
	}
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	require.NoError(t, base.registerTrustedLocalProcess(ctx, definition))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	if mode == "prepared-stream" {
		sink, err := AttestSynchronousStreamSink(&mockStreamSink{})
		require.NoError(t, err)
		invocation, err := registry.PrepareStreamingToolCall(ctx, call, sink)
		require.NoError(t, err)
		result, err := invocation.DispatchStreaming(ctx, sink)
		return result, invocation, err
	}
	invocation, err := registry.PrepareToolCall(ctx, call)
	require.NoError(t, err)
	result, err := invocation.Dispatch(ctx)
	return result, invocation, err
}

func inheritedCPULimit(t *testing.T, requested uint64) uint64 {
	t.Helper()
	var limit syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_CPU, &limit))
	if limit.Cur < requested {
		requested = limit.Cur
	}
	if limit.Max < requested {
		requested = limit.Max
	}
	return requested
}

func TestResourceCPUAcrossExecutionPaths(t *testing.T) {
	want := inheritedCPULimit(t, 2)
	for _, mode := range resourceLimitModes {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			call := ToolCall{ID: "resource-cpu", Name: "bash", Args: map[string]any{"command": "ulimit -S -t; ulimit -H -t; bash -c 'ulimit -S -t; ulimit -H -t'"}}
			result, invocation, err := runResourceLimitMode(ctx, t, mode, call, WithNoSandbox(), WithResourceLimits(ResourceLimits{MaxCPU: 1200 * time.Millisecond}))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, fmt.Sprintf("%d\n%d\n%d\n%d\n", want, want, want, want), result.Output, "the forked and execed child must inherit both CPU limits")
			if invocation != nil {
				require.Equal(t, CompletionOwnedProcessGroupJoined, invocation.CompletionSignal().Proof())
			}
		})
	}
}

func TestResourceCPUIsAppliedBeforeBashEnv(t *testing.T) {
	want := inheritedCPULimit(t, 2)
	for _, mode := range resourceLimitModes {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			startup := filepath.Join(dir, "startup.sh")
			marker := filepath.Join(dir, "limit.txt")
			require.NoError(t, os.WriteFile(startup, []byte("ulimit -H -t > \"$LIMIT_MARKER\"\n"), 0o600))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			call := ToolCall{ID: "resource-env", Name: "bash", Args: map[string]any{"command": "true"}}
			_, _, err := runResourceLimitMode(ctx, t, mode, call, WithNoSandbox(), WithResourceLimits(ResourceLimits{MaxCPU: 2 * time.Second}), WithEnv(map[string]string{"BASH_ENV": startup, "LIMIT_MARKER": marker}))
			require.NoError(t, err)
			actual, err := os.ReadFile(marker)
			require.NoError(t, err)
			require.Equal(t, strconv.FormatUint(want, 10), strings.TrimSpace(string(actual)))
		})
	}
}

func TestResourceLimitsRejectInvalidBeforeUserCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits ResourceLimits
	}{
		{"negative-cpu", ResourceLimits{MaxCPU: -time.Second}},
		{"negative-memory", ResourceLimits{MaxMemory: -1}},
	} {
		for _, mode := range resourceLimitModes {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				marker := filepath.Join(t.TempDir(), "unexpected")
				call := ToolCall{ID: "resource-invalid", Name: "bash", Args: map[string]any{"command": "printf ran > \"$LIMIT_MARKER\""}}
				_, invocation, err := runResourceLimitMode(context.Background(), t, mode, call, WithNoSandbox(), WithResourceLimits(tc.limits), WithEnv(map[string]string{"LIMIT_MARKER": marker}))
				require.Error(t, err)
				require.NoFileExists(t, marker)
				if invocation != nil {
					require.False(t, PreparedBashOwnTimeoutKnown(invocation, err))
					require.Equal(t, CompletionInlinePipelineJoined, invocation.CompletionSignal().Proof())
				}
			})
		}
	}
}

func TestResourceCPURounding(t *testing.T) {
	for _, tc := range []struct {
		name     string
		duration time.Duration
		seconds  uint64
	}{
		{"one-nanosecond", time.Nanosecond, 1},
		{"one-second", time.Second, 1},
		{"fractional", 1200 * time.Millisecond, 2},
		{"maximum-duration", time.Duration(1<<63 - 1), 9223372037},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := inheritedCPULimit(t, tc.seconds)
			tool := NewBashTool(WithNoSandbox(), WithResourceLimits(ResourceLimits{MaxCPU: tc.duration}))
			result, err := tool.Execute(context.Background(), ToolCall{ID: "resource-rounding", Name: "bash", Args: map[string]any{"command": "ulimit -S -t; ulimit -H -t"}})
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("%d\n%d\n", want, want), result.Output)
		})
	}
}

func TestResourceLimitsMergeDefaultSandbox(t *testing.T) {
	want := inheritedCPULimit(t, 2)
	for _, tc := range []struct {
		name    string
		sandbox time.Duration
		direct  time.Duration
	}{
		{"sandbox-only", 2 * time.Second, 0},
		{"direct-stricter", 4 * time.Second, 2 * time.Second},
		{"sandbox-stricter", 2 * time.Second, 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewBashTool(WithBashSandbox(NewDefaultBashSandbox(WithMaxCPU(tc.sandbox))), WithResourceLimits(ResourceLimits{MaxCPU: tc.direct}))
			result, err := tool.Execute(context.Background(), ToolCall{ID: "resource-merge", Name: "bash", Args: map[string]any{"command": "ulimit -H -t"}})
			require.NoError(t, err)
			require.Equal(t, strconv.FormatUint(want, 10), strings.TrimSpace(result.Output))
		})
	}

	for _, tc := range []struct {
		name    string
		sandbox time.Duration
		direct  time.Duration
	}{
		{"negative-sandbox", -time.Second, time.Second},
		{"negative-direct", time.Second, -time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "unexpected")
			tool := NewBashTool(WithBashSandbox(NewDefaultBashSandbox(WithMaxCPU(tc.sandbox))), WithResourceLimits(ResourceLimits{MaxCPU: tc.direct}), WithEnv(map[string]string{"LIMIT_MARKER": marker}))
			_, err := tool.Execute(context.Background(), ToolCall{ID: "resource-negative-merge", Name: "bash", Args: map[string]any{"command": "printf ran > \"$LIMIT_MARKER\""}})
			require.Error(t, err)
			require.NoFileExists(t, marker)
		})
	}
}

func TestResourceNegativeRegistrationIsNotDiscarded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits ResourceLimits
	}{{"cpu", ResourceLimits{MaxCPU: -time.Second}}, {"memory", ResourceLimits{MaxMemory: -1}}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			registry := NewDefaultToolRegistry()
			require.NoError(t, RegisterDefaults(ctx, registry, WithRegisteredBuiltinWhitelist([]string{"bash"}), WithRegisteredBashSandbox(AllowAllSandbox{}), WithRegisteredResourceLimits(tc.limits)))
			tool, err := registry.Get(ctx, "bash")
			require.NoError(t, err)
			marker := filepath.Join(t.TempDir(), "unexpected")
			_, err = tool.Execute(ctx, ToolCall{ID: "resource-negative-register", Name: "bash", Args: map[string]any{"command": "printf ran > " + shellQuoteTestArgument(marker)}})
			require.Error(t, err)
			require.NoFileExists(t, marker)
		})
	}
}

func TestResourceCPUEnforcementLeavesParentUnchanged(t *testing.T) {
	var before, after syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_CPU, &before))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tool := NewBashTool(WithNoSandbox(), WithTimeout(12*time.Second), WithResourceLimits(ResourceLimits{MaxCPU: time.Second}))
	_, err := tool.Execute(ctx, ToolCall{ID: "resource-busy", Name: "bash", Args: map[string]any{"command": "while :; do :; done"}})
	require.Error(t, err)
	require.NoError(t, ctx.Err())
	require.False(t, errors.Is(err, context.DeadlineExceeded))
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	require.True(t, status.Signaled())
	require.Contains(t, []syscall.Signal{syscall.SIGKILL, syscall.SIGXCPU}, status.Signal())
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_CPU, &after))
	require.Equal(t, before, after)
}

func TestResourceCPUInheritedSoftLimitClamped(t *testing.T) {
	if os.Getenv("GOCLI_RESOURCE_TERRA_CPU_CLAMP") == "1" {
		tool := NewBashTool(WithNoSandbox(), WithResourceLimits(ResourceLimits{MaxCPU: 2 * time.Second}))
		result, err := tool.Execute(context.Background(), ToolCall{ID: "resource-cpu-clamp", Name: "bash", Args: map[string]any{"command": "ulimit -S -t; ulimit -H -t"}})
		require.NoError(t, err)
		require.Equal(t, "1\n1\n", result.Output)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", `ulimit -S -t 1 || exit 86; exec "$1" -test.run '^TestResourceCPUInheritedSoftLimitClamped$' -test.v`, "resource-clamp", os.Args[0])
	cmd.Env = append(os.Environ(), "GOCLI_RESOURCE_TERRA_CPU_CLAMP=1")
	output, err := cmd.CombinedOutput()
	require.NoError(t, ctx.Err(), "clamp helper timed out: %s", output)
	require.NoError(t, err, "clamp helper failed: %s", output)
}

func TestResourceCPUDoesNotImposeWallTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tool := NewBashTool(WithNoSandbox(), WithTimeout(6*time.Second), WithResourceLimits(ResourceLimits{MaxCPU: time.Second}))
	start := time.Now()
	result, err := tool.Execute(ctx, ToolCall{ID: "resource-sleep", Name: "bash", Args: map[string]any{"command": "sleep 2; printf 'sleep-complete\\n'"}})
	require.NoError(t, err)
	require.Equal(t, "sleep-complete\n", result.Output)
	require.GreaterOrEqual(t, time.Since(start), 2*time.Second)
}

type resourceReadySink struct {
	once  sync.Once
	ready chan struct{}
}

func (s *resourceReadySink) Send(content, _, _ string) error {
	if content == "RESOURCE_READY" {
		s.once.Do(func() { close(s.ready) })
	}
	return nil
}

func TestResourceCPULimitDoesNotAffectConcurrentCommand(t *testing.T) {
	var before, after syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_CPU, &before))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ordinary := NewBashTool(WithNoSandbox())
	query := ToolCall{ID: "resource-unlimited", Name: "bash", Args: map[string]any{"command": "ulimit -H -t"}}
	baseline, err := ordinary.Execute(ctx, query)
	require.NoError(t, err)

	fifo := filepath.Join(t.TempDir(), "release.fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	release, err := os.OpenFile(fifo, os.O_RDWR, 0)
	require.NoError(t, err)
	defer release.Close()
	sink := &resourceReadySink{ready: make(chan struct{})}
	type runResult struct {
		result *ToolResult
		err    error
	}
	done := make(chan runResult, 1)
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("limited command did not join during cleanup")
			}
		}
	}()
	go func() {
		limited := NewStreamingBashTool(WithNoSandbox(), WithResourceLimits(ResourceLimits{MaxCPU: 2 * time.Second}), WithEnv(map[string]string{"RESOURCE_RELEASE_FIFO": fifo}))
		result, runErr := limited.ExecuteStreaming(ctx, ToolCall{ID: "resource-limited", Name: "bash", Args: map[string]any{"command": "printf 'RESOURCE_READY\\n'; IFS= read -r _ < \"$RESOURCE_RELEASE_FIFO\"; ulimit -H -t"}}, sink)
		done <- runResult{result, runErr}
	}()
	select {
	case <-sink.ready:
	case result := <-done:
		joined = true
		t.Fatalf("limited command exited before its barrier: %v", result.err)
	case <-ctx.Done():
		t.Fatal("limited command did not reach its barrier")
	}
	concurrent, err := ordinary.Execute(ctx, query)
	require.NoError(t, err)
	require.Equal(t, baseline.Output, concurrent.Output, "another command must inherit the unchanged parent limit")
	_, err = release.WriteString("continue\n")
	require.NoError(t, err)
	select {
	case result := <-done:
		joined = true
		require.NoError(t, result.err)
		require.Equal(t, fmt.Sprintf("RESOURCE_READY\n%d\n", inheritedCPULimit(t, 2)), result.result.Output)
	case <-ctx.Done():
		t.Fatal("limited command did not finish after release")
	}
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_CPU, &after))
	require.Equal(t, before, after)
}
