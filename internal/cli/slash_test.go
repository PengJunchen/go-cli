package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
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
		"thinking", "theme", "worktree", "revert", "retry", "edit", "context",
		"mcp",
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
		"/thinking", "/theme", "/worktree", "/retry", "/edit",
		"exit",
	} {
		assert.Contains(t, output, want, "help output should list %s", want)
	}
	for _, want := range []string{
		"Capability boundaries",
		"Memory: user-managed with /memory only",
		"no automatic extraction or prompt injection",
		"Goal, Task, and Todo: stored only in memory for this process",
		"Session storage: without session.store_path, /session is unsupported.",
		"Durable mode exposes /session tree, branches, pending, and explicit one-action reconciliation; resume uses --resume-session <physical-id> at startup.",
		"Durable /session resume, fork, clone, and switch are unsupported.",
		"Legacy mutable mode supports /session tree, branches, resume, fork, clone, and switch with a wired mutable tree; resume listing needs a session store.",
		"Session snapshot and schema migration are unavailable.",
		"Image reading and Web Agent UI: unavailable",
		"Web Search requires a configured provider",
		"Remote TUI is unavailable",
		"private go-cli-acp-json/1 profile",
		"official ACP and ask approvals are unavailable",
		"Hard cost budgets are unsupported",
		"a displayed $0 is not proof of zero spend",
		"Plan Mode changes only through the user's /plan enter|exit commands",
		"Themes cover a renderer subset",
		"Diffs are unified; ASCII mode is unsupported",
		"NO_COLOR only disables color",
		"MCP server configuration hot reload is unsupported",
		"Darwin arm64",
		"Linux and Windows are unverified",
		"Docker target is linux/amd64; runtime support is unverified",
	} {
		assert.Contains(t, output, want, "help output must state boundary: %s", want)
	}
	assert.NotContains(t, output, "Fork, branch switch, snapshot, and migration are unsupported",
		"legacy mutable handlers support fork and switch, so the boundary must distinguish durable mode")
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

func TestSlashCostShowsKnownSubtotalsAndUnknownAttemptCounts(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	result := llm.ProviderAttemptResult{
		OperationID: "op-cost-report",
		AttemptID:   "attempt-cost-report",
		Identity: llm.AttemptIdentity{
			ProviderID: "openai",
			ModelID:    "gpt-4o",
			ProtocolID: llm.ProtocolOpenAICompatible,
			OriginKind: llm.OriginResolverDefaultEndpoint,
			Candidate:  "candidate-cost-report",
		},
		Outcome: llm.AttemptTransportFailure,
		Failure: llm.FailureTransientProvider,
		Usage:   llm.UnknownUsageEvidence(),
	}
	_, err := tracker.RecordAttempt(result)
	require.NoError(t, err)

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, costTracker: tracker}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "cost"}, sc)
	output := buf.String()

	assert.Contains(t, output, "Known cost subtotal: $0.0000")
	assert.Contains(t, output, "Provider attempts: 1")
	assert.Contains(t, output, "Unknown usage attempts: 1")
	assert.Contains(t, output, "Unknown cost attempts: 1")
	assert.Contains(t, output, `provider="openai" model="gpt-4o" usage=unknown`)
	assert.Contains(t, output, "cost=unknown price_source=unknown")
	assert.Contains(t, output, `attempt_id="attempt-cost-report" outcome=transport_failure amount=unknown`)
	assert.NotContains(t, output, "Total cost: $0.0000", "unknown cost must not be represented as a complete zero total")
}

func TestSlashCostShowsActualProviderModelAndEvidenceSources(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	result := llm.ProviderAttemptResult{
		OperationID: "op-cost-source",
		AttemptID:   "attempt-cost-source",
		Identity: llm.AttemptIdentity{
			ProviderID: "actual-provider",
			ModelID:    "gpt-4o",
			ProtocolID: llm.ProtocolOpenAICompatible,
			OriginKind: llm.OriginResolverDefaultEndpoint,
			Candidate:  "candidate-private-marker",
		},
		Outcome: llm.AttemptSuccess,
		Usage: llm.UsageEvidence{
			State:  llm.UsageKnown,
			Usage:  &llm.Usage{InputTokens: 100, OutputTokens: 10, TotalTokens: 110},
			Source: llm.UsageProviderResponse,
		},
	}
	_, err := tracker.RecordAttempt(result)
	require.NoError(t, err)
	reportedCost := 0.03
	providerReported := result
	providerReported.OperationID = "op-cost-reported"
	providerReported.AttemptID = "attempt-cost-reported"
	providerReported.Identity.ProviderID = "reported-provider"
	providerReported.Identity.ModelID = "unpriced-model"
	providerReported.Identity.Candidate = "candidate-reported"
	providerReported.Usage = llm.UnknownUsageEvidence()
	providerReported.ProviderReportedCostUSD = &reportedCost
	_, err = tracker.RecordAttempt(providerReported)
	require.NoError(t, err)

	_, buf := newTestCmd()
	sc := &slashContext{out: buf, costTracker: tracker}
	_, err = (&CostHandler{}).Handle(context.Background(), nil, sc)
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, `provider="actual-provider" model="gpt-4o" usage=reported (100 in / 10 out)`)
	assert.Contains(t, output, "cost=estimated")
	assert.Contains(t, output, "price_source=default_tier")
	assert.Contains(t, output, `provider="reported-provider" model="unpriced-model" usage=unknown cost=reported price_source=provider`)
	assert.NotContains(t, output, "candidate-private-marker")
	assert.NotContains(t, output, "candidate-reported")
	assert.NotContains(t, output, "https://")
}

func TestSlashCostCombinesLegacyAndAttemptCostsWithoutDoubleCounting(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	legacyCost, err := tracker.Record("gpt-4o", 1000, 500)
	require.NoError(t, err)
	_, err = tracker.RecordAttempt(llm.ProviderAttemptResult{
		OperationID: "op-mixed-cost-report",
		AttemptID:   "attempt-mixed-cost-report",
		Identity: llm.AttemptIdentity{
			ProviderID: "actual-provider",
			ModelID:    "gpt-4o",
			ProtocolID: llm.ProtocolOpenAICompatible,
			OriginKind: llm.OriginResolverDefaultEndpoint,
			Candidate:  "candidate-private",
		},
		Outcome: llm.AttemptSuccess,
		Usage: llm.UsageEvidence{
			State:  llm.UsageKnown,
			Usage:  &llm.Usage{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
			Source: llm.UsageProviderResponse,
		},
	})
	require.NoError(t, err)

	_, buf := newTestCmd()
	_, err = (&CostHandler{}).Handle(context.Background(), nil, &slashContext{out: buf, costTracker: tracker})
	require.NoError(t, err)

	output := buf.String()
	assert.InDelta(t, 0.015, tracker.Total(), 0.000001)
	assert.InDelta(t, 0.0075, legacyCost, 0.000001)
	assert.Contains(t, output, "Known cost subtotal: $0.0150",
		"the aggregate must include both the legacy record and the attempt once each")
	assert.Contains(t, output, "Attempt known cost subtotal: $0.0075")
	assert.Contains(t, output, "Legacy cost records: 1; known subtotal: $0.0075")
	assert.Contains(t, output, "provider/model and usage provenance not retained; price_source=default_tier")
	assert.Contains(t, output, `attempt_id="attempt-mixed-cost-report" outcome=success amount=$0.0075`)
	assert.NotContains(t, output, "candidate-private")
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

func TestModelSwitch_List(t *testing.T) {
	models := []llm.ModelInfo{
		{Name: "gpt-4o", Description: "flagship model"},
		{Name: "gpt-4o-mini", Description: "fast and affordable"},
		{Name: "o1-preview", Description: "reasoning model"},
	}
	sel := llm.NewDefaultModelSelector(&stubModel{}, nil).
		WithModelNames("openai", "gpt-4o", "", "").
		WithModelLister(func() []llm.ModelInfo { return models })

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, modelSelector: sel}
	c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "model"}, sc)

	out := buf.String()
	assert.Contains(t, out, "Available models:")
	assert.Contains(t, out, "* gpt-4o - flagship model")
	assert.Contains(t, out, "  gpt-4o-mini - fast and affordable")
	assert.Contains(t, out, "  o1-preview - reasoning model")
	assert.Contains(t, out, "Current: gpt-4o")
}

func TestModelSwitch_Switch(t *testing.T) {
	var switched llm.BaseChatModel
	sel := llm.NewDefaultModelSelector(&stubModel{}, nil).
		WithModelNames("openai", "gpt-4o", "", "").
		WithModelBuilder(func(_ context.Context, name string) (llm.BaseChatModel, func(), error) {
			return &stubModel{}, nil, nil
		}).
		WithModelSwitchCallback(func(m llm.BaseChatModel) {
			switched = m
		}).
		WithModelLister(func() []llm.ModelInfo {
			return []llm.ModelInfo{
				{Name: "gpt-4o"},
				{Name: "gpt-4o-mini"},
			}
		})

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, modelSelector: sel}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "model",
		Args: []string{"gpt-4o-mini"},
	}, sc)

	assert.Contains(t, buf.String(), "Switched to model: gpt-4o-mini")
	assert.Equal(t, "gpt-4o-mini", sel.PrimaryModelName())
	assert.NotNil(t, switched, "switch callback should have been called")
}

func TestModelSwitch_InvalidName(t *testing.T) {
	sel := llm.NewDefaultModelSelector(&stubModel{}, nil).
		WithModelNames("openai", "gpt-4o", "", "").
		WithModelBuilder(func(_ context.Context, name string) (llm.BaseChatModel, func(), error) {
			return nil, nil, errors.New("unknown model: " + name)
		})

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, modelSelector: sel}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "model",
		Args: []string{"nonexistent-model"},
	}, sc)

	out := buf.String()
	assert.Contains(t, out, "Error:")
	assert.Contains(t, out, "switch model")
	assert.Contains(t, out, "nonexistent-model")
	assert.Equal(t, "gpt-4o", sel.PrimaryModelName(), "model name should not change on error")
}

func TestModelSwitch_NoSelector(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf, modelName: "test-model"}
	c.handleSlashCommand(context.Background(), session.SlashCommand{
		Name: "model",
		Args: []string{"some-model"},
	}, sc)

	assert.Contains(t, buf.String(), "Error:")
	assert.Contains(t, buf.String(), "model switching not available")
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

	assert.Contains(t, buf.String(), "Unsupported:")
	assert.Contains(t, buf.String(), "session.store_path")
	assert.NotContains(t, buf.String(), "Session tree not configured")
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

// ---------------------------------------------------------------------------
// /retry command
// ---------------------------------------------------------------------------

func TestRetry_RemovesAssistantMessage(t *testing.T) {
	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second question"},
		{Role: "assistant", Content: "second answer"},
	}))
	require.Len(t, agent.Messages(), 4)

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, agent: agent}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "retry"}, sc)

	assert.Contains(t, buf.String(), "Retrying last message...")
	assert.Equal(t, "second question", pendingInput)

	// History should be truncated to remove the last user+assistant pair.
	msgs := agent.Messages()
	assert.Len(t, msgs, 2, "history should have first user+assistant pair only")
	assert.Equal(t, "first question", msgs[0].Content)
	assert.Equal(t, "first answer", msgs[1].Content)
}

func TestRetry_ResubmitsUserMessage(t *testing.T) {
	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "what is 2+2?"},
		{Role: "assistant", Content: "4"},
	}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, agent: agent}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "retry"}, sc)

	assert.Equal(t, "what is 2+2?", pendingInput, "pendingInput should be the last user message")
	assert.Empty(t, agent.Messages(), "history should be empty after removing the only user+assistant pair")
}

func TestRetry_NoAssistantMessage(t *testing.T) {
	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "hello"},
	}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, agent: agent}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "retry"}, sc)

	assert.Empty(t, pendingInput, "pendingInput should be empty when no assistant message")
	assert.Contains(t, buf.String(), "No assistant message to retry.")
	assert.Len(t, agent.Messages(), 1, "history should be unchanged")
}

func TestRetry_EmptyHistory(t *testing.T) {
	agent := core.NewAgentImpl("test", stubLoop{})

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, agent: agent}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "retry"}, sc)

	assert.Empty(t, pendingInput)
	assert.Contains(t, buf.String(), "No assistant message to retry.")
}

func TestRetry_AgentNotConfigured(t *testing.T) {
	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "retry"}, sc)

	assert.Empty(t, pendingInput)
	assert.Contains(t, buf.String(), "Agent not configured")
}

func TestRetry_RemovesTrailingToolMessages(t *testing.T) {
	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "read a file"},
		{Role: "assistant", Content: "let me check"},
		{Role: "tool", Content: "file content", ToolCallID: "tc1", ToolName: "read_file"},
	}))
	require.Len(t, agent.Messages(), 3)

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, agent: agent}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "retry"}, sc)

	assert.Equal(t, "read a file", pendingInput)
	assert.Empty(t, agent.Messages(), "history should be empty after removing user+assistant+tool")
}

func TestRetry_AliasR(t *testing.T) {
	agent := core.NewAgentImpl("test", stubLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}))

	c, buf := newTestCmd()
	sc := &slashContext{out: buf, agent: agent}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "r"}, sc)

	assert.Equal(t, "hello", pendingInput)
	assert.Contains(t, buf.String(), "Retrying last message...")
}

// ---------------------------------------------------------------------------
// /edit command tests
// ---------------------------------------------------------------------------

// withTestEditor sets EDITOR to a command that copies a source file to the
// editor's target file, simulating an editor session. It returns a cleanup
// function that restores the original EDITOR.
func withTestEditor(t *testing.T, sourceContent string) func() {
	t.Helper()
	sourceFile, err := os.CreateTemp("", "edit-source-*.txt")
	require.NoError(t, err)
	_, err = sourceFile.WriteString(sourceContent)
	require.NoError(t, err)
	sourceFile.Close()

	oldEditor := os.Getenv("EDITOR")
	os.Setenv("EDITOR", "cp "+sourceFile.Name())

	return func() {
		os.Setenv("EDITOR", oldEditor)
		os.Remove(sourceFile.Name())
	}
}

// TestEditCommand_ContentSubmitted verifies AC-4: editor content is returned
// as pendingInput for submission.
func TestEditCommand_ContentSubmitted(t *testing.T) {
	cleanup := withTestEditor(t, "Hello from editor!\nMultiple lines.\n")
	defer cleanup()

	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "edit"}, sc)

	assert.Equal(t, "Hello from editor!\nMultiple lines.\n", pendingInput)
}

// TestEditCommand_EmptyContent verifies AC-5: empty content is not submitted.
func TestEditCommand_EmptyContent(t *testing.T) {
	oldEditor := os.Getenv("EDITOR")
	os.Setenv("EDITOR", "true")
	defer os.Setenv("EDITOR", oldEditor)

	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "edit"}, sc)

	assert.Empty(t, pendingInput)
	assert.Contains(t, buf.String(), "Empty content")
}

// TestEditCommand_AliasVim verifies that /vim dispatches to /edit.
func TestEditCommand_AliasVim(t *testing.T) {
	cleanup := withTestEditor(t, "vim alias works\n")
	defer cleanup()

	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "vim"}, sc)

	assert.Equal(t, "vim alias works\n", pendingInput)
}

// TestEditCommand_FilenameArg verifies /edit <filename> opens the specified file.
func TestEditCommand_FilenameArg(t *testing.T) {
	// Create a target file with initial content.
	targetFile, err := os.CreateTemp("", "edit-target-*.md")
	require.NoError(t, err)
	targetFile.Close()
	defer os.Remove(targetFile.Name())

	// Use a source file to copy content into the target via the editor.
	sourceFile, err := os.CreateTemp("", "edit-source-*.txt")
	require.NoError(t, err)
	_, err = sourceFile.WriteString("file content\n")
	require.NoError(t, err)
	sourceFile.Close()
	defer os.Remove(sourceFile.Name())

	oldEditor := os.Getenv("EDITOR")
	os.Setenv("EDITOR", "cp "+sourceFile.Name())
	defer os.Setenv("EDITOR", oldEditor)

	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	pendingInput := c.handleSlashCommand(context.Background(),
		session.SlashCommand{Name: "edit", Args: []string{targetFile.Name()}}, sc)

	assert.Equal(t, "file content\n", pendingInput)
}

// TestEditCommand_EditorFailure verifies that when the editor exits with a
// non-zero status, the error is reported and no content is submitted.
func TestEditCommand_EditorFailure(t *testing.T) {
	oldEditor := os.Getenv("EDITOR")
	os.Setenv("EDITOR", "false") // exits with non-zero status
	defer os.Setenv("EDITOR", oldEditor)

	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "edit"}, sc)

	assert.Empty(t, pendingInput)
	assert.Contains(t, buf.String(), "Error:")
	assert.Contains(t, buf.String(), "editor exited with error")
}

// TestEditCommand_WhitespaceOnlyContent verifies that whitespace-only content
// is treated the same as empty content (AC-5).
func TestEditCommand_WhitespaceOnlyContent(t *testing.T) {
	cleanup := withTestEditor(t, "   \n\t\n  \n")
	defer cleanup()

	c, buf := newTestCmd()
	sc := &slashContext{out: buf}
	pendingInput := c.handleSlashCommand(context.Background(), session.SlashCommand{Name: "edit"}, sc)

	assert.Empty(t, pendingInput)
	assert.Contains(t, buf.String(), "Empty content")
}
