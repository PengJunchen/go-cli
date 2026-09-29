//go:build darwin || linux

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIBinaryTrustGrantPTYPersistsManifestForLaterInvocation(t *testing.T) {
	binaryPath := buildCLIBinary(t)
	workDir := t.TempDir()
	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workDir, ".go-cli.yaml"), []byte("model:\n  name: project\n"), 0o600))
	root, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)

	master, slave, cleanup := openTrustPTY(t)
	t.Cleanup(cleanup)
	command := exec.Command(binaryPath, "trust", "grant")
	command.Dir = workDir
	command.Env = environmentWithHome(home)
	command.Stdin = slave
	command.Stdout = slave
	command.Stderr = slave
	require.NoError(t, command.Start())
	require.NoError(t, slave.Close())

	type readResult struct {
		output string
		err    error
	}
	promptSeen := make(chan struct{})
	readDone := make(chan readResult, 1)
	go func() {
		var output bytes.Buffer
		buf := make([]byte, 4096)
		promptSignaled := false
		for {
			n, readErr := master.Read(buf)
			if n > 0 {
				_, _ = output.Write(buf[:n])
				if !promptSignaled && strings.Contains(output.String(), "Type TRUST to confirm grant:") {
					promptSignaled = true
					close(promptSeen)
				}
			}
			if readErr != nil {
				readDone <- readResult{output: output.String(), err: readErr}
				return
			}
		}
	}()

	select {
	case <-promptSeen:
	case result := <-readDone:
		t.Fatalf("PTY closed before grant confirmation prompt: %v\n%s", result.err, result.output)
	case <-time.After(15 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("timed out waiting for grant confirmation prompt")
	}

	_, err = master.Write([]byte("TRUST\n"))
	require.NoError(t, err)
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	select {
	case waitErr := <-waitDone:
		require.NoError(t, waitErr)
	case <-time.After(15 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("timed out waiting for trust grant command")
	}

	var result readResult
	select {
	case result = <-readDone:
	case <-time.After(5 * time.Second):
		_ = master.Close()
		t.Fatal("timed out draining PTY output")
	}
	if result.err != nil && !errors.Is(result.err, io.EOF) && !errors.Is(result.err, os.ErrClosed) && !errors.Is(result.err, syscall.EIO) {
		// PTY masters commonly return EIO after the last slave closes.
		assert.NoError(t, result.err)
	}
	assert.Contains(t, result.output, root)
	assert.Contains(t, result.output, "Manifest SHA-256:")
	assert.Contains(t, result.output, "Sources:")
	assert.Contains(t, result.output, "Type TRUST to confirm grant:")
	assert.Contains(t, result.output, "active only after a later invocation")

	storeDir := filepath.Join(home, ".config", "go-cli")
	storePath := filepath.Join(storeDir, "trust.json")
	storeInfo, err := os.Stat(storePath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), storeInfo.Mode().Perm())
	dirInfo, err := os.Stat(storeDir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	code, output := runCLIBinary(t, binaryPath, workDir, home, "trust", "list")
	require.Zero(t, code, "output=%q", output)
	assert.Contains(t, output, root)
	assert.Contains(t, output, "manifest v1")

	code, output = runCLIBinary(t, binaryPath, workDir, home, "trust", "revoke", root)
	require.Zero(t, code, "output=%q", output)
	code, output = runCLIBinary(t, binaryPath, workDir, home, "trust", "list")
	require.Zero(t, code, "output=%q", output)
	assert.Contains(t, output, "No trust entries.")
}
