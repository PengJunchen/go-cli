//go:build mock

package mock

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/session"
)

// Compile-time assertion that the mock satisfies the branch summary contract.
var _ session.BranchSummary = (*MockBranchSummary)(nil)

func TestMockBranchSummaryReturnsFixedSummary(t *testing.T) {
	m := NewMockBranchSummary("departed recap")
	sessionEntries := []session.SessionEntry{
		{ID: "a", Content: "hello"},
		{ID: "b", Content: "world"},
	}
	sum, err := m.Summarize(t.Context(), sessionEntries)
	require.NoError(t, err)
	assert.Equal(t, "departed recap", sum)
	assert.Equal(t, "mock-branch-summary", m.Name())
	require.Equal(t, 1, m.CallCount())
	require.Len(t, m.Calls(), 1)
	assert.Equal(t, sessionEntries, m.Calls()[0].Entries)
}

func TestMockBranchSummaryRecordsAllCalls(t *testing.T) {
	m := NewMockBranchSummary("x")
	_, err := m.Summarize(t.Context(), nil)
	require.NoError(t, err)
	_, err = m.Summarize(t.Context(), []session.SessionEntry{{ID: "z"}})
	require.NoError(t, err)
	assert.Equal(t, 2, m.CallCount())
	assert.Len(t, m.Calls(), 2)
}
