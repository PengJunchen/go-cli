package core

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSystemPromptMemoryEmptyProducesNoBlock(t *testing.T) {
	builder := NewDefaultSystemPromptBuilder()
	prompt := builder.Build(context.Background(), SystemPromptOptions{})

	assert.NotContains(t, prompt, "<memory>")
}

func TestSystemPromptMemoryGroupedByCategory(t *testing.T) {
	builder := NewDefaultSystemPromptBuilder()
	prompt := builder.Build(context.Background(), SystemPromptOptions{
		Memories: []MemoryEntry{
			{ID: "1", Content: "prefer dark mode", Category: "preference"},
			{ID: "2", Content: "the API uses REST", Category: "fact"},
			{ID: "3", Content: "use tabs not spaces", Category: "preference"},
			{ID: "4", Content: "adopt clean architecture", Category: "decision"},
		},
	})

	assert.Contains(t, prompt, "<memory>")
	assert.Contains(t, prompt, "## User Preferences")
	assert.Contains(t, prompt, "- prefer dark mode")
	assert.Contains(t, prompt, "- use tabs not spaces")
	assert.Contains(t, prompt, "## Facts")
	assert.Contains(t, prompt, "- the API uses REST")
	assert.Contains(t, prompt, "## Decisions")
	assert.Contains(t, prompt, "- adopt clean architecture")
	assert.Contains(t, prompt, "</memory>")

	// Project Conventions has no entries and should be skipped.
	assert.NotContains(t, prompt, "## Project Conventions")
}

func TestSystemPromptMemoryBetweenSkillsAndCurrentDate(t *testing.T) {
	builder := NewDefaultSystemPromptBuilder()
	prompt := builder.Build(context.Background(), SystemPromptOptions{
		Skills: []SkillInfo{
			{Name: "my-skill", Description: "does things", Category: "dev"},
		},
		Memories: []MemoryEntry{
			{ID: "1", Content: "remember this", Category: "fact"},
		},
	})

	skillsIdx := strings.Index(prompt, "</skills>")
	memoryIdx := strings.Index(prompt, "<memory>")
	dateIdx := strings.Index(prompt, "Current date:")

	assert.Greater(t, skillsIdx, -1, "prompt should contain skills section")
	assert.Greater(t, memoryIdx, -1, "prompt should contain memory section")
	assert.Greater(t, dateIdx, -1, "prompt should contain current date section")

	assert.Greater(t, memoryIdx, skillsIdx, "<memory> should appear after skills")
	assert.Less(t, memoryIdx, dateIdx, "<memory> should appear before Current date")
}

func TestSystemPromptMemoryMixedCategories(t *testing.T) {
	builder := NewDefaultSystemPromptBuilder()
	prompt := builder.Build(context.Background(), SystemPromptOptions{
		Memories: []MemoryEntry{
			{ID: "1", Content: "prefer verbose output", Category: "preference"},
			{ID: "2", Content: "use conventional commits", Category: "convention"},
			{ID: "3", Content: "project requires Go 1.24", Category: "fact"},
			{ID: "4", Content: "migrate to postgres", Category: "decision"},
			{ID: "5", Content: "random note", Category: "misc"},
			{ID: "6", Content: "another random note", Category: "unknown"},
		},
	})

	assert.Contains(t, prompt, "## User Preferences")
	assert.Contains(t, prompt, "- prefer verbose output")
	assert.Contains(t, prompt, "## Project Conventions")
	assert.Contains(t, prompt, "- use conventional commits")
	assert.Contains(t, prompt, "## Facts")
	assert.Contains(t, prompt, "- project requires Go 1.24")
	assert.Contains(t, prompt, "## Decisions")
	assert.Contains(t, prompt, "- migrate to postgres")

	// Unknown categories are grouped under "## Other".
	assert.Contains(t, prompt, "## Other")
	assert.Contains(t, prompt, "- random note")
	assert.Contains(t, prompt, "- another random note")

	// Verify the category ordering: preference, convention, fact, decision, other.
	prefIdx := strings.Index(prompt, "## User Preferences")
	convIdx := strings.Index(prompt, "## Project Conventions")
	factIdx := strings.Index(prompt, "## Facts")
	decIdx := strings.Index(prompt, "## Decisions")
	otherIdx := strings.Index(prompt, "## Other")

	assert.Less(t, prefIdx, convIdx, "preferences before conventions")
	assert.Less(t, convIdx, factIdx, "conventions before facts")
	assert.Less(t, factIdx, decIdx, "facts before decisions")
	assert.Less(t, decIdx, otherIdx, "decisions before other")
}
