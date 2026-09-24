package gatecapture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const helperModeEnv = "VERIFY_GATE_CAPTURE_HELPER_MODE"

func TestGateCaptureProcessFixture(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "stdout", "stderr":
		writer := os.Stdout
		if mode == "stderr" {
			writer = os.Stderr
		}
		_, _ = fmt.Fprint(writer, strings.Repeat("x", 128))
	case "sleep":
		time.Sleep(10 * time.Second)
	case "spawn-holder":
		pidFile := os.Getenv("VERIFY_GATE_CAPTURE_PID_FILE")
		child := exec.Command(os.Args[0], "-test.run=^TestGateCaptureProcessFixture$")
		child.Env = envWith(helperModeEnv, "hold")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "start pipe holder: %v\n", err)
			os.Exit(21)
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "write pipe holder pid: %v\n", err)
			os.Exit(22)
		}
	case "hold":
		time.Sleep(10 * time.Second)
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown fixture mode %q\n", mode)
		os.Exit(23)
	}
}

func TestRunCapturesOutputWithinHardLimit(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			result, err := runFixture(t, stream, Limits{StdoutBytes: 32, StderrBytes: 32, WaitDelay: 100 * time.Millisecond}, context.Background())
			if err == nil {
				t.Fatal("Run() error = nil after fixture exceeded the capture limit")
			}
			if len(result.Stdout) > 32 || len(result.Stderr) > 32 {
				t.Fatalf("captured bytes exceed configured bounds: stdout=%d stderr=%d", len(result.Stdout), len(result.Stderr))
			}
			if stream == "stdout" && !result.StdoutExceeded {
				t.Fatal("stdout overflow was not reported")
			}
			if stream == "stderr" && !result.StderrExceeded {
				t.Fatal("stderr overflow was not reported")
			}
		})
	}
}

func TestRunReturnsWithinWaitDelayWhenDescendantHoldsPipes(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := runFixtureWithEnv(t, "spawn-holder", "VERIFY_GATE_CAPTURE_PID_FILE="+pidFile, Limits{StdoutBytes: 1024, StderrBytes: 1024, WaitDelay: 150 * time.Millisecond}, ctx)
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("fixture context error = %v, want deadline exceeded (result=%+v err=%v)", ctx.Err(), result, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Run() took %s with a 100ms timeout and 150ms WaitDelay", elapsed)
	}
	killProcessFromFile(t, pidFile)
}

func TestRunReturnsAfterContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runFixture(t, "sleep", Limits{StdoutBytes: 1024, StderrBytes: 1024, WaitDelay: 150 * time.Millisecond}, ctx)
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("fixture context error = %v, want deadline exceeded (err=%v)", ctx.Err(), err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Run() took %s with a 100ms timeout and 150ms WaitDelay", elapsed)
	}
}

func runFixture(t *testing.T, mode string, limits Limits, ctx context.Context) (Result, error) {
	t.Helper()
	return runFixtureWithEnv(t, mode, "", limits, ctx)
}

func runFixtureWithEnv(t *testing.T, mode, extraEnv string, limits Limits, ctx context.Context) (Result, error) {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGateCaptureProcessFixture$")
	cmd.Env = append(os.Environ(), helperModeEnv+"="+mode)
	if extraEnv != "" {
		cmd.Env = append(cmd.Env, extraEnv)
	}
	return Run(cmd, limits)
}

func killProcessFromFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pipe holder pid: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatalf("parse pipe holder pid %q: %v", data, err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find pipe holder process %d: %v", pid, err)
	}
	if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill pipe holder process %d: %v", pid, err)
	}
}

func envWith(name, value string) []string {
	prefix := name + "="
	env := os.Environ()
	filtered := env[:0]
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			filtered = append(filtered, item)
		}
	}
	return append(filtered, prefix+value)
}
