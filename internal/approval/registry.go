package approval

import (
	"log/slog"
	"sync"
)

// registry is a minimal process-wide holder for the active classifier and
// store. It lets callers override the wiring at startup while keeping a
// sensible deny-first default otherwise.
type registry struct {
	classifier ApprovalClassifier
	store      ApprovalStore
}

var (
	registryMu sync.RWMutex
	defaultReg = &registry{classifier: &AllowAllClassifier{}, store: NewInMemoryApprovalStore()}
)

// RegisterApprovalClassifier swaps in a new active classifier. Pass nil to
// reset to the allow-all classifier.
func RegisterApprovalClassifier(classifier ApprovalClassifier) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if classifier == nil {
		classifier = &AllowAllClassifier{}
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
