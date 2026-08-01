package mock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type configTarget struct {
	Model string  `json:"model"`
	Temp  float64 `json:"temperature"`
}

func TestMockConfigProviderSetConfigAndLoad(t *testing.T) {
	provider := NewMockConfigProvider()
	provider.SetConfig("model.name", "mock-model")

	var target string
	err := provider.Load(context.Background(), "model.name", &target)
	require.NoError(t, err)
	assert.Equal(t, "mock-model", target)
}

func TestMockConfigProviderLoadTypeConversion(t *testing.T) {
	provider := NewMockConfigProvider()
	// Load a scalar and a structured value into different target shapes.
	provider.SetConfig("temperature", 0.7)

	var temp float64
	err := provider.Load(context.Background(), "temperature", &temp)
	require.NoError(t, err)
	assert.Equal(t, 0.7, temp)

	provider.SetConfig("block", map[string]any{"model": "gpt", "temperature": 0.5})
	var block configTarget
	require.NoError(t, provider.Load(context.Background(), "block", &block))
	assert.Equal(t, "gpt", block.Model)
	assert.Equal(t, 0.5, block.Temp)
}

func TestMockConfigProviderLoadMissingKey(t *testing.T) {
	provider := NewMockConfigProvider()

	var target string
	err := provider.Load(context.Background(), "unknown.key", &target)
	require.Error(t, err)
}

func TestMockConfigProviderWatchReceivesNotify(t *testing.T) {
	provider := NewMockConfigProvider()

	ch, err := provider.Watch(context.Background(), "model.temp")
	require.NoError(t, err)

	provider.NotifyChange("model.temp", 0.5, 0.7)

	select {
	case change := <-ch:
		assert.Equal(t, "model.temp", change.Key)
		assert.Equal(t, 0.5, change.OldValue)
		assert.Equal(t, 0.7, change.NewValue)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for config change notification")
	}
}

func TestMockConfigProviderBufferedChannelNonBlocking(t *testing.T) {
	provider := NewMockConfigProvider()

	ch, err := provider.Watch(context.Background(), "k")
	require.NoError(t, err)

	// Overflow the buffer; NotifyChange must not block even when full.
	for i := 0; i < 40; i++ {
		provider.NotifyChange("k", i, i+1)
	}

	// At least one event must have been buffered.
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("expected at least one buffered change event")
	}
}
