package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newScopedReadTestRegistry(t *testing.T) *DefaultToolRegistry {
	t.Helper()
	registry, ok := NewDefaultToolRegistry().(*DefaultToolRegistry)
	require.True(t, ok)
	return registry
}

func TestRegisterDefaultsMintsScopedWorkspaceReadForConcreteReaders(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	registryFactories := []struct {
		name string
		new  func(*DefaultToolRegistry) ToolRegistry
	}{
		{name: "default", new: func(base *DefaultToolRegistry) ToolRegistry { return base }},
		{name: "deferred", new: func(base *DefaultToolRegistry) ToolRegistry { return NewDeferredToolRegistryAdapter(base) }},
		{name: "middleware", new: func(base *DefaultToolRegistry) ToolRegistry {
			return NewMiddlewareToolRegistry(NewDeferredToolRegistryAdapter(base))
		}},
	}

	for _, registryFactory := range registryFactories {
		t.Run(registryFactory.name, func(t *testing.T) {
			base := newScopedReadTestRegistry(t)
			outer := registryFactory.new(base)
			require.NoError(t, RegisterDefaults(ctx, outer, WithRegisteredPathWhitelist([]string{root})))

			for _, name := range []string{"read", "ls", "grep", "find"} {
				snapshot, err := base.toolRegistrationSnapshot(ctx, name)
				require.NoError(t, err, name)
				assert.Equal(t, RegistrationSourceBuiltin, snapshot.identity.RegistrationSource(), name)
				assert.Equal(t, []TrustedToolEffect{EffectWorkspaceRead}, snapshot.effects.Values(), name)
				assert.NotEqual(t, [32]byte{}, snapshot.identity.Scope().Fingerprint(), name)

				if name == "read" || name == "ls" {
					assert.Equal(t, CompletionInlinePipelineJoined, snapshot.completionProof, name)
				} else {
					assert.Equal(t, completionScopedSearchJoined, snapshot.completionProof, name, "scoped search readers capture a mode whose final proof depends on their execution path")
				}
			}
		})
	}
}

func TestScopedWorkspaceReadScopeUsesCanonicalWhitelistRoots(t *testing.T) {
	ctx := context.Background()
	rootA := t.TempDir()
	rootB := t.TempDir()
	rootC := t.TempDir()
	rootAlias := filepath.Join(t.TempDir(), "canonical-alias")
	require.NoError(t, os.Symlink(rootA, rootAlias))

	registerRead := func(roots []string) [32]byte {
		t.Helper()
		registry := newScopedReadTestRegistry(t)
		require.NoError(t, RegisterDefaults(ctx, registry, WithRegisteredPathWhitelist(roots)))
		snapshot, err := registry.toolRegistrationSnapshot(ctx, "read")
		require.NoError(t, err)
		return snapshot.identity.Scope().Fingerprint()
	}

	first := registerRead([]string{rootA, rootB})
	permutedWithDuplicate := registerRead([]string{rootB, rootAlias, filepath.Join(rootA, "..", filepath.Base(rootA)), rootA})
	different := registerRead([]string{rootA, rootC})

	assert.NotEqual(t, [32]byte{}, first)
	assert.Equal(t, first, permutedWithDuplicate, "scope identity is based on the executor's canonical root set")
	assert.NotEqual(t, first, different, "distinct executor roots require distinct approval identity")
}

func TestUnscopedAndInvalidReadersKeepTheirExistingRegistrationMetadata(t *testing.T) {
	ctx := context.Background()
	registry := newScopedReadTestRegistry(t)
	require.NoError(t, RegisterDefaults(ctx, registry))

	for _, name := range []string{"read", "ls", "grep", "find"} {
		snapshot, err := registry.toolRegistrationSnapshot(ctx, name)
		require.NoError(t, err, name)
		assert.Equal(t, []TrustedToolEffect{EffectUnknown}, snapshot.effects.Values(), name)
		assert.Equal(t, [32]byte{}, snapshot.identity.Scope().Fingerprint(), name)
		if name == "read" || name == "ls" {
			assert.Equal(t, RegistrationSourceBuiltin, snapshot.identity.RegistrationSource(), name)
			assert.Equal(t, CompletionInlinePipelineJoined, snapshot.completionProof, name)
		} else {
			assert.Equal(t, RegistrationSourceToolRegistry, snapshot.identity.RegistrationSource(), name)
			assert.Equal(t, completionScopedSearchJoined, snapshot.completionProof, name, "search completion authority must not depend on workspace scope")
		}
	}

	invalid := newPathWhitelist([]string{"unresolvable"}, func(string) (string, error) {
		return "", errors.New("injected absolute-path resolution failure")
	})
	require.True(t, invalid.invalid)

	invalidRegistry := newScopedReadTestRegistry(t)
	invalidReaders := []ToolDefinition{
		NewReadTool(),
		NewLSTool(),
		NewGrepTool(),
		NewFindTool(),
	}
	for _, reader := range invalidReaders {
		switch tool := reader.(type) {
		case *ReadTool:
			tool.whitelist = invalid
		case *LSTool:
			tool.whitelist = invalid
		case *GrepTool:
			tool.whitelist = invalid
		case *FindTool:
			tool.whitelist = invalid
		}
		require.NoError(t, registerDefaultTool(ctx, invalidRegistry, reader), reader.Name())

		snapshot, err := invalidRegistry.toolRegistrationSnapshot(ctx, reader.Name())
		require.NoError(t, err, reader.Name())
		assert.Equal(t, []TrustedToolEffect{EffectUnknown}, snapshot.effects.Values(), reader.Name())
		assert.Equal(t, [32]byte{}, snapshot.identity.Scope().Fingerprint(), reader.Name())
		if reader.Name() == "read" || reader.Name() == "ls" {
			assert.Equal(t, RegistrationSourceBuiltin, snapshot.identity.RegistrationSource(), reader.Name())
			assert.Equal(t, CompletionInlinePipelineJoined, snapshot.completionProof, reader.Name())
		} else {
			assert.Equal(t, RegistrationSourceToolRegistry, snapshot.identity.RegistrationSource(), reader.Name())
			assert.Equal(t, completionScopedSearchJoined, snapshot.completionProof, reader.Name(), "invalid scope still permits only path-completion attestation")
		}
	}
}

func TestUnreviewedToolNamedGrepDoesNotReceiveSearchCompletionMode(t *testing.T) {
	ctx := context.Background()
	base := newScopedReadTestRegistry(t)
	custom := &stubTool{name: "grep"}
	require.Error(t, base.registerTrustedScopedWorkspaceRead(ctx, custom), "the trusted scoped-reader path accepts only concrete reviewed reader types")
	require.NoError(t, base.Register(ctx, custom), "ordinary custom registration remains available")

	snapshot, err := base.toolRegistrationSnapshot(ctx, "grep")
	require.NoError(t, err)
	assert.Equal(t, RegistrationSourceToolRegistry, snapshot.identity.RegistrationSource())
	assert.Equal(t, []TrustedToolEffect{EffectUnknown}, snapshot.effects.Values())
	assert.Equal(t, [32]byte{}, snapshot.identity.Scope().Fingerprint())
	assert.Empty(t, snapshot.completionProof, "a matching tool name cannot inherit first-party search completion authority")
}

type scopedReadReplacement struct {
	calls int
}

func (*scopedReadReplacement) Name() string        { return "read" }
func (*scopedReadReplacement) Description() string { return "ordinary same-name read replacement" }
func (t *scopedReadReplacement) Execute(context.Context, ToolCall) (*ToolResult, error) {
	t.calls++
	return &ToolResult{Output: "replacement"}, nil
}

func TestScopedReaderReplacementChangesScopeAndStalesPriorInvocation(t *testing.T) {
	ctx := context.Background()
	rootA := t.TempDir()
	rootB := t.TempDir()
	registry := newScopedReadTestRegistry(t)
	require.NoError(t, RegisterDefaults(ctx, registry, WithRegisteredPathWhitelist([]string{rootA})))
	protected := NewMiddlewareToolRegistry(registry)
	configurePreparedRegistry(t, protected, allowPreparedCall, PreparedToolStages{})

	invocation, err := protected.PrepareToolCall(ctx, ToolCall{
		Name: "read",
		Args: map[string]any{"path": filepath.Join(rootA, "inside.txt")},
	})
	require.NoError(t, err)
	scopeA := invocation.ToolIdentity().Scope().Fingerprint()
	require.NotEqual(t, [32]byte{}, scopeA)

	readerB := NewReadTool(WithReadPathWhitelist([]string{rootB}))
	require.NoError(t, registerDefaultTool(ctx, registry, readerB))
	snapshotB, err := registry.toolRegistrationSnapshot(ctx, "read")
	require.NoError(t, err)
	scopeB := snapshotB.identity.Scope().Fingerprint()
	assert.NotEqual(t, scopeA, scopeB)
	assert.Equal(t, []TrustedToolEffect{EffectWorkspaceRead}, snapshotB.effects.Values())

	_, err = invocation.Dispatch(ctx)
	require.ErrorIs(t, err, ErrDispatchFenceChanged)
	assert.False(t, invocation.DispatchCommitted())
}

func TestOrdinarySameNameReaderRegistrationCannotKeepScopedReadMetadata(t *testing.T) {
	ctx := context.Background()
	registry := newScopedReadTestRegistry(t)
	require.NoError(t, RegisterDefaults(ctx, registry, WithRegisteredPathWhitelist([]string{t.TempDir()})))

	replacement := &scopedReadReplacement{}
	require.NoError(t, registry.Register(ctx, replacement))
	snapshot, err := registry.toolRegistrationSnapshot(ctx, "read")
	require.NoError(t, err)
	assert.Equal(t, RegistrationSourceToolRegistry, snapshot.identity.RegistrationSource())
	assert.Equal(t, []TrustedToolEffect{EffectUnknown}, snapshot.effects.Values())
	assert.Equal(t, [32]byte{}, snapshot.identity.Scope().Fingerprint())
	assert.Empty(t, snapshot.completionProof)
}

func TestTrustedScopedWorkspaceReadRejectsSameNameCustomDefinition(t *testing.T) {
	ctx := context.Background()
	registry := newScopedReadTestRegistry(t)
	replacement := &scopedReadReplacement{}

	err := registry.registerTrustedScopedWorkspaceRead(ctx, replacement)
	require.Error(t, err)
	_, err = registry.toolRegistrationSnapshot(ctx, "read")
	require.ErrorIs(t, err, ErrToolNotFound)
}
