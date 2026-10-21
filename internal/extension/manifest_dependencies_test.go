package extension

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	api "github.com/pengjunchen/go-cli/pkg/extensionapi/v1"
)

type manifestTrackingExtension struct {
	manifest    api.Manifest
	order       *[]string
	fail        bool
	initialized bool
}

func (e *manifestTrackingExtension) Name() string           { return e.manifest.ID }
func (e *manifestTrackingExtension) Manifest() api.Manifest { return e.manifest }
func (e *manifestTrackingExtension) Init(context.Context, ExtensionRegistry) error {
	e.initialized = true
	*e.order = append(*e.order, "init:"+e.Name())
	if e.fail {
		return errors.New("failed prerequisite")
	}
	return nil
}
func (e *manifestTrackingExtension) Shutdown(context.Context) error {
	*e.order = append(*e.order, "close:"+e.Name())
	return nil
}

func TestManifestDependenciesOrderInitializationAndReverseCleanup(t *testing.T) {
	order := []string{}
	provider := &manifestTrackingExtension{manifest: api.Manifest{ID: "z-provider", Version: "1", HostAPI: api.Version, Provides: []string{"storage@1"}}, order: &order}
	consumer := &manifestTrackingExtension{manifest: api.Manifest{ID: "a-consumer", Version: "1", HostAPI: api.Version, Requires: []string{"storage@1"}}, order: &order}
	loader := newMgrTestPluginLoader("manifest")
	loader.SetResult("extensions", []Extension{consumer, provider})
	manager := NewPluginManager(loader)
	require.NoError(t, manager.Load(context.Background(), []string{"extensions"}))
	declarations := manager.Manifests()
	require.Len(t, declarations, 2)
	declarations[0].Requires[0] = "changed"
	require.Equal(t, "storage@1", manager.Manifests()[0].Requires[0])
	require.NoError(t, manager.Init(context.Background()))
	require.Equal(t, []string{"init:z-provider", "init:a-consumer"}, order)
	require.NoError(t, manager.Shutdown(context.Background()))
	require.Equal(t, []string{"init:z-provider", "init:a-consumer", "close:a-consumer", "close:z-provider"}, order)
}

func TestManifestInvalidGraphNeverInitializesAndStillCleansLoadedOwners(t *testing.T) {
	for _, graph := range []string{"missing", "cycle", "duplicate-provider"} {
		t.Run(graph, func(t *testing.T) {
			order := []string{}
			a := api.Manifest{ID: "a", Version: "1", HostAPI: api.Version, Requires: []string{"b"}, Provides: []string{"a"}}
			b := api.Manifest{ID: "b", Version: "1", HostAPI: api.Version, Provides: []string{"b"}}
			switch graph {
			case "missing":
				a.Requires = []string{"missing"}
			case "cycle":
				b.Requires = []string{"a"}
			case "duplicate-provider":
				a.Requires = nil
				a.Provides = []string{"b"}
			}
			first := &manifestTrackingExtension{manifest: a, order: &order}
			second := &manifestTrackingExtension{manifest: b, order: &order}
			loader := newMgrTestPluginLoader("manifest")
			loader.SetResult("extensions", []Extension{first, second})
			manager := NewPluginManager(loader)
			require.NoError(t, manager.Load(context.Background(), []string{"extensions"}))
			require.Error(t, manager.Init(context.Background()))
			require.False(t, first.initialized)
			require.False(t, second.initialized)
			require.NoError(t, manager.Shutdown(context.Background()))
			require.Len(t, order, 2)
		})
	}
}

func TestManifestFailedPrerequisiteDoesNotInitializeDependent(t *testing.T) {
	order := []string{}
	provider := &manifestTrackingExtension{manifest: api.Manifest{ID: "provider", Version: "1", HostAPI: api.Version, Provides: []string{"storage"}}, order: &order, fail: true}
	consumer := &manifestTrackingExtension{manifest: api.Manifest{ID: "consumer", Version: "1", HostAPI: api.Version, Requires: []string{"storage"}}, order: &order}
	loader := newMgrTestPluginLoader("manifest")
	loader.SetResult("extensions", []Extension{consumer, provider})
	manager := NewPluginManager(loader)
	require.NoError(t, manager.Load(context.Background(), []string{"extensions"}))
	require.Error(t, manager.Init(context.Background()))
	require.True(t, provider.initialized)
	require.False(t, consumer.initialized)
	require.NoError(t, manager.Shutdown(context.Background()))
}
