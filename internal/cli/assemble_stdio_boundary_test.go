package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
)

const (
	acpStdioHelperModeEnv = "GO_CLI_TEST_ACP_STDIO_HELPER"
	acpStdioHelperFileEnv = "GO_CLI_TEST_ACP_STDIO_HELPER_FILE"
)

func TestACPStdioConfiguredCommandHelper(t *testing.T) {
	if os.Getenv(acpStdioHelperModeEnv) != "1" {
		return
	}
	input := bufio.NewReader(os.Stdin)
	frame, err := input.ReadString('\n')
	if err != nil {
		os.Exit(31)
	}
	if err := os.WriteFile(os.Getenv(acpStdioHelperFileEnv), []byte(frame), 0o600); err != nil {
		os.Exit(32)
	}
	_, _ = io.Copy(io.Discard, input)
	os.Exit(0)
}

func TestAssembleACPStdioUsesConfiguredChildAndKeepsProcessStdinOpen(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("owned ACP stdio process groups are supported on Linux and Darwin")
	}
	globalStdinWriter, globalStdoutPath := replaceProcessStdioForACPTest(t)
	helperPath := filepath.Join(t.TempDir(), "connect.jsonl")
	t.Setenv(acpStdioHelperModeEnv, "1")
	t.Setenv(acpStdioHelperFileEnv, helperPath)
	executable, err := os.Executable()
	require.NoError(t, err)

	cfg := newAssembleTestConfig(t)
	cfg.ACP = config.ACPConfig{
		Transport: "stdio",
		Command:   executable,
		Args:      []string{"-test.run=^TestACPStdioConfiguredCommandHelper$"},
	}
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "stdio-test", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(helperPath)
		return statErr == nil
	}, time.Second, 10*time.Millisecond, "the configured child must consume the private connect handshake")

	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, assembly.Close(closeCtx))

	frame, err := os.ReadFile(helperPath)
	require.NoError(t, err, "the child must receive ACP's private connect handshake")
	var connect map[string]any
	require.NoError(t, json.Unmarshal(frame, &connect))
	require.Equal(t, "connect", connect["type"])
	require.Equal(t, "go-cli-acp-json/1", connect["version"])
	globalOutput, err := os.ReadFile(globalStdoutPath)
	require.NoError(t, err)
	require.Empty(t, globalOutput, "the handshake must stay on the owned child pipes")

	_, err = globalStdinWriter.Write([]byte("still-open\n"))
	require.NoError(t, err, "Assembly.Close must not close process-global stdin")
}

func TestAssembleACPStdioRequiresCommandBeforeTouchingGlobalStdio(t *testing.T) {
	globalStdinWriter, globalStdoutPath := replaceProcessStdioForACPTest(t)
	cfg := newAssembleTestConfig(t)
	cfg.ACP = config.ACPConfig{
		Transport: "stdio",
		Endpoints: []string{"stdio://legacy"},
	}

	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "stdio-test", io.Discard)
	if err == nil && assembly != nil {
		_ = globalStdinWriter.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = assembly.Close(cleanupCtx)
		cancel()
	}
	require.Error(t, err, "endpoint-only stdio must fail closed without starting a process")
	require.Nil(t, assembly)
	require.ErrorContains(t, err, "command")
	globalOutput, readErr := os.ReadFile(globalStdoutPath)
	require.NoError(t, readErr)
	require.Empty(t, globalOutput, "missing command must not emit an ACP frame to global stdout")
	_, writeErr := globalStdinWriter.Write([]byte("still-open\n"))
	require.NoError(t, writeErr, "missing command must not close process-global stdin")
}

func replaceProcessStdioForACPTest(t *testing.T) (*os.File, string) {
	t.Helper()
	previousStdin, previousStdout := os.Stdin, os.Stdout
	stdinReader, stdinWriter, err := os.Pipe()
	require.NoError(t, err)
	stdoutFile, err := os.CreateTemp(t.TempDir(), "global-stdout-*.log")
	require.NoError(t, err)
	stdoutPath := stdoutFile.Name()
	os.Stdin = stdinReader
	os.Stdout = stdoutFile
	t.Cleanup(func() {
		os.Stdin = previousStdin
		os.Stdout = previousStdout
		_ = stdinWriter.Close()
		_ = stdinReader.Close()
		_ = stdoutFile.Close()
	})
	return stdinWriter, stdoutPath
}
