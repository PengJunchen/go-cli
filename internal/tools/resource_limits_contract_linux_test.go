//go:build linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestResourceLinuxMemoryLimitAndRounding(t *testing.T) {
	const requestedBytes = int64(512<<20 + 777)
	var inherited syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_AS, &inherited))
	want := uint64(requestedBytes / 1024)
	if inherited.Cur/1024 < want {
		want = inherited.Cur / 1024
	}
	if inherited.Max/1024 < want {
		want = inherited.Max / 1024
	}
	for _, mode := range resourceLimitModes {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			call := ToolCall{ID: "resource-memory", Name: "bash", Args: map[string]any{"command": "ulimit -S -v; ulimit -H -v; bash -c 'ulimit -H -v'"}}
			result, _, err := runResourceLimitMode(ctx, t, mode, call, WithNoSandbox(), WithResourceLimits(ResourceLimits{MaxMemory: requestedBytes}))
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, fmt.Sprintf("%d\n%d\n%d\n", want, want, want), result.Output)
		})
	}
}

func TestResourceLinuxSubKiBRejected(t *testing.T) {
	for _, value := range []int64{1, 1023} {
		t.Run(fmt.Sprint(value), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "unexpected")
			call := ToolCall{ID: "resource-memory-small", Name: "bash", Args: map[string]any{"command": "printf ran > \"$LIMIT_MARKER\""}}
			_, invocation, err := runResourceLimitMode(context.Background(), t, "prepared", call, WithNoSandbox(), WithResourceLimits(ResourceLimits{MaxMemory: value}), WithEnv(map[string]string{"LIMIT_MARKER": marker}))
			require.Error(t, err)
			require.NoFileExists(t, marker)
			require.Equal(t, CompletionInlinePipelineJoined, invocation.CompletionSignal().Proof())
		})
	}
}

func TestResourceLinuxMemoryDefaultSandboxMerge(t *testing.T) {
	const strict = int64(512 << 20)
	var inherited syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_AS, &inherited))
	want := uint64(strict / 1024)
	if inherited.Cur/1024 < want {
		want = inherited.Cur / 1024
	}
	if inherited.Max/1024 < want {
		want = inherited.Max / 1024
	}
	for _, tc := range []struct {
		name    string
		sandbox int64
		direct  int64
	}{
		{"sandbox-only", strict, 0},
		{"sandbox-stricter", strict, 768 << 20},
		{"direct-stricter", 768 << 20, strict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewBashTool(WithBashSandbox(NewDefaultBashSandbox(WithMaxMemory(tc.sandbox))), WithResourceLimits(ResourceLimits{MaxMemory: tc.direct}))
			result, err := tool.Execute(context.Background(), ToolCall{ID: "resource-memory-merge", Name: "bash", Args: map[string]any{"command": "ulimit -H -v"}})
			require.NoError(t, err)
			require.Equal(t, fmt.Sprint(want), strings.TrimSpace(result.Output))
		})
	}
}

func TestResourceLinuxMemoryAllocationControl(t *testing.T) {
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skipf("Perl allocation control unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	call := ToolCall{ID: "resource-memory-allocation", Name: "bash", Args: map[string]any{"command": `perl -e 'my $x = "a" x (128 * 1024 * 1024); print "ALLOCATION_OK:" . length($x) . "\n"'`}}
	unlimited := NewBashTool(WithNoSandbox())
	baseline, err := unlimited.Execute(ctx, call)
	require.NoError(t, err, "the unrestricted allocation is the positive control")
	require.Equal(t, "ALLOCATION_OK:134217728\n", baseline.Output)

	limited := NewBashTool(WithNoSandbox(), WithResourceLimits(ResourceLimits{MaxMemory: 64 << 20}))
	result, err := limited.Execute(ctx, call)
	require.Error(t, err)
	require.NotNil(t, result)
	require.NotContains(t, result.Output, "ALLOCATION_OK:")
	require.Regexp(t, `(?i)(out of memory|cannot allocate memory)`, result.Output, "failure must be the allocator's memory rejection")
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.NotEqual(t, 0, exitErr.ExitCode())
	t.Logf("allocation control: unrestricted=%q limited_output=%q limited_exit=%d", baseline.Output, result.Output, exitErr.ExitCode())
}

// denyResourceLimitSyscalls restricts only this locked OS thread and its
// children. The test helper process owns that thread until it exits.
func denyResourceLimitSyscalls() error {
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: unix.SYS_PRLIMIT64},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: unix.SYS_SETRLIMIT},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	return unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0)
}

func TestResourceLinuxKernelSetupFailureIsClosed(t *testing.T) {
	if os.Getenv("GOCLI_RESOURCE_TERRA_SECCOMP_HELPER") == "1" {
		runtime.LockOSThread()
		var current unix.Rlimit
		require.NoError(t, unix.Getrlimit(unix.RLIMIT_CPU, &current))
		if err := denyResourceLimitSyscalls(); err != nil {
			t.Skipf("kernel seccomp filter unavailable: %v", err)
		}
		require.ErrorIs(t, unix.Setrlimit(unix.RLIMIT_CPU, &current), unix.EPERM, "prove the filter rejects an otherwise unchanged limit")
		control := exec.Command("bash", "-c", "printf control")
		output, err := control.Output()
		require.NoError(t, err)
		require.Equal(t, "control", string(output))
		dir := t.TempDir()
		startup := filepath.Join(dir, "startup.sh")
		startupMarker := filepath.Join(dir, "startup-ran")
		commandMarker := filepath.Join(dir, "command-ran")
		require.NoError(t, os.WriteFile(startup, []byte("printf ran > \"$STARTUP_MARKER\"\n"), 0o600))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		call := ToolCall{ID: "resource-seccomp", Name: "bash", Args: map[string]any{"command": "printf ran > \"$COMMAND_MARKER\""}}
		_, invocation, err := runResourceLimitMode(ctx, t, os.Getenv("GOCLI_RESOURCE_TERRA_MODE"), call,
			WithNoSandbox(), WithResourceLimits(ResourceLimits{MaxCPU: 2 * time.Second}),
			WithEnv(map[string]string{"BASH_ENV": startup, "STARTUP_MARKER": startupMarker, "COMMAND_MARKER": commandMarker, "LARGE_RESOURCE_ENV": strings.Repeat("x", 96<<10)}))
		require.Error(t, err)
		require.False(t, errors.Is(err, context.DeadlineExceeded), "gate and child must finish within the deadline")
		require.NoFileExists(t, startupMarker)
		require.NoFileExists(t, commandMarker)
		if invocation != nil {
			require.False(t, PreparedBashOwnTimeoutKnown(invocation, err))
			require.NotEqual(t, CompletionInlinePipelineJoined, invocation.CompletionSignal().Proof(), "a started child cannot produce a no-process proof")
		}
		return
	}
	for _, mode := range resourceLimitModes {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResourceLinuxKernelSetupFailureIsClosed$", "-test.v")
			cmd.Env = append(os.Environ(), "GOCLI_RESOURCE_TERRA_SECCOMP_HELPER=1", "GOCLI_RESOURCE_TERRA_MODE="+mode)
			output, err := cmd.CombinedOutput()
			if strings.Contains(string(output), "--- SKIP: TestResourceLinuxKernelSetupFailureIsClosed") {
				t.Skipf("seccomp control unavailable: %s", output)
			}
			require.NoError(t, ctx.Err(), "bounded helper did not join: %s", output)
			require.NoError(t, err, "kernel setup control failed: %s", output)
		})
	}
}
