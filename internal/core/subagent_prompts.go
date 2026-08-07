package core //exempt:scan009

// Role template constants used to seed a sub-agent's system prompt when a
// caller delegates a task with a named role but no explicit system_prompt.

const (
	// DefaultSubAgentPrompt is used when no system_prompt is provided.
	DefaultSubAgentPrompt = `You are a focused sub-agent. Execute the given task precisely and return the result.`

	// ResearcherPrompt for research-oriented sub-agents.
	ResearcherPrompt = `You are a research sub-agent. Your role is to investigate and gather information. Focus on: thorough exploration, factual accuracy, and comprehensive coverage. Return a structured summary of findings.`

	// ImplementerPrompt for code implementation sub-agents.
	ImplementerPrompt = `You are an implementation sub-agent. Your role is to write code. Focus on: clean implementation, proper error handling, and adherence to existing patterns. Return the code changes made and a brief explanation.`

	// ReviewerPrompt for code review sub-agents.
	ReviewerPrompt = `You are a code review sub-agent. Your role is to review code changes. Focus on: correctness, security, performance, and maintainability. Return a structured review with issues found and severity ratings.`

	// TesterPrompt for test-writing sub-agents.
	TesterPrompt = `You are a test-writing sub-agent. Your role is to write tests. Focus on: edge cases, error paths, and coverage. Return the test code and coverage notes.`
)

// SubAgentRoles lists the recognized role names a caller may delegate with. It
// mirrors the role template set so callers (and tool descriptions) can
// enumerate the available roles.
var SubAgentRoles = []string{"researcher", "implementer", "reviewer", "tester"}

// RoleToolWhitelist maps each role to the set of tool names it is allowed to
// use. When a caller does not specify Tools explicitly, the dispatcher applies
// the role whitelist so each sub-agent operates within its role's scope.
var RoleToolWhitelist = map[string][]string{
	"researcher":  {"read", "grep", "find", "ls", "web_fetch"},
	"implementer": {"read", "write", "edit", "bash", "grep", "find"},
	"reviewer":    {"read", "grep", "find", "git_diff", "git_status"},
	"tester":      {"read", "bash", "grep", "go_test"},
}

// RoleTools returns the tool whitelist for the given role. It returns nil for
// unknown roles, signaling the caller to leave the tool set unrestricted.
func RoleTools(role string) []string {
	if tools, ok := RoleToolWhitelist[role]; ok {
		return append([]string(nil), tools...)
	}
	return nil
}

// RolePrompt returns the system prompt template for the given role name. It
// returns an empty string for unknown roles so callers can fall back to the
// default prompt. Recognized roles: researcher, implementer, reviewer, tester.
func RolePrompt(role string) string {
	switch role {
	case "researcher":
		return ResearcherPrompt
	case "implementer":
		return ImplementerPrompt
	case "reviewer":
		return ReviewerPrompt
	case "tester":
		return TesterPrompt
	}
	return ""
}

// resolveSubAgentSystemPrompt picks the system prompt for a delegated task. An
// explicit SystemPrompt wins; otherwise a recognized Role selects its template;
// otherwise the DefaultSubAgentPrompt is used. This guarantees every sub-agent
// receives a non-empty system prompt.
func resolveSubAgentSystemPrompt(task SubagentTask) string {
	if task.SystemPrompt != "" {
		return task.SystemPrompt
	}
	if p := RolePrompt(task.Role); p != "" {
		return p
	}
	return DefaultSubAgentPrompt
}

// resolveSubAgentTools picks the tool set for a delegated task. Explicit Tools
// win; otherwise a recognized Role selects its whitelist; otherwise nil is
// returned (no restriction). This guarantees a sub-agent operates within its
// role's scope when the caller does not specify tools.
func resolveSubAgentTools(task SubagentTask) []string {
	if len(task.Tools) > 0 {
		return task.Tools
	}
	return RoleTools(task.Role)
}
