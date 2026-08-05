package tools

import (
	"context"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// --- ResourceLimits struct and option tests ---

func TestWithResourceLimits(t *testing.T) {
	limits := ResourceLimits{MaxCPU: 5 * time.Second, MaxMemory: 100 * 1024 * 1024}
	tool := NewBashTool(WithResourceLimits(limits))
	assert.Equal(t, 5*time.Second, tool.ResourceLimits.MaxCPU)
	assert.Equal(t, int64(100*1024*1024), tool.ResourceLimits.MaxMemory)
}

func TestApplyResourceLimits_DoesNotPanic(t *testing.T) {
	cmd := exec.Command("echo", "hi")
	limits := ResourceLimits{MaxMemory: 50 * 1024 * 1024, MaxCPU: 5 * time.Second}
	assert.NotPanics(t, func() {
		ApplyResourceLimits(cmd, limits)
	})
}

func TestApplyResourceLimits_ZeroLimitsNoOp(t *testing.T) {
	cmd := exec.Command("echo", "hi")
	limits := ResourceLimits{}
	assert.NotPanics(t, func() {
		ApplyResourceLimits(cmd, limits)
	})
}

// --- BashTool with resource limits integration tests ---

func TestBashTool_ResourceLimitsSimpleCommand(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	limits := ResourceLimits{MaxMemory: 256 * 1024 * 1024} // 256MB
	tool := NewBashTool(WithResourceLimits(limits), WithTimeout(10*time.Second))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "echo limited"},
	})
	require.NoError(t, err)
	assert.Equal(t, "limited\n", res.Output)
}

func TestBashTool_ResourceLimitsMemoryOOM(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("RLIMIT_AS memory enforcement is Linux-specific; Darwin uses advisory RLIMIT_DATA")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	// 10MB memory limit — allocating 100MB should fail.
	limits := ResourceLimits{MaxMemory: 10 * 1024 * 1024}
	tool := NewBashTool(WithResourceLimits(limits), WithTimeout(10*time.Second))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "perl -e 'my $x = \"a\" x (100*1024*1024)'"},
	})
	require.Error(t, err)
}

// --- Timeout tier classification tests ---

func TestClassifyCommand_FastReadOnly(t *testing.T) {
	fastCmds := []string{
		"ls -la",
		"cat file.txt",
		"head -n 10 file",
		"tail -n 10 file",
		"grep pattern file",
		"find . -name '*.go'",
		"wc -l file",
		"echo hello",
	}
	for _, cmd := range fastCmds {
		assert.Equal(t, timeoutTierFast, classifyCommand(cmd), "command %q should be fast", cmd)
	}
}

func TestClassifyCommand_SlowBuild(t *testing.T) {
	slowCmds := []string{
		"make build",
		"go test ./...",
		"npm install",
		"cargo build",
		"docker build .",
	}
	for _, cmd := range slowCmds {
		assert.Equal(t, timeoutTierSlow, classifyCommand(cmd), "command %q should be slow", cmd)
	}
}

func TestClassifyCommand_Normal(t *testing.T) {
	normalCmds := []string{
		"python3 script.py",
		"sleep 1",
		"git status",
		"curl https://example.com",
	}
	for _, cmd := range normalCmds {
		assert.Equal(t, timeoutTierNormal, classifyCommand(cmd), "command %q should be normal", cmd)
	}
}

func TestClassifyCommand_PathPrefix(t *testing.T) {
	assert.Equal(t, timeoutTierFast, classifyCommand("/usr/bin/ls"))
	assert.Equal(t, timeoutTierSlow, classifyCommand("/usr/local/go/bin/go build"))
	assert.Equal(t, timeoutTierFast, classifyCommand("./ls"))
}

func TestClassifyCommand_Empty(t *testing.T) {
	assert.Equal(t, timeoutTierNormal, classifyCommand(""))
	assert.Equal(t, timeoutTierNormal, classifyCommand("   "))
}

// --- TimeoutTier option tests ---

func TestWithTimeoutTier(t *testing.T) {
	tool := NewBashTool(WithTimeoutTier(true))
	assert.True(t, tool.TimeoutTier)
}

func TestBashTool_TimeoutTierFastCommandSucceeds(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewBashTool(WithTimeoutTier(true))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "echo fast"},
	})
	require.NoError(t, err)
	assert.Equal(t, "fast\n", res.Output)
}
