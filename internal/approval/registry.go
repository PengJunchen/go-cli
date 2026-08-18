package approval

import (
	"log/slog"
	"sync"
)

// registry is a minimal process-wide holder for the active classifier,
// store, permission-mode resolver and trust manager. It lets callers override
// the wiring at startup while keeping a sensible deny-first default otherwise.
type registry struct {
	classifier ApprovalClassifier
	store      ApprovalStore

	permissionModeResolver PermissionModeResolver
	permissionMode         PermissionMode
	trustManager           TrustManager
}

var (
	registryMu sync.RWMutex
	defaultReg = &registry{
		classifier:             &DenyAllClassifier{},
		store:                  NewInMemoryApprovalStore(),
		permissionModeResolver: NewDefaultPermissionModeResolver(),
		permissionMode:         PermissionDefault,
		trustManager:           NewDefaultTrustManager(nil),
	}
)

// RegisterApprovalClassifier swaps in a new active classifier. Pass nil to
// reset to the deny-all classifier.
func RegisterApprovalClassifier(classifier ApprovalClassifier) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if classifier == nil {
		classifier = &DenyAllClassifier{}
	}
	slog.Info("approval.register.classifier", "classifier", classifier.Name())
	defaultReg.classifier = classifier
}

// RegisterApprovalStore swaps in a new active store. Pass nil to reset to an
// in-memory store.
func RegisterApprovalStore(store ApprovalStore) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if store == nil {
		store = NewInMemoryApprovalStore()
	}
	slog.Info("approval.register.store")
	defaultReg.store = store
}

// GetApprovalClassifier returns the currently active classifier.
func GetApprovalClassifier() ApprovalClassifier {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return defaultReg.classifier
}

// GetApprovalStore returns the currently active store.
func GetApprovalStore() ApprovalStore {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return defaultReg.store
}

// RegisterPermissionModeResolver swaps in a new permission-mode resolver. Pass
// nil to reset to the default resolver.
func RegisterPermissionModeResolver(r PermissionModeResolver) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if r == nil {
		r = NewDefaultPermissionModeResolver()
	}
	slog.Info("approval.register.permission_mode_resolver", "resolver", r.Name())
	defaultReg.permissionModeResolver = r
}

// GetPermissionModeResolver returns the currently active permission-mode
// resolver.
func GetPermissionModeResolver() PermissionModeResolver {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return defaultReg.permissionModeResolver
}

// RegisterPermissionMode sets the currently active permission mode. Changes are
// logged so operators can trace posture switches.
func RegisterPermissionMode(mode PermissionMode) {
	registryMu.Lock()
	defer registryMu.Unlock()
	slog.Info("approval.register.permission_mode", "permission_mode", mode.String())
	defaultReg.permissionMode = mode
}

// GetPermissionMode returns the currently active permission mode.
func GetPermissionMode() PermissionMode {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return defaultReg.permissionMode
}

// RegisterTrustManager swaps in a new trust manager. Pass nil to reset to a
// manager backed by an in-memory trust store.
func RegisterTrustManager(tm TrustManager) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if tm == nil {
		tm = NewDefaultTrustManager(nil)
	}
	slog.Info("approval.register.trust_manager")
	defaultReg.trustManager = tm
}

// GetTrustManager returns the currently active trust manager.
func GetTrustManager() TrustManager {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return defaultReg.trustManager
}
