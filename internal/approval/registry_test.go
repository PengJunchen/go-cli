package approval

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubResolver struct{}

var _ PermissionModeResolver = (*stubResolver)(nil)

func (stubResolver) Name() string                                { return "stub_resolver" }
func (stubResolver) Resolve(_ PermissionMode) ApprovalClassifier { return &AllowAllClassifier{} }

type stubTrustManager struct{}

var _ TrustManager = (*stubTrustManager)(nil)

func (stubTrustManager) IsTrusted(_ context.Context, _ string) bool     { return true }
func (stubTrustManager) TrustProject(_ context.Context, _ string) error { return nil }
func (stubTrustManager) RevokeTrust(_ context.Context, _ string) error  { return nil }
func (stubTrustManager) TrustedProjects() []string                      { return nil }

func TestRegistryClassifierRoundTrip(t *testing.T) {
	orig := GetApprovalClassifier()
	defer RegisterApprovalClassifier(orig)

	custom := &DenyAllClassifier{}
	RegisterApprovalClassifier(custom)
	got := GetApprovalClassifier()
	assert.Equal(t, "deny_all", got.Name())
}

func TestRegistryClassifierNilResets(t *testing.T) {
	orig := GetApprovalClassifier()
	defer RegisterApprovalClassifier(orig)

	RegisterApprovalClassifier(&DenyAllClassifier{})
	RegisterApprovalClassifier(nil)
	got := GetApprovalClassifier()
	assert.Equal(t, "allow_all", got.Name())
}

func TestRegistryStoreRoundTrip(t *testing.T) {
	orig := GetApprovalStore()
	defer RegisterApprovalStore(orig)

	custom := NewInMemoryApprovalStore()
	RegisterApprovalStore(custom)
	got := GetApprovalStore()
	assert.Equal(t, custom, got)
}

func TestRegistryStoreNilResets(t *testing.T) {
	orig := GetApprovalStore()
	defer RegisterApprovalStore(orig)

	RegisterApprovalStore(nil)
	got := GetApprovalStore()
	require.NotNil(t, got)
	_, ok := got.(*InMemoryApprovalStore)
	assert.True(t, ok, "expected *InMemoryApprovalStore after nil reset")
}

func TestRegistryResolverRoundTrip(t *testing.T) {
	orig := GetPermissionModeResolver()
	defer RegisterPermissionModeResolver(orig)

	custom := &stubResolver{}
	RegisterPermissionModeResolver(custom)
	got := GetPermissionModeResolver()
	assert.Equal(t, "stub_resolver", got.Name())
}

func TestRegistryResolverNilResets(t *testing.T) {
	orig := GetPermissionModeResolver()
	defer RegisterPermissionModeResolver(orig)

	RegisterPermissionModeResolver(&stubResolver{})
	RegisterPermissionModeResolver(nil)
	got := GetPermissionModeResolver()
	assert.Equal(t, "permission_mode", got.Name())
}

func TestRegistryPermissionModeRoundTrip(t *testing.T) {
	orig := GetPermissionMode()
	defer RegisterPermissionMode(orig)

	RegisterPermissionMode(PermissionAuto)
	got := GetPermissionMode()
	assert.Equal(t, PermissionAuto, got)
}

func TestRegistryTrustManagerRoundTrip(t *testing.T) {
	orig := GetTrustManager()
	defer RegisterTrustManager(orig)

	custom := &stubTrustManager{}
	RegisterTrustManager(custom)
	got := GetTrustManager()
	assert.Equal(t, custom, got)
}

func TestRegistryTrustManagerNilResets(t *testing.T) {
	orig := GetTrustManager()
	defer RegisterTrustManager(orig)

	RegisterTrustManager(nil)
	got := GetTrustManager()
	require.NotNil(t, got)
	_, ok := got.(*DefaultTrustManager)
	assert.True(t, ok, "expected *DefaultTrustManager after nil reset")
}
