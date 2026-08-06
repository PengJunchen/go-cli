package config

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFeatureFlagRegisterAndIsEnabled(t *testing.T) {
	r := NewFeatureFlagRegistry()
	r.Register(FeatureFlag{Name: "beta", Enabled: true, Description: "beta feature"})

	assert.True(t, r.IsEnabled("beta"))
	assert.False(t, r.IsEnabled("missing"))
}

func TestFeatureFlagSet(t *testing.T) {
	r := NewFeatureFlagRegistry()
	r.Register(FeatureFlag{Name: "beta", Enabled: false})

	require.NoError(t, r.Set("beta", true))
	assert.True(t, r.IsEnabled("beta"))

	require.NoError(t, r.Set("beta", false))
	assert.False(t, r.IsEnabled("beta"))
}

func TestFeatureFlagSetUnknownReturnsError(t *testing.T) {
	r := NewFeatureFlagRegistry()
	err := r.Set("ghost", true)
	require.ErrorIs(t, err, ErrFlagNotFound)
}

func TestFeatureFlagListSorted(t *testing.T) {
	r := NewFeatureFlagRegistry()
	r.Register(FeatureFlag{Name: "zeta", Enabled: false})
	r.Register(FeatureFlag{Name: "alpha", Enabled: true})
	r.Register(FeatureFlag{Name: "mid", Enabled: false})

	list := r.List()
	require.Len(t, list, 3)
	assert.Equal(t, "alpha", list[0].Name)
	assert.Equal(t, "mid", list[1].Name)
	assert.Equal(t, "zeta", list[2].Name)
}

func TestFeatureFlagListIsCopy(t *testing.T) {
	r := NewFeatureFlagRegistry()
	r.Register(FeatureFlag{Name: "beta", Enabled: true})

	list := r.List()
	list[0].Enabled = false

	// Mutating the returned slice does not affect the registry.
	assert.True(t, r.IsEnabled("beta"))
}

func TestFeatureFlagLoadFromConfig(t *testing.T) {
	r := NewFeatureFlagRegistry()
	r.Register(FeatureFlag{Name: "a", Enabled: false})
	r.Register(FeatureFlag{Name: "b", Enabled: false})

	r.LoadFromConfig(map[string]bool{"a": true, "b": false, "unknown": true})

	assert.True(t, r.IsEnabled("a"))
	assert.False(t, r.IsEnabled("b"))
	// Unknown keys are ignored.
	assert.False(t, r.IsEnabled("unknown"))
}

func TestFeatureFlagRegisterOverwrites(t *testing.T) {
	r := NewFeatureFlagRegistry()
	r.Register(FeatureFlag{Name: "x", Enabled: false, Description: "v1"})
	r.Register(FeatureFlag{Name: "x", Enabled: true, Description: "v2"})

	list := r.List()
	require.Len(t, list, 1)
	assert.True(t, list[0].Enabled)
	assert.Equal(t, "v2", list[0].Description)
}

func TestFeatureFlagRegistryConcurrent(t *testing.T) {
	r := NewFeatureFlagRegistry()
	for i := 0; i < 20; i++ {
		r.Register(FeatureFlag{Name: "flag" + itoa(i), Enabled: i%2 == 0})
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			_ = r.Set("flag"+itoa(i%20), i%3 == 0) //nolint:errcheck
		}(i)
		go func(i int) {
			defer wg.Done()
			_ = r.IsEnabled("flag" + itoa(i%20))
		}(i)
		go func() {
			defer wg.Done()
			_ = r.List()
		}()
	}
	wg.Wait()
}
