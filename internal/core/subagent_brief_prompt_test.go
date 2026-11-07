package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResearcherBriefPromptAndOverride(t *testing.T) {
	research := strings.ToLower(resolveSubAgentSystemPrompt(SubagentTask{Role: "researcher"}))
	for _, phrase := range []string{"task brief", "search narrowly", "expand when", "offset and limit", "read complete files when needed", "locations and bounded excerpts", "what was covered", "what remains unverified", "minimal validation", "missing facts as unknown", "do not invent evidence"} {
		assert.Contains(t, research, phrase)
	}
	const custom = "explicit override remains exact"
	assert.Equal(t, custom, resolveSubAgentSystemPrompt(SubagentTask{Role: "researcher", SystemPrompt: custom}))
	for _, role := range []string{"implementer", "reviewer", "tester"} {
		assert.Equal(t, RolePrompt(role), resolveSubAgentSystemPrompt(SubagentTask{Role: role}))
		assert.NotEqual(t, ResearcherPrompt, RolePrompt(role))
	}
	assert.Equal(t, DefaultSubAgentPrompt, resolveSubAgentSystemPrompt(SubagentTask{Role: "unknown"}))
}
