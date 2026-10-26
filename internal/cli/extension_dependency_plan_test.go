package cli

import (
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/extension"
	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

type manifestPlanFixture struct {
	manifest            api.Manifest
	initialized, closed *atomic.Int32
}

func (f *manifestPlanFixture) Name() string           { return f.manifest.ID }
func (f *manifestPlanFixture) Manifest() api.Manifest { return f.manifest.Clone() }
func (f *manifestPlanFixture) Init(context.Context, extension.ExtensionRegistry) error {
	f.initialized.Add(1)
	return nil
}
func (f *manifestPlanFixture) Shutdown(context.Context) error { f.closed.Add(1); return nil }

type manifestPlanLoader struct {
	entries map[string]extension.Extension
}

func (f *manifestPlanLoader) Name() string { return "manifest-plan-fixture" }
func (f *manifestPlanLoader) Load(_ context.Context, path string) ([]extension.Extension, error) {
	return []extension.Extension{f.entries[path]}, nil
}

func TestAssemblyRejectsInvalidExtensionDependencyPlanBeforeInit(t *testing.T) {
	tests := []struct {
		name                                       string
		requiresA, providesA, requiresB, providesB []string
	}{
		{name: "missing", requiresA: []string{"missing-capability"}},
		{name: "cycle", requiresA: []string{"cap-b"}, providesA: []string{"cap-a"}, requiresB: []string{"cap-a"}, providesB: []string{"cap-b"}},
		{name: "duplicate-provider", providesA: []string{"shared"}, providesB: []string{"shared"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Chdir(t.TempDir())
			var initialized, closed atomic.Int32
			makeEntry := func(id string, requires, provides []string) *manifestPlanFixture {
				return &manifestPlanFixture{manifest: api.Manifest{ID: id, Version: "1", HostAPI: api.Version, Requires: requires, Provides: provides}, initialized: &initialized, closed: &closed}
			}
			loader := &manifestPlanLoader{entries: map[string]extension.Extension{"a": makeEntry("a", test.requiresA, test.providesA), "b": makeEntry("b", test.requiresB, test.providesB)}}
			cfg := newAssembleTestConfig(t)
			enabled := true
			cfg.Extensions = config.ExtensionsConfig{Enabled: &enabled, PluginPaths: []string{"a", "b"}}
			assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, func(options *assembleConfig) { options.pluginLoader = loader })
			require.Error(t, err)
			require.Nil(t, assembly)
			require.Zero(t, initialized.Load(), "graph validation precedes any extension Init")
			require.EqualValues(t, 2, closed.Load(), "loaded resources are still owned and closed on rejection")
		})
	}
}
