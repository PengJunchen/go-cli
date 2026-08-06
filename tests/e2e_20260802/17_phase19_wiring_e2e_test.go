//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 19 wiring fixes (D1-D24) through AssembleAgent:
// approval callback/cache/resolver, FileTracker, DiffGenerator, BashSandbox,
// HTMLConverter, PlanModeController, and WebSearchProvider config-driven
// selection.
package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// =============================================================================
// Shared helpers
// =============================================================================

// phase19wTestConfig returns a minimal Config whose provider section forces
// buildModel down the EinoProvider path (no network calls), so assembly
// succeeds without a live endpoint.
func phase19wTestConfig() *config.Config {
	return &config.Config{
		Provider: config.ProviderConfig{
			Name:    "openai",
			BaseURL: "http://127.0.0.1:0",
			APIKey:  "test-key",
		},
	}
}

// phase19wAssemble calls AssembleAgent and registers cleanup. The timeout
// context bounds the assembly process only; tool execution in the test body
// uses a fresh context.
func phase19wAssemble(t *testing.T, cfg *config.Config) *cli.AgentAssembly {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	assembly, err := cli.AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	return assembly
}

// phase19wFindTool returns the (unwrapped) ToolDefinition with the given name
// from the registry. List() returns the original definitions without middleware
// wrapping, so the concrete type is accessible.
func phase19wFindTool(t *testing.T, tr tools.ToolRegistry, name string) tools.ToolDefinition {
	t.Helper()
	defs, err := tr.List(context.Background())
	require.NoError(t, err)
	for _, d := range defs {
		if d.Name() == name {
			return d
		}
	}
	t.Fatalf("tool %q not found in registry", name)
	return nil
}

// =============================================================================
// AC-1: AssembleAgent creates non-nil FileTracker, DiffGenerator, BashSandbox,
//       HTMLConverter
// =============================================================================

// TestET_Phase19_AC1_AssembledComponentsNotNil verifies that AssembleAgent
// creates and wires the four PARTIAL components: FileTracker (D5) and
// DiffGenerator (D6) are exposed on the assembly; BashSandbox (D7) is wired
// into the BashTool (public Sandbox field); HTMLConverter (D9) is wired into
// the WebFetchTool (verified behaviorally via HTML→Markdown conversion).
func TestET_Phase19_AC1_AssembledComponentsNotNil(t *testing.T) {
	assembly := phase19wAssemble(t, phase19wTestConfig())

	// D5: FileTracker must be created and exposed.
	require.NotNil(t, assembly.FileTracker, "FileTracker must be wired by AssembleAgent")

	// D6: DiffGenerator must be created and exposed.
	require.NotNil(t, assembly.DiffGenerator, "DiffGenerator must be wired by AssembleAgent")

	// D7: BashSandbox must be wired into the BashTool. AssembleAgent now
	// uses StreamingBashToolImpl which embeds *BashTool, so Sandbox is
	// promoted and accessible directly.
	bashDef := phase19wFindTool(t, assembly.ToolRegistry, "bash")
	streamingBash, ok := bashDef.(*tools.StreamingBashToolImpl)
	require.True(t, ok, "bash tool should be *tools.StreamingBashToolImpl")
	require.NotNil(t, streamingBash.Sandbox, "BashSandbox must be wired into BashTool")

	// D9: HTMLConverter must be wired into the WebFetchTool. The converter
	// field is private, so we verify behaviorally: fetch HTML from a test
	// server and confirm the output is Markdown (no HTML tags).
	htmlPage := `<html><head><title>T</title></head><body><h1>Title</h1><p>Para</p></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(htmlPage))
	}))
	defer srv.Close()

	fetchDef := phase19wFindTool(t, assembly.ToolRegistry, "web_fetch")
	fetchTool, ok := fetchDef.(*tools.WebFetchTool)
	require.True(t, ok, "web_fetch tool should be *tools.WebFetchTool")

	res, err := fetchTool.Execute(context.Background(), toolCallWithArgs("web_fetch", map[string]any{
		"url": srv.URL,
	}))
	require.NoError(t, err)
	// Conversion happened → Markdown heading present, no HTML tags.
	assert.Contains(t, res.Output, "# Title")
	assert.NotContains(t, res.Output, "<html>")
	assert.NotContains(t, res.Output, "<h1>")
}

// =============================================================================
// AC-2: PlanModeController is exposed in AgentAssembly and wired into tools
// =============================================================================

// TestET_Phase19_AC2_PlanModeControllerExposedAndWired verifies that
// PlanModeController (D22) is exposed on AgentAssembly and that the
// enter_plan_mode / exit_plan_mode tools in the assembled registry share the
// same controller instance, proving the wiring is live end-to-end.
func TestET_Phase19_AC2_PlanModeControllerExposedAndWired(t *testing.T) {
	assembly := phase19wAssemble(t, phase19wTestConfig())

	// Exposed on assembly.
	require.NotNil(t, assembly.PlanCtrl, "PlanCtrl must be exposed on AgentAssembly")
	assert.False(t, assembly.PlanCtrl.IsActive(), "plan mode should be inactive initially")

	ctx := context.Background()

	// Execute enter_plan_mode through the wrapped registry (E2E: approval +
	// mutation middleware). The tool shares the same PlanCtrl as the
	// assembly, so IsActive must flip to true.
	enterDef, err := assembly.ToolRegistry.Get(ctx, "enter_plan_mode")
	require.NoError(t, err)
	_, err = enterDef.Execute(ctx, toolCallWithArgs("enter_plan_mode", map[string]any{
		"reason": "e2e planning",
	}))
	require.NoError(t, err)
	assert.True(t, assembly.PlanCtrl.IsActive(), "plan mode must be active after enter_plan_mode")

	// Exit plan mode through the registry.
	exitDef, err := assembly.ToolRegistry.Get(ctx, "exit_plan_mode")
	require.NoError(t, err)
	_, err = exitDef.Execute(ctx, toolCallWithArgs("exit_plan_mode", map[string]any{
		"summary": "done",
	}))
	require.NoError(t, err)
	assert.False(t, assembly.PlanCtrl.IsActive(), "plan mode must be inactive after exit_plan_mode")

	// ShouldBlockWrite reflects plan-mode semantics while active.
	require.NoError(t, assembly.PlanCtrl.Enter(ctx, "block check"))
	assert.True(t, assembly.PlanCtrl.ShouldBlockWrite("write"))
	assert.True(t, assembly.PlanCtrl.ShouldBlockWrite("bash"))
	assert.False(t, assembly.PlanCtrl.ShouldBlockWrite("read"))
	require.NoError(t, assembly.PlanCtrl.Exit(ctx, ""))
}

// =============================================================================
// AC-3: PermissionModeResolver is wired into ApprovalMiddleware
// =============================================================================

// TestET_Phase19_AC3_PermissionModeResolverWired verifies that
// PermissionModeResolver (D1/D24) is created by AssembleAgent, exposed on the
// assembly, and functional: it resolves PermissionMode values to the correct
// classifiers. A middleware built with the resolver + PermissionPlan mode
// denies write (Ask→Deny with autoApprove=false), proving the resolver drives
// classification.
func TestET_Phase19_AC3_PermissionModeResolverWired(t *testing.T) {
	assembly := phase19wAssemble(t, phase19wTestConfig())

	// Resolver exposed on assembly.
	require.NotNil(t, assembly.ModeResolver, "ModeResolver must be wired by AssembleAgent")
	assert.Equal(t, "permission_mode", assembly.ModeResolver.Name(),
		"resolver must be the default permission_mode resolver")

	ctx := context.Background()

	// Resolve(PermissionPlan) returns a PlanClassifier that classifies all
	// tools as Ask (holding them for confirmation).
	planClassifier := assembly.ModeResolver.Resolve(approval.PermissionPlan)
	require.NotNil(t, planClassifier)
	assert.Equal(t, approval.Ask, planClassifier.Classify(ctx, toolCall("write")),
		"PermissionPlan classifier should return Ask for write")

	// Resolve(PermissionAutoFull) returns AllowAllClassifier.
	autoFullClassifier := assembly.ModeResolver.Resolve(approval.PermissionAutoFull)
	require.NotNil(t, autoFullClassifier)
	assert.Equal(t, approval.Allow, autoFullClassifier.Classify(ctx, toolCall("write")),
		"PermissionAutoFull classifier should return Allow for write")

	// Behavioral: a middleware built with the resolver + PermissionPlan mode
	// denies write (Ask → Deny with autoApprove=false, no callback). This
	// proves the resolver drives the effective classifier inside the
	// ApprovalMiddleware.
	mw := approval.NewApprovalMiddleware(
		&approval.AllowAllClassifier{}, // static classifier; overridden by resolver
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(false),
		approval.WithPermissionModeResolver(assembly.ModeResolver),
		approval.WithPermissionMode(approval.PermissionPlan),
	)
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(ctx, &recordingTool{name: "write"}))
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall)

	def, err := wrapped.Get(ctx, "write")
	require.NoError(t, err)
	_, err = def.Execute(ctx, toolCall("write"))
	require.ErrorIs(t, err, approval.ErrToolDenied,
		"PermissionPlan + resolver should deny write (Ask→Deny)")
}

// =============================================================================
// AC-4: WebSearchProvider defaults to mock, switches to fetch/brave with config
// =============================================================================

// TestET_Phase19_AC4_WebSearchProviderConfigDriven verifies that the
// WebSearchProvider (D2) is selected from config.WebSearch: default is mock,
// "fetch" uses FetchSearchProvider, "brave" with an API key uses
// BraveSearchProvider, and "brave" without a key falls back to mock.
func TestET_Phase19_AC4_WebSearchProviderConfigDriven(t *testing.T) {
	// Default (no config) → mock.
	t.Run("default_mock", func(t *testing.T) {
		assembly := phase19wAssemble(t, phase19wTestConfig())
		ws := phase19wFindTool(t, assembly.ToolRegistry, "web_search")
		wst, ok := ws.(*tools.WebSearchTool)
		require.True(t, ok, "web_search should be *tools.WebSearchTool")
		assert.Equal(t, "mock", wst.ProviderName())
	})

	// fetch provider.
	t.Run("fetch_provider", func(t *testing.T) {
		cfg := phase19wTestConfig()
		cfg.WebSearch.Provider = "fetch"
		assembly := phase19wAssemble(t, cfg)
		ws := phase19wFindTool(t, assembly.ToolRegistry, "web_search")
		wst, ok := ws.(*tools.WebSearchTool)
		require.True(t, ok)
		assert.Equal(t, "fetch", wst.ProviderName())
	})

	// brave provider with API key.
	t.Run("brave_provider", func(t *testing.T) {
		cfg := phase19wTestConfig()
		cfg.WebSearch.Provider = "brave"
		cfg.WebSearch.APIKey = "test-key"
		assembly := phase19wAssemble(t, cfg)
		ws := phase19wFindTool(t, assembly.ToolRegistry, "web_search")
		wst, ok := ws.(*tools.WebSearchTool)
		require.True(t, ok)
		assert.Equal(t, "brave", wst.ProviderName())
	})

	// brave without API key → falls back to mock.
	t.Run("brave_no_key_falls_back_to_mock", func(t *testing.T) {
		cfg := phase19wTestConfig()
		cfg.WebSearch.Provider = "brave"
		assembly := phase19wAssemble(t, cfg)
		ws := phase19wFindTool(t, assembly.ToolRegistry, "web_search")
		wst, ok := ws.(*tools.WebSearchTool)
		require.True(t, ok)
		assert.Equal(t, "mock", wst.ProviderName())
	})
}

// =============================================================================
// AC-5: WriteTool has FileTracker option wired — write a file, verify checkpoint
// =============================================================================

// TestET_Phase19_AC5_WriteToolFileTrackerCheckpoint verifies that the
// FileTracker (D5) is wired into the WriteTool via WithFileTracker. Writing a
// new file through the assembled (wrapped) registry must produce a checkpoint
// in the shared FileTracker exposed on the assembly.
func TestET_Phase19_AC5_WriteToolFileTrackerCheckpoint(t *testing.T) {
	assembly := phase19wAssemble(t, phase19wTestConfig())

	dir := t.TempDir()
	filePath := filepath.Join(dir, "ac5_new.txt")

	ctx := context.Background()

	// Execute write through the wrapped registry (E2E: approval + mutation
	// middleware → real WriteTool with FileTracker).
	def, err := assembly.ToolRegistry.Get(ctx, "write")
	require.NoError(t, err)

	res, err := def.Execute(ctx, toolCallWithArgs("write", map[string]any{
		"path":    filePath,
		"content": "ac5 content",
	}))
	require.NoError(t, err)
	assert.Contains(t, res.Output, "queued")

	// Verify the file was created on disk.
	data, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, "ac5 content", string(data))

	// Verify a checkpoint was created in the shared FileTracker. The WriteTool
	// calls Backup before writing; for a new file the checkpoint has
	// Existed=false.
	checkpoints := assembly.FileTracker.ListCheckpoints()
	var found bool
	for _, cp := range checkpoints {
		if cp.Path == filePath {
			found = true
			break
		}
	}
	assert.True(t, found, "FileTracker must have a checkpoint for the written file")
}

// =============================================================================
// AC-6: EditTool has FileTracker option wired — edit a file, verify checkpoint
// =============================================================================

// TestET_Phase19_AC6_EditToolFileTrackerCheckpoint verifies that the
// FileTracker (D5) is wired into the EditFileTool via WithEditFileTracker.
// Editing an existing file through the assembled (wrapped) registry must
// produce a checkpoint in the shared FileTracker, and Restore must revert the
// content to the original.
func TestET_Phase19_AC6_EditToolFileTrackerCheckpoint(t *testing.T) {
	assembly := phase19wAssemble(t, phase19wTestConfig())

	dir := t.TempDir()
	filePath := filepath.Join(dir, "ac6_edit.txt")
	original := "line1 alpha\nline2 beta\nline3 gamma\n"
	require.NoError(t, os.WriteFile(filePath, []byte(original), 0644))

	ctx := context.Background()

	// Execute edit through the wrapped registry (E2E: approval + mutation
	// middleware → real EditFileTool with FileTracker).
	def, err := assembly.ToolRegistry.Get(ctx, "edit")
	require.NoError(t, err)

	res, err := def.Execute(ctx, toolCallWithArgs("edit", map[string]any{
		"file_path":  filePath,
		"old_string": "line2 beta",
		"new_string": "line2 delta",
	}))
	require.NoError(t, err)
	assert.Contains(t, res.Output, "queued")

	// Verify the file was modified on disk.
	data, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "line2 delta")
	assert.NotContains(t, string(data), "line2 beta")

	// Verify a checkpoint was created in the shared FileTracker.
	checkpoints := assembly.FileTracker.ListCheckpoints()
	var cpID string
	for _, cp := range checkpoints {
		if cp.Path == filePath {
			cpID = cp.ID
			break
		}
	}
	require.NotEmpty(t, cpID, "FileTracker must have a checkpoint for the edited file")

	// Verify Restore reverts the content (undo).
	require.NoError(t, assembly.FileTracker.Restore(cpID))
	data, err = os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, original, string(data), "Restore must revert to original content")
}
