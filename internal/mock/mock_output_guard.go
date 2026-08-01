//go:build mock

package mock

import (
	"context"

	"github.com/pengjunchen/go-cli/internal/production"
)

// MockOutputGuard is a test-only production.OutputGuard with a programmatically
// controllable verdict. It records every Check call so tests can assert on the
// number and content of guard evaluations.
type MockOutputGuard struct {
	name       string
	allowed    bool
	reason     string
	severity   production.GuardSeverity
	sanitized  string
	checkCount int
}

// Compile-time assertion that the mock satisfies the guard contract.
var _ production.OutputGuard = (*MockOutputGuard)(nil)

// NewMockOutputGuard creates a mock guard that allows outputs by default.
func NewMockOutputGuard(name string) *MockOutputGuard {
	return &MockOutputGuard{
		name:     name,
		allowed:  true,
		severity: production.GuardHigh,
	}
}

// WithDenied configures the mock to deny outputs with the given reason.
func (m *MockOutputGuard) WithDenied(reason string) *MockOutputGuard {
	m.allowed = false
	m.reason = reason
	return m
}

// WithSanitized sets the sanitized text returned on a denial.
func (m *MockOutputGuard) WithSanitized(text string) *MockOutputGuard {
	m.sanitized = text
	return m
}

// WithSeverity sets the severity returned with the verdict.
func (m *MockOutputGuard) WithSeverity(sev production.GuardSeverity) *MockOutputGuard {
	m.severity = sev
	return m
}

// WithAllowed configures the mock to allow outputs.
func (m *MockOutputGuard) WithAllowed() *MockOutputGuard {
	m.allowed = true
	m.reason = ""
	return m
}

// Check records the call and returns the configured verdict.
func (m *MockOutputGuard) Check(_ context.Context, text string) (*production.GuardResult, error) {
	m.checkCount++
	if m.allowed {
		return &production.GuardResult{
			Allowed:   true,
			Sanitized: text,
			Severity:  production.GuardLow,
		}, nil
	}
	return &production.GuardResult{
		Allowed:   false,
		Reason:    m.reason,
		Sanitized: m.sanitized,
		Severity:  m.severity,
	}, nil
}

// CheckCount returns how many times Check ran.
func (m *MockOutputGuard) CheckCount() int { return m.checkCount }

// Name returns the mock guard name.
func (m *MockOutputGuard) Name() string { return m.name }
