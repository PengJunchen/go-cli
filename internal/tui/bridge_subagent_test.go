package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/pengjunchen/go-cli/internal/core"
)

func TestSubagentEventToAgentEventBasic(t *testing.T) {
	ev := core.AgentEvent{
		Kind:    "message",
		Content: "sub-agent output",
	}
	got := SubagentEventToAgentEvent("task-1", ev)
	assert.Equal(t, ContentTypeSubagent, got.ContentType)
	assert.Contains(t, got.Content, "[subagent:task-1]")
	assert.Contains(t, got.Content, "sub-agent output")
}

func TestSubagentEventToAgentEventIndentsMultiline(t *testing.T) {
	ev := core.AgentEvent{
		Kind:    "message",
		Content: "line1\nline2\nline3",
	}
	got := SubagentEventToAgentEvent("worker", ev)
	lines := strings.Split(got.Content, "\n")
	assert.True(t, len(lines) >= 3)
	// Every line after the prefix line should be indented.
	for _, line := range lines {
		if line != "" {
			assert.True(t, strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "[subagent:"),
				"line should be indented or prefixed: %q", line)
		}
	}
}

func TestSubagentEventToAgentEventPreservesIncremental(t *testing.T) {
	ev := core.AgentEvent{
		Kind:        "message",
		Content:     "partial",
		Incremental: true,
	}
	got := SubagentEventToAgentEvent("task-1", ev)
	assert.True(t, got.Incremental, "Incremental flag should be preserved")
}

func TestSubagentEventToAgentEventEmptyContent(t *testing.T) {
	ev := core.AgentEvent{
		Kind:    "status",
		Content: "",
	}
	got := SubagentEventToAgentEvent("task-1", ev)
	assert.Contains(t, got.Content, "[subagent:task-1]")
}

func TestIndentLines(t *testing.T) {
	assert.Equal(t, "  hello", indentLines("hello", "  "))
	assert.Equal(t, "  a\n  b", indentLines("a\nb", "  "))
	assert.Equal(t, "", indentLines("", "  "))
}

func TestContentTypeSubagentConstant(t *testing.T) {
	assert.Equal(t, "subagent", ContentTypeSubagent)
}

func TestSubagentEventToAgentEventTimestampNotUsed(t *testing.T) {
	// The function should work regardless of timestamp.
	ev := core.AgentEvent{
		Kind:      "message",
		Content:   "test",
		Timestamp: time.Now(),
	}
	got := SubagentEventToAgentEvent("t1", ev)
	assert.NotEmpty(t, got.Content)
}
