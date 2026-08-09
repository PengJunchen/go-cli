package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tui"
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

// newTestCmd returns an interactiveCmd and buffer pair for slash dispatch tests.
func newTestCmd() (*interactiveCmd, *bytes.Buffer) {
	var buf bytes.Buffer
	return &interactiveCmd{out: &buf, slashReg: defaultSlashReg}, &buf
}

// ---------------------------------------------------------------------------
// Registry builder
// ---------------------------------------------------------------------------

func TestBuildSlashCommandRegistry(t *testing.T) {
	reg := buildSlashCommandRegistry()

	want := []string{
		"help", "cost", "compact", "clear", "tools", "model", "session",
		"undo", "diff", "plan", "config", "history", "save", "load", "memory",
		"thinking", "theme", "worktree", "revert",
	}
	assert.ElementsMatch(t, want, reg.Names())

	for _, name := range want {
		_, ok := reg.Lookup(name)
		assert.True(t, ok, "command %q should be registered", name)
	}

	// Aliases resolve to their target commands.
	h, ok := reg.Lookup("h")
	require.True(t, ok)
	assert.Equal(t, "help", h.Name())
	c, ok := reg.Lookup("c")
	require.True(t, ok)
	assert.Equal(t, "cost", c.Name())
}

// ---------------------------------------------------------------------------
// Existing commands (dispatched via handleSlashCommand)
// ---------------------------------------------------------------------------

func TestSlashHelp(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "help"}, sc)
	output := buf.String()

	for _, want := range []string{
		"/help", "/cost", "/compact", "/clear", "/tools", "/model", "/session",
		"/undo", "/diff", "/plan", "/config", "/history", "/save", "/load", "/memory",
		"/thinking", "/theme", "/worktree",
		"exit",
	} {
		assert.Contains(t, output, want, "help output should list %s", want)
	}
}

func TestThemeHandler(t *testing.T) {
	// --- List themes (no args) ---
	c, buf := newTestCmd()
	mgr := tui.NewThemeManager()
	sc := &slashContext{out: buf, themeMgr: mgr}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "theme"}, sc)
	output := buf.String()
	assert.Contains(t, output, "dark")
	assert.Contains(t, output, "light")
	assert.Contains(t, output, "monokai")
	assert.Contains(t, output, "solarized")
	assert.Contains(t, output, "(active)") // dark should be marked active

	// --- Switch to a valid theme ---
	buf.Reset()
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "theme", Args: []string{"light"}}, sc)
	assert.Contains(t, buf.String(), "Theme switched to: light")
	assert.Equal(t, "light", mgr.CurrentName())

	// --- Switch to an invalid theme ---
	buf.Reset()
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "theme", Args: []string{"nonexistent"}}, sc)
	output = buf.String()
	assert.Contains(t, output, "unknown theme")
	assert.Contains(t, output, "Available themes:")
	// Current theme should be unchanged.
	assert.Equal(t, "light", mgr.CurrentName())

	// --- Nil themeMgr graceful degradation ---
	buf.Reset()
	scNil := &slashContext{out: buf, themeMgr: nil}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "theme"}, scNil)
	assert.Contains(t, buf.String(), "only available in interactive TUI mode")
}

func TestSlashCost(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	_, err := tracker.Record("gpt-4o", 1000, 500)
	require.NoError(t, err)

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, costTracker: tracker}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "cost"}, sc)
	output := buf.String()

	assert.Contains(t, output, "Total cost:")
	assert.Contains(t, output, "Total calls: 1")
}

// TestCostHandlerWorksWithSnapshot verifies that CostHandler.Handle renders the
// sub-agent cost breakdown using SubagentCostSnapshot without direct field
// access.
func TestCostHandlerWorksWithSnapshot(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	_, err := tracker.RecordSubagent("task-research", "gpt-4o", 2000, 1000)
	require.NoError(t, err)
	_, err = tracker.RecordSubagent("task-implement", "gpt-4o-mini", 500, 200)
	require.NoError(t, err)

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, costTracker: tracker}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "cost"}, sc)
	output := buf.String()

	assert.Contains(t, output, "Sub-agent costs:")
	assert.Contains(t, output, "task-research:")
	assert.Contains(t, output, "task-implement:")
}

func TestSlashCostWithStats(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	_, err := tracker.Record("gpt-4o", 1000, 500)
	require.NoError(t, err)

	stats := production.NewStatsRegistry()
	stats.RecordTurn("test-session")
	stats.RecordToolCall("test-session")
	stats.RecordTokens("test-session", 200, 100)

	c, buf := newTestCmd()
	sc := &slashContext{
		out:           buf,
		costTracker:   tracker,
		statsRegistry: stats,
		sessionID:     "test-session",
	}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "cost"}, sc)
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

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, agent: agent}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "clear"}, sc)

	assert.Contains(t, buf.String(), "cleared")
	assert.Empty(t, agent.Messages(), "history should be empty after /clear")
}

func TestSlashTools(t *testing.T) {
	reg := tools.NewDefaultToolRegistry()
	require.NoError(t, reg.Register(context.Background(), &stubTool{name: "read_file", desc: "reads a file"}))
	require.NoError(t, reg.Register(context.Background(), &stubTool{name: "write_file", desc: "writes a file"}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, toolRegistry: reg}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "tools"}, sc)
	output := buf.String()

	assert.Contains(t, output, "Registered tools (2):")
	assert.Contains(t, output, "read_file")
	assert.Contains(t, output, "reads a file")
	assert.Contains(t, output, "write_file")
	assert.Contains(t, output, "writes a file")
}

func TestSlashModel(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, modelName: "gpt-4o-test"}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "model"}, sc)

	assert.Contains(t, buf.String(), "Current model: gpt-4o-test")
}

func TestSlashUnknown(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
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

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, agent: agent}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "compact"}, sc)

	assert.Contains(t, buf.String(), "Compacted history:")
	assert.Len(t, agent.Messages(), 1, "history should be compacted to 1 message")
}

func TestSlashCompactNoHook(t *testing.T) {
	// Agent without a compaction hook - Compact is a no-op.
	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "hello"},
	}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, agent: agent}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "compact"}, sc)

	assert.Contains(t, buf.String(), "Compacted history:")
	assert.Len(t, agent.Messages(), 1, "history should be unchanged without a hook")
}

func TestSlashSessionNotConfigured(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, sessionHandler: nil}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "session"}, sc)

	assert.Contains(t, buf.String(), "Session tree not configured")
}

func TestSlashSessionWithHandler(t *testing.T) {
	tree := session.NewDefaultSessionTree()
	store := session.NewDefaultSessionStore()
	handler := session.NewSessionSlashHandler(tree, store)

	// Add an entry to the tree so /session (-> /tree) has something to show.
	require.NoError(t, tree.Append(context.Background(), &session.SessionEntry{
		ID:      "e1",
		Type:    session.EntryTypeUser,
		Content: "hello world",
	}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, sessionHandler: handler}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "session"}, sc)
	output := buf.String()

	assert.Contains(t, output, "Session tree")
	assert.Contains(t, output, "hello world")
}

// ---------------------------------------------------------------------------
// Aliases
// ---------------------------------------------------------------------------

func TestSlashAliasHelp(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "h"}, sc)

	assert.Contains(t, buf.String(), "/help")
	assert.Contains(t, buf.String(), "Available commands:")
}

func TestSlashAliasCost(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	_, err := tracker.Record("gpt-4o", 10, 5)
	require.NoError(t, err)

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, costTracker: tracker}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "c"}, sc)

	assert.Contains(t, buf.String(), "Total cost:")
}

// ---------------------------------------------------------------------------
// New commands
// ---------------------------------------------------------------------------

func TestSlashUndoRestoresMostRecentCheckpoint(t *testing.T) {
	ft := tools.NewFileTracker()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("original\n"), 0o600))

	_, err := ft.Backup(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("modified\n"), 0o600))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, fileTracker: ft}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "undo"}, sc)

	assert.Contains(t, buf.String(), "Restored")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "original\n", string(data), "/undo should restore the original content")
}

func TestSlashUndoNoCheckpoints(t *testing.T) {
	ft := tools.NewFileTracker()
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, fileTracker: ft}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "undo"}, sc)

	assert.Contains(t, buf.String(), "No checkpoints to undo")
}

func TestSlashUndoNotConfigured(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "undo"}, sc)

	assert.Contains(t, buf.String(), "File tracking not configured")
}

func TestSlashDiffShowsUnifiedDiff(t *testing.T) {
	ft := tools.NewFileTracker()
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600))

	_, err := ft.Backup(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("alpha\ngamma\n"), 0o600))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, fileTracker: ft, diffGenerator: tools.NewUnifiedDiffGenerator(0, false)}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "diff"}, sc)
	output := buf.String()

	assert.Contains(t, output, "--- a/")
	assert.Contains(t, output, "+++ b/")
	assert.Contains(t, output, "-beta")
	assert.Contains(t, output, "+gamma")
}

func TestSlashDiffNoCheckpoints(t *testing.T) {
	ft := tools.NewFileTracker()
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, fileTracker: ft, diffGenerator: tools.NewUnifiedDiffGenerator(0, false)}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "diff"}, sc)

	assert.Contains(t, buf.String(), "No file changes recorded")
}

func TestSlashDiffNotConfigured(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "diff"}, sc)

	assert.Contains(t, buf.String(), "File tracking not configured")
}

func TestSlashPlanEnterAndExit(t *testing.T) {
	ctrl := core.NewDefaultPlanModeController()
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, planCtrl: ctrl}

	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "plan", Args: []string{"enter", "investigate"}}, sc)
	assert.Contains(t, buf.String(), "Entered plan mode")
	assert.True(t, ctrl.IsActive())

	buf.Reset()
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "plan", Args: []string{"exit"}}, sc)
	assert.Contains(t, buf.String(), "Exited plan mode")
	assert.False(t, ctrl.IsActive())
}

func TestSlashPlanToggle(t *testing.T) {
	ctrl := core.NewDefaultPlanModeController()
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, planCtrl: ctrl}

	// No args when inactive -> enter.
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "plan"}, sc)
	assert.Contains(t, buf.String(), "Entered plan mode")
	assert.True(t, ctrl.IsActive())

	// No args when active -> exit.
	buf.Reset()
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "plan"}, sc)
	assert.Contains(t, buf.String(), "Exited plan mode")
	assert.False(t, ctrl.IsActive())
}

func TestSlashPlanNotConfigured(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "plan"}, sc)

	assert.Contains(t, buf.String(), "Plan mode not configured")
}

func TestSlashConfigShowsSummary(t *testing.T) {
	cfg := &config.Config{}
	cfg.Provider.Name = "openai"
	cfg.Model.Name = "gpt-4o"
	cfg.Approval.Mode = "always"
	cfg.Session.StorePath = "/tmp/s.jsonl"

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, config: cfg}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "config"}, sc)
	output := buf.String()

	assert.Contains(t, output, "Configuration summary:")
	assert.Contains(t, output, "openai")
	assert.Contains(t, output, "gpt-4o")
	assert.Contains(t, output, "always")
	assert.Contains(t, output, "/tmp/s.jsonl")
}

func TestSlashConfigNotAvailable(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "config"}, sc)

	assert.Contains(t, buf.String(), "Config not available")
}

func TestSlashHistorySummary(t *testing.T) {
	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, agent: agent}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "history"}, sc)
	output := buf.String()

	assert.Contains(t, output, "Conversation history (2 messages)")
	assert.Contains(t, output, "hello")
	assert.Contains(t, output, "hi there")
}

func TestSlashHistoryEmpty(t *testing.T) {
	agent := core.NewAgentImpl("test", stubLoop{})

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, agent: agent}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "history"}, sc)

	assert.Contains(t, buf.String(), "No conversation history")
}

func TestSlashHistoryNotConfigured(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "history"}, sc)

	assert.Contains(t, buf.String(), "Agent not configured")
}

func TestSlashSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	store := session.NewJSONLSessionStore(path)
	require.NoError(t, store.Open(context.Background()))
	defer func() { _ = store.Close() }() //nolint:errcheck

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, sessionStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "save"}, sc)

	assert.Contains(t, buf.String(), "Session saved")
}

func TestSlashSaveNotConfigured(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "save"}, sc)

	assert.Contains(t, buf.String(), "Session store not configured")
}

func TestSlashLoadSummarizesStoredEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	store := session.NewJSONLSessionStore(path)
	require.NoError(t, store.Open(context.Background()))
	defer func() { _ = store.Close() }() //nolint:errcheck

	require.NoError(t, store.Append(context.Background(), &session.SessionEntry{
		ID: "e1", Type: session.EntryTypeUser, Content: "hello", Timestamp: time.Now(),
	}))
	require.NoError(t, store.Append(context.Background(), &session.SessionEntry{
		ID: "e2", Type: session.EntryTypeAssistant, Content: "hi", Timestamp: time.Now().Add(time.Second),
	}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, sessionStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "load"}, sc)
	output := buf.String()

	assert.Contains(t, output, "Loaded 2 session entries")
	assert.Contains(t, output, "hello")
}

func TestSlashLoadEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	store := session.NewJSONLSessionStore(path)
	require.NoError(t, store.Open(context.Background()))
	defer func() { _ = store.Close() }() //nolint:errcheck

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, sessionStore: store}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "load"}, sc)

	assert.Contains(t, buf.String(), "No stored session entries")
}

func TestSlashLoadNotConfigured(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "load"}, sc)

	assert.Contains(t, buf.String(), "Session store not configured")
}

// ---------------------------------------------------------------------------
// Table-driven dispatch
// ---------------------------------------------------------------------------

// TestSlashCommandsTableDriven exercises handleSlashCommand dispatch for
// multiple commands in a single table-driven test.
func TestSlashCommandsTableDriven(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	_, err := tracker.Record("gpt-4o", 100, 50)
	require.NoError(t, err)

	reg := tools.NewDefaultToolRegistry()
	require.NoError(t, reg.Register(context.Background(), &stubTool{name: "t1", desc: "test tool"}))

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
			c := &interactiveCmd{out: &buf, slashReg: defaultSlashReg}
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
}
