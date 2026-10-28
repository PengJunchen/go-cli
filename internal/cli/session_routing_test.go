package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

func TestSessionRegistryKeepsValidationOrderAndReadOnlyCatalog(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "absent")
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"unknown"}, "--store"},
		{[]string{"unknown", "--store", store, "extra"}, "trailing"},
		{[]string{"unknown", "--store", store}, "unknown subcommand"},
		{[]string{"list", "--store", store, "extra"}, "trailing"},
	} {
		var out bytes.Buffer
		require.ErrorContains(t, newSessionCmd(&out).Run(t.Context(), nil, test.args), test.want)
	}
	var out bytes.Buffer
	require.NoError(t, newSessionCmd(&out).Run(t.Context(), nil, []string{"list", "--store", store}))
	require.Contains(t, out.String(), "Physical sessions:")
	_, err := os.Stat(store)
	require.True(t, os.IsNotExist(err), "read-only list and invalid routes must not create a store")
}

func TestDurableSessionRegistryPreservesSafetyFallbackAndCatalogAliases(t *testing.T) {
	configureDurableCLISessionTest(t)
	store := filepath.Join(t.TempDir(), "history")
	durable, err := newDurableCLISession(t.Context(), store, "routing fixture", "", "")
	require.NoError(t, err)
	defer func() { require.NoError(t, durable.requestClose(t.Context())) }()
	initial, err := durable.snapshotSelected()
	require.NoError(t, err)
	file := filepath.Join(store, "sessions", initial.SessionID+".jsonl")
	before, err := os.ReadFile(file)
	require.NoError(t, err)
	var out bytes.Buffer
	repl := &REPLSession{out: &out, assembly: &AgentAssembly{durableSession: durable, runSlotGuard: core.NewDefaultRunSlotGuard()}}
	commands := &durableHistoryCommands{repl: repl}
	for _, name := range []string{"pending", "reconcile", "new", "tree", "branches", "unknown"} {
		_, handled, err := commands.handle(t.Context(), "session", []string{name})
		require.NoError(t, err)
		require.False(t, handled, "safety and read-only handlers must retain their original SessionSlashHandler route")
	}
	for _, name := range []string{"list", "resume"} {
		_, handled, err := commands.handle(t.Context(), "session", []string{name})
		require.NoError(t, err)
		require.True(t, handled)
	}
	require.Contains(t, out.String(), "Physical sessions")
	for _, args := range [][]string{{"list", "extra"}, {"resume", "target", "extra"}, {"entry", "target", "extra"}} {
		_, handled, err := commands.handle(t.Context(), "session", args)
		require.True(t, handled)
		require.Error(t, err)
	}
	after, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, before, after)
	entries, err := os.ReadDir(filepath.Join(store, "sessions"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}
