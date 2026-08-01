//go:build mock

package mock

import (
	"context"
	"sync"

	"github.com/pengjunchen/go-cli/internal/production"
)

// MockAuditLog is a test-only AuditLog that records every Log/Query invocation
// and returns a fixed, in-memory set of entries from Query.
type MockAuditLog struct {
	mu      sync.Mutex
	entries []production.AuditEntry
	logged  []production.AuditEntry
	queried []production.AuditFilter
	err     error
	// QueryErr, when non-nil, is returned by Query instead of stored entries.
	queryErr error
	name     string
}

// Compile-time assertion that the mock satisfies the audit contract.
var _ production.AuditLog = (*MockAuditLog)(nil)

// NewMockAuditLog creates an empty mock audit log.
func NewMockAuditLog() *MockAuditLog {
	return &MockAuditLog{name: "mock-audit-log"}
}

// WithName overrides the identifier returned by Name.
func (m *MockAuditLog) WithName(name string) *MockAuditLog {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.name = name
	return m
}

// Log records the call and appends the entry to the in-memory store.
func (m *MockAuditLog) Log(_ context.Context, entry production.AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logged = append(m.logged, entry)
	if m.err != nil {
		return m.err
	}
	m.entries = append(m.entries, entry)
	return nil
}

// Query records the call and returns the in-memory entries (or queryErr).
func (m *MockAuditLog) Query(_ context.Context, filter production.AuditFilter) ([]production.AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queried = append(m.queried, filter)
	if m.queryErr != nil {
		return nil, m.queryErr
	}
	out := make([]production.AuditEntry, len(m.entries))
	copy(out, m.entries)
	return out, nil
}

// SetErr configures Log to return an error after recording the call.
func (m *MockAuditLog) SetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// SetQueryErr configures Query to return err.
func (m *MockAuditLog) SetQueryErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queryErr = err
}

// LoggedEntries returns a copy of all entries passed to Log.
func (m *MockAuditLog) LoggedEntries() []production.AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]production.AuditEntry, len(m.logged))
	copy(out, m.logged)
	return out
}

// LoggedFilters returns a copy of all filters passed to Query.
func (m *MockAuditLog) LoggedFilters() []production.AuditFilter {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]production.AuditFilter, len(m.queried))
	copy(out, m.queried)
	return out
}

// LogCount returns the number of Log invocations.
func (m *MockAuditLog) LogCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.logged)
}

// Name returns the audit log identifier.
func (m *MockAuditLog) Name() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.name
}
