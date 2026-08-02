package extension_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/mock"
)

// TestConfigProviderLoadTyped verifies Load unmarshals a stored value into a
// typed target via JSON round-tripping.
func TestConfigProviderLoadTyped(t *testing.T) {
	p := mock.NewMockConfigProvider()
	p.SetConfig("threshold", 12)
	var got int
	require.NoError(t, p.Load(context.Background(), "threshold", &got))
	assert.Equal(t, 12, got)
}

// TestConfigProviderLoadStruct verifies Load works with a struct target and
// respects JSON field names.
func TestConfigProviderLoadStruct(t *testing.T) {
	type cfg struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	p := mock.NewMockConfigProvider()
	p.SetConfig("app", cfg{Name: "x", Count: 3})
	var got cfg
	require.NoError(t, p.Load(context.Background(), "app", &got))
	assert.Equal(t, cfg{Name: "x", Count: 3}, got)
}

// TestConfigProviderLoadMissingKey verifies Load errors for an absent key.
func TestConfigProviderLoadMissingKey(t *testing.T) {
	p := mock.NewMockConfigProvider()
	var got string
	err := p.Load(context.Background(), "missing", &got)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config not found")
}

// TestConfigProviderName verifies the mock provider exposes its name.
func TestConfigProviderName(t *testing.T) {
	var p extension.ConfigProvider = mock.NewMockConfigProvider()
	assert.Equal(t, "mock", p.Name())
}

// TestConfigProviderWatchDeliversChange verifies Watch returns a channel that
// receives NotifyChange events for the watched key.
func TestConfigProviderWatchDeliversChange(t *testing.T) {
	p := mock.NewMockConfigProvider()
	ctx := context.Background()
	ch, err := p.Watch(ctx, "app.port")
	require.NoError(t, err)
	require.NotNil(t, ch)

	p.NotifyChange("app.port", 8080, 9090)

	var change extension.ConfigChange
	select {
	case change = <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected a config change notification")
	}
	assert.Equal(t, "app.port", change.Key)
	assert.Equal(t, 8080, change.OldValue)
	assert.Equal(t, 9090, change.NewValue)
	assert.False(t, change.Timestamp.IsZero())
}

// TestConfigProviderGetConfig verifies the mock's raw accessor round-trips.
func TestConfigProviderGetConfig(t *testing.T) {
	p := mock.NewMockConfigProvider()
	p.SetConfig("k", "v")
	got, ok := p.GetConfig("k")
	require.True(t, ok)
	assert.Equal(t, "v", got)
	_, ok = p.GetConfig("absent")
	assert.False(t, ok)
}
