//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestAutomaticMemoryStartupLockCancellationAndFailOpen(t *testing.T) {
	for _, cancelCommand := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancel", false: "busy"}[cancelCommand], func(t *testing.T) {
			server := newMemoryTestServer(t)
			defer server.Close()
			cfg, home := newMemoryTestConfig(t, server.URL)
			cfg.Memory.Enabled = nil
			cfg.Memory.StorePath = filepath.Join(home, "locked.jsonl")
			lock, err := os.OpenFile(cfg.Memory.StorePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
			require.NoError(t, err)
			defer lock.Close()
			require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB))
			defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			var output bytes.Buffer
			go func() { done <- newPromptCmd(&output).Run(ctx, cfg, []string{"main chat must remain usable"}) }()
			if cancelCommand {
				time.Sleep(100 * time.Millisecond)
				cancel()
			}
			select {
			case err := <-done:
				if cancelCommand {
					require.ErrorIs(t, err, context.Canceled)
					require.Empty(t, server.requestBodies())
				} else {
					require.NoError(t, err)
					require.Len(t, mainMemoryRequestBodies(server), 1)
					require.Zero(t, server.extractionRequests.Load())
					require.NotContains(t, output.String(), "Memory store unavailable; continuing without memory")
				}
			case <-time.After(2 * time.Second):
				require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_UN))
				cancel()
				<-done
				t.Fatal("optional memory lock blocked command")
			}
		})
	}
}

func TestAutomaticMemoryDiagnosticsKeepCommandOutputClean(t *testing.T) {
	for _, failure := range []string{"busy", "directory"} {
		for _, mode := range []string{"json", "stream", "text", "interactive"} {
			t.Run(failure+"/"+mode, func(t *testing.T) {
				server := newMemoryTestServer(t)
				defer server.Close()
				cfg, home := newMemoryTestConfig(t, server.URL)
				cfg.Memory.Enabled = nil
				cfg.Memory.StorePath = filepath.Join(home, "memory.jsonl")
				if failure == "busy" {
					lock, err := os.OpenFile(cfg.Memory.StorePath+".lock", os.O_CREATE|os.O_RDWR, 0600)
					require.NoError(t, err)
					defer lock.Close()
					require.NoError(t, unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB))
					defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
				} else {
					require.NoError(t, os.Mkdir(cfg.Memory.StorePath, 0700))
				}
				data, err := json.Marshal(cfg)
				require.NoError(t, err)
				path := filepath.Join(home, "config.json")
				require.NoError(t, os.WriteFile(path, data, 0600))
				executable, err := os.Executable()
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				command := exec.CommandContext(ctx, executable, "-test.run=^TestAutomaticMemoryDiagnosticProcess$")
				command.Env = append(os.Environ(), "GO_CLI_MEMORY_DIAGNOSTIC_CONFIG="+path, "GO_CLI_MEMORY_DIAGNOSTIC_MODE="+mode)
				var output, diagnostics bytes.Buffer
				command.Stdout = &output
				command.Stderr = &diagnostics
				require.NoError(t, command.Run(), diagnostics.String())
				require.True(t, bytes.Contains(diagnostics.Bytes(), []byte("Memory store unavailable; continuing without memory")), "diagnostic must remain visible on stderr")
				require.NotContains(t, output.String(), "Memory store unavailable")
				if mode == "json" {
					decoder := json.NewDecoder(&output)
					events := 0
					for {
						var event map[string]any
						err := decoder.Decode(&event)
						if err == io.EOF {
							break
						}
						require.NoError(t, err, "stdout must contain only complete JSON events")
						events++
					}
					require.Positive(t, events)
				} else if mode == "stream" {
					// Compare with the same public streaming command without memory;
					// its event rendering is outside this diagnostic contract.
					disabled := false
					cfg.Memory.Enabled = &disabled
					cfg.Memory.StorePath = ""
					data, err := json.Marshal(cfg)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(path, data, 0600))
					baseline := exec.CommandContext(ctx, executable, "-test.run=^TestAutomaticMemoryDiagnosticProcess$")
					baseline.Env = command.Env
					var baselineOutput, baselineDiagnostics bytes.Buffer
					baseline.Stdout, baseline.Stderr = &baselineOutput, &baselineDiagnostics
					require.NoError(t, baseline.Run(), baselineDiagnostics.String())
					require.Equal(t, baselineOutput.String(), output.String(), "memory diagnostics must preserve raw stream output")
				} else {
					require.Contains(t, output.String(), "ok")
				}
				require.Zero(t, server.extractionRequests.Load())
			})
		}
	}
}
