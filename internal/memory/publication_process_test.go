package memory_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/memory"
)

func TestMemoryPublicationSubprocess(t *testing.T) {
	mode := os.Getenv("GO_CLI_MEMORY_TEST_PROCESS")
	if mode == "" {
		t.Skip("subprocess entry")
	}
	path := os.Getenv("GO_CLI_MEMORY_TEST_PATH")
	store, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if mode == "first" {
		waitMemoryFile(ctx, t, path+".second-open")
		require.NoError(t, store.Add(ctx, memory.Memory{ID: "new", Content: "new"}))
		require.NoError(t, os.WriteFile(path+".added", nil, 0o600))
		waitMemoryFile(ctx, t, path+".deleted")
		require.NoError(t, store.Add(ctx, memory.Memory{ID: "later", Content: "later"}))
	} else {
		require.NoError(t, os.WriteFile(path+".second-open", nil, 0o600))
		waitMemoryFile(ctx, t, path+".added")
		require.NoError(t, store.Delete(ctx, "old"))
		require.NoError(t, os.WriteFile(path+".deleted", nil, 0o600))
	}
}

func waitMemoryFile(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("subprocess coordination timed out: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestMemoryIndependentProcessesPreservePublicationAcrossReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.jsonl")
	store, err := memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	require.NoError(t, store.Add(t.Context(), memory.Memory{ID: "old", Content: "old"}))
	require.NoError(t, store.Close())
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var commands []*exec.Cmd
	var outputs []*bytes.Buffer
	for _, mode := range []string{"first", "second"} {
		command := exec.CommandContext(ctx, executable, "-test.run=^TestMemoryPublicationSubprocess$")
		command.Env = append(os.Environ(), "GO_CLI_MEMORY_TEST_PROCESS="+mode, "GO_CLI_MEMORY_TEST_PATH="+path)
		output := &bytes.Buffer{}
		command.Stdout = output
		command.Stderr = output
		require.NoError(t, command.Start())
		commands = append(commands, command)
		outputs = append(outputs, output)
	}
	for index, command := range commands {
		require.NoError(t, command.Wait(), outputs[index].String())
	}
	store, err = memory.NewFileMemoryStore(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, store.Close()) }()
	items, err := store.List(t.Context())
	require.NoError(t, err)
	require.Len(t, items, 2)
	_, err = store.Get(t.Context(), "new")
	require.NoError(t, err)
	_, err = store.Get(t.Context(), "later")
	require.NoError(t, err)
	_, err = store.Get(t.Context(), "old")
	require.ErrorIs(t, err, memory.ErrMemoryNotFound)
}
