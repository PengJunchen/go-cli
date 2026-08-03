package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestSessionVersionString(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	assert.Equal(t, "v1", SessionV1.String())
	assert.Equal(t, "v2", SessionV2.String())
	assert.Equal(t, "v3", SessionV3.String())
	assert.Equal(t, SessionV3, CurrentVersion)
}

func TestMigrationChain_DefaultMigrations(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	chain := NewMigrationChain()
	data := map[string]any{"id": "sess-1"}

	result, err := chain.Migrate(data, SessionV1)
	require.NoError(t, err)

	assert.Equal(t, "sess-1", result["id"], "original fields must be preserved")
	_, ok := result["metadata"]
	assert.True(t, ok, "v1->v2 should add metadata")
	_, ok = result["trace_id"]
	assert.True(t, ok, "v2->v3 should add trace_id")
}

func TestMigrationChain_V2ToV3(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	chain := NewMigrationChain()
	data := map[string]any{
		"id":       "sess-1",
		"metadata": map[string]any{"key": "val"},
	}

	result, err := chain.Migrate(data, SessionV2)
	require.NoError(t, err)

	assert.Equal(t, "sess-1", result["id"])
	_, ok := result["metadata"]
	assert.True(t, ok, "metadata should already exist")
	_, ok = result["trace_id"]
	assert.True(t, ok, "v2->v3 should add trace_id")
}

func TestMigrationChain_AlreadyCurrentVersion(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	chain := NewMigrationChain()
	data := map[string]any{"id": "sess-1", "metadata": map[string]any{}, "trace_id": "t-123"}

	result, err := chain.Migrate(data, SessionV3)
	require.NoError(t, err)
	assert.Equal(t, "sess-1", result["id"])
	assert.Equal(t, "t-123", result["trace_id"])
}

func TestMigrationChain_PreservesExistingFields(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	chain := NewMigrationChain()
	data := map[string]any{
		"id":          "sess-1",
		"metadata":    map[string]any{"existing": true},
		"trace_id":    "t-existing",
		"other_field": "value",
	}

	result, err := chain.Migrate(data, SessionV1)
	require.NoError(t, err)

	meta, ok := result["metadata"].(map[string]any)
	require.True(t, ok)
	assert.True(t, meta["existing"].(bool), "existing metadata should be preserved")

	assert.Equal(t, "t-existing", result["trace_id"], "existing trace_id should be preserved")
	assert.Equal(t, "value", result["other_field"])
}

func TestMigrationChain_NilData(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	chain := NewMigrationChain()
	result, err := chain.Migrate(nil, SessionV1)
	require.NoError(t, err)
	require.NotNil(t, result)
	_, ok := result["metadata"]
	assert.True(t, ok)
	_, ok = result["trace_id"]
	assert.True(t, ok)
}

func TestMigrationChain_MissingMigration(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	chain := NewMigrationChain()
	// Unregister the v2->v3 migration to force a missing-migration error.
	chain.mu.Lock()
	delete(chain.migrations, SessionV2)
	chain.mu.Unlock()

	data := map[string]any{"id": "sess-1", "metadata": map[string]any{}}
	_, err := chain.Migrate(data, SessionV1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no migration registered")
}

func TestMigrationChain_CustomMigration(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	chain := NewMigrationChain()
	customCalled := false
	chain.Register(SessionV2, func(data map[string]any) (map[string]any, error) {
		customCalled = true
		data["trace_id"] = "custom-trace"
		return data, nil
	})

	data := map[string]any{"id": "sess-1", "metadata": map[string]any{}}
	result, err := chain.Migrate(data, SessionV1)
	require.NoError(t, err)
	assert.True(t, customCalled, "custom migration should have been called")
	assert.Equal(t, "custom-trace", result["trace_id"])
}

func TestMigrationChain_ConcurrentRegister(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	chain := NewMigrationChain()

	done := make(chan struct{})
	go func() {
		defer close(done)
		chain.Register(SessionV1, migrateV1ToV2)
	}()
	<-done

	data := map[string]any{"id": "sess-1"}
	result, err := chain.Migrate(data, SessionV1)
	require.NoError(t, err)
	_, ok := result["metadata"]
	assert.True(t, ok)
}
