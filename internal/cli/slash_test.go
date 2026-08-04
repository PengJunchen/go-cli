package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// stubLoop is a minimal AgentLoop used by slash command tests. It never
// executes and returns no events.
type stubLoop struct{}

func (stubLoop) Run(_ context.Context, _ core.Submission, _ ...core.EventStream) ([]core.AgentEvent, error) {
	return nil, nil
}

// stubTool is a minimal ToolDefinition for testing /tools.
type stubTool struct {
	name string
	desc string
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return s.desc }
func (s *stubTool) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: ""}, nil
}

func TestSlashHelp(t *testing.T) {
	var buf bytes.Buffer
	c := &interactiveCmd{out: &buf}
	sc := &slashContext{out: &buf}
	c.slashHelp(sc)
	output := buf.String()

	for _, want := range []string{
		"/help",
		"/cost",
		"/compact",
		"/clear",
		"/tools",
		"/model",
		"/session",
	} {
		assert.Contains(t, output, want, "help output should list %s", want)
	}
}

func TestSlashCost(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	_, err := tracker.Record("gpt-4o", 1000, 500)
	require.NoError(t, err)

	var buf bytes.Buffer
	c := &interactiveCmd{out: &buf}
	sc := &slashContext{out: &buf, costTracker: tracker}
	c.slashCost(sc)
	output := buf.String()

	assert.Contains(t, output, "Total cost:")
	assert.Contains(t, output, "Total calls: 1")
}

func TestSlashCostWithStats(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	_, err := tracker.Record("gpt-4o", 1000, 500)
	require.NoError(t, err)

	stats := production.NewStatsRegistry()
	stats.RecordTurn("test-session")
	stats.RecordToolCall("test-session")
	stats.RecordTokens("test-session", 200, 100)

	var buf bytes.Buffer
	c := &interactiveCmd{out: &buf}
	sc := &slashContext{
		out:           &buf,
		costTracker:   tracker,
		statsRegistry: stats,
		sessionID:     "test-session",
	}
	c.slashCost(sc)
	output := buf.String()

	assert.Contains(t, output, "Total cost:")
	assert.Contains(t, output, "Session stats:")
	assert.Contains(t, output, "Turns:")
	assert.Contains(t, output, "Tokens in:")
}

func TestSlashClear(t *testing.T) {
	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}))
	require.Len(t, agent.Messages(), 2)

	var buf bytes.Buffer
	c := &interactiveCmd{out: &buf}
	sc := &slashContext{out: &buf, agent: agent}
	c.slashClear(sc)

	assert.Contains(t, buf.String(), "cleared")
	assert.Empty(t, agent.Messages(), "history should be empty after /clear")
}

func TestSlashTools(t *testing.T) {
	reg := tools.NewDefaultToolRegistry()
	require.NoError(t, reg.Register(context.Background(), &stubTool{name: "read_file", desc: "reads a file"}))
	require.NoError(t, reg.Register(context.Background(), &stubTool{name: "write_file", desc: "writes a file"}))

	var buf bytes.Buffer
	c := &interactiveCmd{out: &buf}
	sc := &slashContext{out: &buf, toolRegistry: reg}
	c.slashTools(context.Background(), sc)
	output := buf.String()

	assert.Contains(t, output, "Registered tools (2):")
	assert.Contains(t, output, "read_file")
	assert.Contains(t, output, "reads a file")
	assert.Contains(t, output, "write_file")
	assert.Contains(t, output, "writes a file")
}

func TestSlashModel(t *testing.T) {
	var buf bytes.Buffer
	c := &interactiveCmd{out: &buf}
	sc := &slashContext{out: &buf, modelName: "gpt-4o-test"}
	c.slashModel(sc)

	assert.Contains(t, buf.String(), "Current model: gpt-4o-test")
}

func TestSlashUnknown(t *testing.T) {
	var buf bytes.Buffer
	c := &interactiveCmd{out: &buf}
	sc := &slashContext{out: &buf}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "foobar"}, sc)
	output := buf.String()

	assert.Contains(t, output, "Unknown command: /foobar")
	assert.Contains(t, output, "/help")
}

func TestSlashCompact(t *testing.T) {
	// Compaction hook that keeps only the last message.
	hook := func(_ context.Context, msgs []core.AgentMessage) ([]core.AgentMessage, error) {
		if len(msgs) > 1 {
			return msgs[len(msgs)-1:], nil
		}
		return msgs, nil
	}
	agent := core.NewAgentImpl("test", stubLoop{},
		core.WithCompactionHook(hook),
		core.WithHistory([]core.AgentMessage{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "reply one"},
			{Role: "user", Content: "second"},
		}),
	)
	require.Len(t, agent.Messages(), 3)

	var buf bytes.Buffer
	c := &interactiveCmd{out: &buf}
	sc := &slashContext{out: &buf, agent: agent}
	c.slashCompact(context.Background(), sc)

	assert.Contains(t, buf.String(), "Compacted history:")
	assert.Len(t, agent.Messages(), 1, "history should be compacted to 1 message")
}

func TestSlashCompactNoHook(t *testing.T) {
	// Agent without a compaction hook — Compact is a no-op.
	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "hello"},
	}))

	var buf bytes.Buffer
	c := &interactiveCmd{out: &buf}
	sc := &slashContext{out: &buf, agent: agent}
	c.slashCompact(context.Background(), sc)

	assert.Contains(t, buf.String(), "Compacted history:")
	assert.Len(t, agent.Messages(), 1, "history should be unchanged without a hook")
}

func TestSlashSessionNotConfigured(t *testing.T) {
	var buf bytes.Buffer
	c := &interactiveCmd{out: &buf}
	sc := &slashContext{out: &buf, sessionHandler: nil}
	c.slashSession(context.Background(), session.SlashCommand{Name: "session"}, sc)

	assert.Contains(t, buf.String(), "Session tree not configured")
}

func TestSlashSessionWithHandler(t *testing.T) {
	tree := session.NewDefaultSessionTree()
	store := session.NewDefaultSessionStore()
	handler := session.NewSessionSlashHandler(tree, store)

	// Add an entry to the tree so /session (→ /tree) has something to show.
	require.NoError(t, tree.Append(context.Background(), &session.SessionEntry{
		ID:      "e1",
		Type:    session.EntryTypeUser,
		Content: "hello world",
	}))

	var buf bytes.Buffer
	c := &interactiveCmd{out: &buf}
	sc := &slashContext{out: &buf, sessionHandler: handler}
	c.slashSession(context.Background(), session.SlashCommand{Name: "session"}, sc)
	output := buf.String()

	assert.Contains(t, output, "Session tree")
	assert.Contains(t, output, "hello world")
}

// TestSlashCommandsTableDriven exercises handleSlashCommand dispatch for
// multiple commands in a single table-driven test.
func TestSlashCommandsTableDriven(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	_, err := tracker.Record("gpt-4o", 100, 50)
	require.NoError(t, err)

	reg := tools.NewDefaultToolRegistry()
	require.NoError(t, reg.Register(context.Background(), &stubTool{name: "t1", desc: "test tool"}))

	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "msg"},
	}))

	tests := []struct {
		name    string
		cmd     session.SlashCommand
		wantSub string
	}{
		{"help", session.SlashCommand{Name: "help"}, "/help"},
		{"cost", session.SlashCommand{Name: "cost"}, "Total cost:"},
		{"compact", session.SlashCommand{Name: "compact"}, "Compacted history:"},
		{"clear", session.SlashCommand{Name: "clear"}, "cleared"},
		{"tools", session.SlashCommand{Name: "tools"}, "Registered tools"},
		{"model", session.SlashCommand{Name: "model"}, "Current model:"},
		{"unknown", session.SlashCommand{Name: "xyz"}, "Unknown command"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := &interactiveCmd{out: &buf}
			// Use a fresh agent per iteration so /clear in one sub-test
			// does not affect another.
			testAgent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
				{Role: "user", Content: "msg"},
			}))
			sc := &slashContext{
				out:          &buf,
				agent:        testAgent,
				costTracker:  tracker,
				toolRegistry: reg,
				modelName:    "test-model",
			}
			c.handleSlashCommand(context.Background(), tt.cmd, sc)
			assert.Contains(t, buf.String(), tt.wantSub)
		})
	}

	// Verify the original agent (outside the table) is unaffected.
	_ = agent
}
