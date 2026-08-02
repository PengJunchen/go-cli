package config

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEnvKey verifies the environment variable name derivation helper.
func TestEnvKey(t *testing.T) {
	assert.Equal(t, "GO_CLI_VERBOSE", envKey("GO_CLI", "VERBOSE"))
	assert.Equal(t, "MY_PREFIX_KEY", envKey("MY_PREFIX", "KEY"))
	assert.Equal(t, "_KEY", envKey("", "KEY"))
}

// TestOverlayValue_MismatchedTypeIsSafe verifies overlayValue is a no-op when
// the destination and source reflect values have incompatible types, rather
// than panicking. This guards a hand-written reflection merge against
// accidental callers passing unrelated reflect values.
func TestOverlayValue_MismatchedTypeIsSafe(t *testing.T) {
	dst := new(Config)
	srcStr := reflect.ValueOf("not-a-config")
	overlayValue(reflect.ValueOf(dst).Elem(), srcStr)

	// Destination must be left on its zero value entirely.
	assert.Equal(t, Config{}, *dst)

	// An invalid (zero) reflect.Value is also a safe no-op.
	overlayValue(reflect.Value{}, reflect.ValueOf(dst).Elem())
	overlayValue(reflect.ValueOf(dst).Elem(), reflect.Value{})
}

// TestMergeConfigs_NilOverIsCopy verifies merging a nil overlay returns an
// equivalent independent copy of the base (no shared backing data).
func TestMergeConfigs_NilOverIsCopy(t *testing.T) {
	base := defaultConfig()
	base.Provider.Name = "base-keep"
	merged := mergeConfigs(base, &Config{})

	require.NotSame(t, base, merged)
	assert.Equal(t, base.Provider.Name, merged.Provider.Name)

	// Mutating the merged config must not affect the base.
	merged.Provider.Name = "changed"
	assert.Equal(t, "base-keep", base.Provider.Name)
}

// TestOverlayValue_PointerNilUnsetDriver verifies an explicit nil pointer in
// the overlay is treated as "unset" and leaves the destination pointer intact,
// whereas a non-nil pointer replaces it wholesale.
func TestOverlayValue_PointerNilUnsetDriver(t *testing.T) {
	base := defaultConfig()
	enabled := true
	base.Tracing.Enabled = &enabled

	// A nil pointer means "unset": the base value is preserved.
	overNil := &Config{Tracing: TracingConfig{Enabled: nil}}
	mergedNil := mergeConfigs(base, overNil)
	require.NotNil(t, mergedNil.Tracing.Enabled)
	assert.True(t, *mergedNil.Tracing.Enabled)

	// A non-nil pointer replaces the destination wholesale, even when it is
	// false, enabling an explicit false override.
	falseVal := false
	overFalse := &Config{Tracing: TracingConfig{Enabled: &falseVal}}
	mergedFalse := mergeConfigs(base, overFalse)
	require.NotNil(t, mergedFalse.Tracing.Enabled)
	assert.False(t, *mergedFalse.Tracing.Enabled)
}
