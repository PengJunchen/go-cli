package production

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegistryLazyDefaults verifies that unset registry entries return fresh
// default implementations, and that registering a custom impl is retrievable.
func TestRegistryLazyDefaults(t *testing.T) {
	// Lazy defaults when nothing registered.
	assert.NotNil(t, GetIdempotentCache())
	assert.NotNil(t, GetAuditLog())
	assert.NotNil(t, GetTelemetry())

	// Register custom implementations (guard is satisfied by nil->default).
	RegisterIdempotentCache(nil)
	RegisterAuditLog(nil)
	RegisterTelemetry(nil)
	assert.NotNil(t, GetIdempotentCache())
	assert.NotNil(t, GetAuditLog())
	assert.NotNil(t, GetTelemetry())
}

func TestRegistryCustomImplementations(t *testing.T) {
	origCache, origAudit, origTel := GetIdempotentCache(), GetAuditLog(), GetTelemetry()
	defer func() {
		RegisterIdempotentCache(origCache)
		RegisterAuditLog(origAudit)
		RegisterTelemetry(origTel)
	}()

	customCache := NewFIFOIdempotentCache(8, WithName("reg-cache"))
	customAudit := NewDefaultAuditLog("", WithAuditName("reg-audit"))
	customTel := NewDefaultTelemetry(WithName("reg-telemetry"))

	RegisterIdempotentCache(customCache)
	RegisterAuditLog(customAudit)
	RegisterTelemetry(customTel)

	require.Equal(t, "reg-cache", GetIdempotentCache().Name())
	require.Equal(t, "reg-audit", GetAuditLog().Name())
	require.Equal(t, "reg-telemetry", GetTelemetry().Name())
}
