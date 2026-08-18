package core

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSystemPromptIncludesSubagentStrategy(t *testing.T) {
	builder := NewDefaultSystemPromptBuilder()
	prompt := builder.Build(context.Background(), SystemPromptOptions{})

	assert.Contains(t, prompt, "Sub-Agent Delegation Strategy")
	assert.Contains(t, prompt, "dispatch_subagent")
	assert.Contains(t, prompt, "researcher")
	assert.Contains(t, prompt, "implementer")
	assert.Contains(t, prompt, "reviewer")
	assert.Contains(t, prompt, "tester")
}

func TestSystemPromptSubagentStrategyContainsRecursionConstraints(t *testing.T) {
	builder := NewDefaultSystemPromptBuilder()
	prompt := builder.Build(context.Background(), SystemPromptOptions{})

	assert.Contains(t, prompt, "Recursion constraints")
	assert.Contains(t, prompt, "depth limit")
	assert.Contains(t, prompt, "tool whitelist")
}

func TestSystemPromptSubagentStrategyContainsAggregationGuidelines(t *testing.T) {
	builder := NewDefaultSystemPromptBuilder()
	prompt := builder.Build(context.Background(), SystemPromptOptions{})

	idx := strings.Index(prompt, "Result aggregation guidelines")
	assert.Greater(t, idx, 0, "prompt should contain result aggregation guidelines")
	rest := prompt[idx:]
	assert.Contains(t, rest, "parallel")
	assert.Contains(t, rest, "synthesize")
}
