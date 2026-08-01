//go:build mock

package mock

import (
	"context"
	"sync"

	"github.com/pengjunchen/go-cli/internal/session"
)

// MockBranchSummary is a test-only session.BranchSummary that returns a fixed
// summary and records the entries passed to each Summarize call. It lets
// wiring tests drive the departed-branch summary behavior without an LLM.
type MockBranchSummary struct {
	mu      sync.Mutex
	summary string
	calls   []SessionEntryList
}

// SessionEntryList is a value copy of the entries passed to a Summarize call.
type SessionEntryList struct {
	Entries []session.SessionEntry
}

// Compile-time assertion that the mock satisfies the contract.
var _ session.BranchSummary = (*MockBranchSummary)(nil)

// NewMockBranchSummary creates a mock that always returns summary.
func NewMockBranchSummary(summary string) *MockBranchSummary {
	return &MockBranchSummary{summary: summary}
}

// Summarize records the call and returns the fixed summary.
func (m *MockBranchSummary) Summarize(_ context.Context, entries []session.SessionEntry) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, SessionEntryList{Entries: entries})
	return m.summary, nil
}

// Name returns a stable identifier for the mock.
func (m *MockBranchSummary) Name() string { return "mock-branch-summary" }

// Calls returns the recorded Summarize invocations.
func (m *MockBranchSummary) Calls() []SessionEntryList {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]SessionEntryList, len(m.calls))
	copy(result, m.calls)
	return result
}

// CallCount returns the number of Summarize invocations.
func (m *MockBranchSummary) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}
