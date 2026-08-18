package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRoleToolsKnownRoles(t *testing.T) {
	cases := []struct {
		role     string
		expected []string
	}{
		{"researcher", []string{"read", "grep", "find", "ls", "web_fetch"}},
		{"implementer", []string{"read", "write", "edit", "bash", "grep", "find"}},
		{"reviewer", []string{"read", "grep", "find", "git_diff", "git_status"}},
		{"tester", []string{"read", "bash", "grep", "go_test"}},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			got := RoleTools(tc.role)
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestRoleToolsUnknownRoleReturnsNil(t *testing.T) {
	assert.Nil(t, RoleTools("unknown"))
	assert.Nil(t, RoleTools(""))
}

func TestRoleToolsReturnsCopy(t *testing.T) {
	original := RoleTools("researcher")
	original[0] = "mutated"
	again := RoleTools("researcher")
	assert.Equal(t, "read", again[0], "RoleTools must return a copy, not the underlying slice")
}

func TestResolveSubAgentToolsExplicitWins(t *testing.T) {
	task := SubagentTask{
		Role:  "researcher",
		Tools: []string{"bash", "write"},
	}
	got := resolveSubAgentTools(task)
	assert.Equal(t, []string{"bash", "write"}, got)
}

func TestResolveSubAgentToolsRoleWhitelist(t *testing.T) {
	task := SubagentTask{Role: "implementer"}
	got := resolveSubAgentTools(task)
	assert.Equal(t, []string{"read", "write", "edit", "bash", "grep", "find"}, got)
}

func TestResolveSubAgentToolsNoRoleNoTools(t *testing.T) {
	task := SubagentTask{}
	got := resolveSubAgentTools(task)
	assert.Nil(t, got)
}
