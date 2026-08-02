package extension

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultConfigProviderName verifies the compile-time default provider is
// identified as "default".
func TestDefaultConfigProviderName(t *testing.T) {
	var p ConfigProvider = defaultConfigProvider{}
	assert.Equal(t, "default", p.Name())
}

// TestDefaultConfigProviderLoadUnimplemented verifies Load reports the
// not-implemented sentinel error.
func TestDefaultConfigProviderLoadUnimplemented(t *testing.T) {
	p := defaultConfigProvider{}
	err := p.Load(context.Background(), "key", new(string))
	require.Error(t, err)
	assert.True(t, errors.Is(err, errDefaultConfig))
}

// TestDefaultConfigProviderWatchUnimplemented verifies Watch reports the
// not-implemented sentinel error and returns a nil channel.
func TestDefaultConfigProviderWatchUnimplemented(t *testing.T) {
	p := defaultConfigProvider{}
	ch, err := p.Watch(context.Background(), "key")
	assert.Nil(t, ch)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errDefaultConfig))
}

// TestConfigChangeRoundTrip verifies ConfigChange JSON round-trips key and
// values.
func TestConfigChangeRoundTrip(t *testing.T) {
	cc := ConfigChange{Key: "a.b", OldValue: 1, NewValue: 2, Timestamp: time.Unix(0, 0)}
	data, err := json.Marshal(cc)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"key":"a.b"`)
	assert.Contains(t, string(data), `"old_value":1`)
	assert.Contains(t, string(data), `"new_value":2`)

	var back ConfigChange
	require.NoError(t, json.Unmarshal(data, &back))
	assert.Equal(t, cc.Key, back.Key)
	oldVal, ok := back.OldValue.(float64)
	require.True(t, ok, "numbers should decode to float64")
	assert.Equal(t, float64(1), oldVal)
}
