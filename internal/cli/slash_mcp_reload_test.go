package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type slashMCPBackendFixture struct {
	loads, refreshes []string
	err              error
}

func (f *slashMCPBackendFixture) CapabilityStatus(context.Context) (CapabilityStatus, error) {
	return CapabilityStatus{MCP: []MCPServerStatus{{Name: "selected", State: "loaded"}}}, f.err
}
func (f *slashMCPBackendFixture) ReloadCapabilities(context.Context) error { return f.err }
func (f *slashMCPBackendFixture) LoadMCPServer(_ context.Context, name string) error {
	f.loads = append(f.loads, name)
	return f.err
}
func (f *slashMCPBackendFixture) RefreshMCPServer(_ context.Context, name string) error {
	f.refreshes = append(f.refreshes, name)
	return f.err
}

type slashMCPDependenciesFixture struct {
	*slashContext
	reloadCount, reloadCalls int
}

func (f *slashMCPDependenciesFixture) ReloadMCP(context.Context) (int, error) {
	f.reloadCalls++
	return f.reloadCount, nil
}

func TestSlashMCPHandlerRoutesRegisteredSubcommands(t *testing.T) {
	for _, test := range []struct {
		name          string
		args          []string
		withoutLazy   bool
		wantOutput    string
		wantError     string
		wantLoads     []string
		wantRefreshes []string
		wantReloads   int
	}{
		{name: "default-list", wantOutput: "selected: loaded"},
		{name: "list", args: []string{"list"}, wantOutput: "selected: loaded"},
		{name: "load", args: []string{"load", "selected"}, wantOutput: "selected: loaded", wantLoads: []string{"selected"}},
		{name: "legacy-reload", args: []string{"reload"}, withoutLazy: true, wantOutput: "Triggered reload on 2", wantReloads: 1},
		{name: "named-reload", args: []string{"reload", "selected"}, wantOutput: "refreshed for the next run", wantRefreshes: []string{"selected"}},
		{name: "usage-without-backend", withoutLazy: true, wantOutput: "list | load <server> | reload [server]"},
		{name: "unknown", args: []string{"invalid"}, wantOutput: "Unknown subcommand"},
		{name: "load-needs-name", args: []string{"load"}, wantError: "load <server>"},
		{name: "list-extra-args", args: []string{"list", "selected"}, wantError: "Usage: /mcp"},
		{name: "reload-extra-args", args: []string{"reload", "one", "two"}, wantError: "reload [server]"},
		{name: "named-reload-unavailable", args: []string{"reload", "selected"}, withoutLazy: true, wantError: "named MCP refresh is unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			backend := &slashMCPBackendFixture{}
			deps := &slashMCPDependenciesFixture{slashContext: &slashContext{out: &out}, reloadCount: 2}
			if !test.withoutLazy {
				deps.capabilities = backend
			}
			_, err := (&MCPHandler{}).Handle(context.Background(), test.args, deps)
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
			} else {
				require.NoError(t, err)
				require.Contains(t, out.String(), test.wantOutput)
			}
			require.Equal(t, test.wantLoads, backend.loads)
			require.Equal(t, test.wantRefreshes, backend.refreshes)
			require.Equal(t, test.wantReloads, deps.reloadCalls)
		})
	}
}

func TestSlashMCPCompletionUsesCanonicalSubcommands(t *testing.T) {
	completer := NewSlashCommandCompleterFromRegistry(defaultSlashReg)
	matches, start := completer.Complete("/mcp ", len("/mcp "))
	require.Equal(t, len("/mcp "), start)
	var names []string
	for _, match := range matches {
		names = append(names, match.Text)
		require.NotEmpty(t, match.Description)
	}
	require.ElementsMatch(t, []string{"list", "load", "reload"}, names)
	matches, _ = completer.Complete("/mcp lo", len("/mcp lo"))
	require.Len(t, matches, 1)
	require.Equal(t, "load", matches[0].Text)
}

func TestSlashMCPHandlerPreservesBackendErrors(t *testing.T) {
	failure := errors.New("candidate rejected")
	for _, args := range [][]string{{"list"}, {"load", "selected"}, {"reload", "selected"}} {
		var out bytes.Buffer
		deps := &slashContext{out: &out, capabilities: &slashMCPBackendFixture{err: failure}}
		_, err := (&MCPHandler{}).Handle(context.Background(), args, deps)
		require.ErrorIs(t, err, failure)
		require.Empty(t, out.String())
	}
}
