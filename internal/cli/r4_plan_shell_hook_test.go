package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
)

type r4ShellHookSpec struct {
	event   string
	command string
}

type r4PlanControllerWithoutStartBoundary struct{}

func (r4PlanControllerWithoutStartBoundary) Enter(context.Context, string) error { return nil }
func (r4PlanControllerWithoutStartBoundary) Exit(context.Context, string) error  { return nil }
func (r4PlanControllerWithoutStartBoundary) IsActive() bool                      { return false }
func (r4PlanControllerWithoutStartBoundary) ShouldBlockWrite(string) bool        { return false }

func TestPlanAwareShellHookStartReturnsTypedDenialAndResumesAfterExit(t *testing.T) {
	controller := core.NewDefaultPlanModeController()
	require.NoError(t, controller.Enter(context.Background(), "planning"))

	started := false
	err := planModeShellHookStart(core.EventPreToolUse, controller, func() error {
		started = true
		return nil
	})
	message, safe := core.SafeToolInterceptionMessage(err)
	require.True(t, safe, "Plan denial must remain a typed safe cancellation")
	assert.Equal(t, planModePreToolShellHookDenialMessage, message)
	assert.False(t, started, "pre-tool shell command must not start during Plan mode")

	err = planModeShellHookStart(core.EventSessionStart, controller, func() error {
		started = true
		return nil
	})
	require.IsType(t, planModeLifecycleShellHookSkipped{}, err)
	assert.False(t, started, "lifecycle shell command must not start during Plan mode")

	require.NoError(t, controller.Exit(context.Background(), "done"))
	err = planModeShellHookStart(core.EventPreToolUse, controller, func() error {
		started = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, started, "the captured hook should resume after Plan mode exits")
}

func TestPlanAwareShellHookStartFailsClosedWithoutAtomicControllerBoundary(t *testing.T) {
	started := false
	err := planModeShellHookStart(core.EventPreToolUse, r4PlanControllerWithoutStartBoundary{}, func() error {
		started = true
		return nil
	})
	_, safe := core.SafeToolInterceptionMessage(err)
	assert.True(t, safe, "an external controller without a shared start boundary must not fall through AC-5")
	assert.False(t, started, "a shell command must not start without the Plan transition fence")

	err = planModeShellHookStart(core.EventPreToolUse, &core.DefaultPlanModeController{}, func() error {
		started = true
		return nil
	})
	_, safe = core.SafeToolInterceptionMessage(err)
	assert.True(t, safe, "a controller without its fence must be a typed safe denial")
	assert.False(t, started, "a shell command must not start without the Plan transition fence")
}

func TestPlanAwareShellHookStartPreservesOrdinaryStartError(t *testing.T) {
	startErr := errors.New("private child start detail")
	err := planModeShellHookStart(core.EventPreToolUse, core.NewDefaultPlanModeController(), func() error {
		return startErr
	})
	assert.ErrorIs(t, err, startErr)
	_, safe := core.SafeToolInterceptionMessage(err)
	assert.False(t, safe, "an admitted cmd.Start error must keep ordinary AC-5 handling")
}

func TestAssembledPlanBlocksOpaquePreToolHooksBeforeDispatch(t *testing.T) {
	for _, toolName := range []string{"read", "git_diff"} {
		t.Run(toolName, func(t *testing.T) {
			root := t.TempDir()
			readFile := filepath.Join(root, "private.txt")
			require.NoError(t, os.WriteFile(readFile, []byte("plan-hook-secret-fixture"), 0o600))
			capturedMarker := filepath.Join(root, "captured-pre-tool-hook-ran")

			var secondRequestBodies = make(chan string, 2)
			fixture := newAssemblyStreamFixture(t, func(w http.ResponseWriter, r *http.Request, requestNumber int) {
				if requestNumber%2 == 1 {
					callID := fmt.Sprintf("call-%d", requestNumber)
					args := `{"path":"` + filepath.ToSlash(readFile) + `"}`
					if toolName == "git_diff" {
						args = `{"path":"."}`
					}
					writeR4ToolCallResponse(w, callID, toolName, args)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "read request", http.StatusBadRequest)
					return
				}
				secondRequestBodies <- string(body)
				writeAssemblyTextResponse(w, []string{"turn complete"}, "stop")
			})

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			capturedHooks := []r4ShellHookSpec{{event: "pre_tool_use", command: r4MarkerCommand(capturedMarker)}}
			config := newR4TrustedProjectHookConfig(ctx, t, root, fixture.server.URL, capturedHooks)
			assembly, err := AssembleAgent(ctx, config, config.Provider.Name, config.Provider.Model, io.Discard, WithApproveMode(ApproveAuto))
			require.NoError(t, err)
			t.Cleanup(assembly.Cleanup)

			r4SetPlanModeViaSlash(ctx, t, assembly, "enter")
			stream, err := assembly.Harness.Submit(ctx, "Use the requested tool.")
			require.NoError(t, err)
			events, _, runErr := collectAssemblyEvents(stream)
			require.NoError(t, runErr)
			assert.Contains(t, eventKinds(events), "tool_canceled", "the Plan pre-tool guard must cancel before dispatch")
			assert.NotContains(t, eventKinds(events), "tool_call", "a blocked Plan request must not reach dispatch")

			firstFollowUp := <-secondRequestBodies
			assert.Contains(t, firstFollowUp, planModePreToolShellHookDenialMessage, "the model must receive a safe, actionable denial")
			assert.NotContains(t, firstFollowUp, "plan-hook-secret-fixture", "the read executor must not disclose content after denial")
			assert.NotContains(t, firstFollowUp, "printf 'ran\\n'", "the safe denial must not expose the shell command")
			assert.NotContains(t, firstFollowUp, "captured-pre-tool-hook-ran", "the denial must not expose captured hook arguments")
			assert.NotContains(t, firstFollowUp, "live-pre-tool-hook-ran", "the denial must not expose untrusted hook arguments")
			_, err = os.Stat(capturedMarker)
			assert.ErrorIs(t, err, os.ErrNotExist, "the captured pre-tool hook must not run while Plan is active")

			r4SetPlanModeViaSlash(ctx, t, assembly, "exit")
			stream, err = assembly.Harness.Submit(ctx, "Use the requested tool again.")
			require.NoError(t, err)
			events, _, runErr = collectAssemblyEvents(stream)
			require.NoError(t, runErr)
			assert.NotContains(t, eventKinds(events), "tool_canceled", "exiting Plan must restore captured hook execution")
			secondFollowUp := <-secondRequestBodies
			assert.NotEmpty(t, secondFollowUp, "the model should receive the post-exit tool result")
			assert.FileExists(t, capturedMarker, "the previously captured hook must resume after /plan exit")
		})
	}
}

func TestAssembledPlanReadWithoutPreToolHookStillDispatches(t *testing.T) {
	root := t.TempDir()
	readFile := filepath.Join(root, "allowed.txt")
	require.NoError(t, os.WriteFile(readFile, []byte("scoped plan read"), 0o600))
	var followUpBody string
	fixture := newAssemblyStreamFixture(t, func(w http.ResponseWriter, r *http.Request, requestNumber int) {
		if requestNumber == 1 {
			writeR4ToolCallResponse(w, "call-no-hook-read", "read", `{"path":"`+filepath.ToSlash(readFile)+`"}`)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		followUpBody = string(body)
		writeAssemblyTextResponse(w, []string{"turn complete"}, "stop")
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := newE2ETestConfig(t, fixture.server.URL)
	config.Git.WorkDir = root
	config.Sandbox.AllowedPaths = []string{root}
	assembly, err := AssembleAgent(ctx, config, config.Provider.Name, config.Provider.Model, io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	r4SetPlanModeViaSlash(ctx, t, assembly, "enter")
	stream, err := assembly.Harness.Submit(ctx, "Read the allowed file.")
	require.NoError(t, err)
	events, _, runErr := collectAssemblyEvents(stream)
	require.NoError(t, runErr)
	assert.NotContains(t, eventKinds(events), "tool_canceled")
	assert.Contains(t, followUpBody, "scoped plan read", "a scoped read with no pre-tool hook remains usable in Plan")
	assert.True(t, assembly.PlanCtrl.IsActive(), "the user-owned mode remains active after the read")
}

func TestAssembledPlanSuppressesOpaqueLifecycleShellHooksAndResumesAfterExit(t *testing.T) {
	root := t.TempDir()
	capturedStart := filepath.Join(root, "captured-session-start")
	capturedEnd := filepath.Join(root, "captured-session-end")
	captured := []r4ShellHookSpec{
		{event: "session_start", command: r4MarkerCommand(capturedStart)},
		{event: "session_end", command: r4MarkerCommand(capturedEnd)},
	}
	fixture := newAssemblyStreamFixture(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeAssemblyTextResponse(w, []string{"unused"}, "stop")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config := newR4TrustedProjectHookConfig(ctx, t, root, fixture.server.URL, captured)
	assembly, err := AssembleAgent(ctx, config, config.Provider.Name, config.Provider.Model, io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NotNil(t, assembly.HookChain)

	previousLogger := slog.Default()
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	r4SetPlanModeViaSlash(ctx, t, assembly, "enter")
	logs.Reset()
	require.NoError(t, assembly.HookChain.OnTurnStart(ctx, "plan-start"))
	require.NoError(t, assembly.HookChain.OnTurnEnd(ctx, "plan-start", core.Result{}, nil))
	assert.NoFileExists(t, capturedStart, "opaque session_start hooks must not start during Plan")
	assert.NoFileExists(t, capturedEnd, "opaque session_end hooks must not start during Plan")
	assert.Contains(t, strings.ToLower(logs.String()), "plan mode", "suppression should leave a safe diagnostic")
	assert.NotContains(t, logs.String(), r4MarkerCommand(capturedStart), "diagnostics must not expose shell commands")

	r4SetPlanModeViaSlash(ctx, t, assembly, "exit")
	require.NoError(t, assembly.HookChain.OnTurnStart(ctx, "normal-start"))
	require.NoError(t, assembly.HookChain.OnTurnEnd(ctx, "normal-start", core.Result{}, nil))
	assert.FileExists(t, capturedStart, "the captured session_start hook resumes after Plan exit")
	assert.FileExists(t, capturedEnd, "the captured session_end hook resumes after Plan exit")
}

func newR4TrustedProjectHookConfig(ctx context.Context, t *testing.T, root, baseURL string, hooks []r4ShellHookSpec) *config.Config {
	t.Helper()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	root = canonicalRoot
	t.Chdir(root)

	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".go-cli"), 0o700))
	globalConfig := fmt.Sprintf("provider:\n  name: openai\n  api_key: test-key\n  base_url: %s\n  model: r4-hook-test\nmodel:\n  name: r4-hook-test\ntracing:\n  enabled: false\n", strconv.Quote(baseURL))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".go-cli", "config.yaml"), []byte(globalConfig), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".go-cli"), 0o700))
	hooksPath := filepath.Join(root, ".go-cli", "hooks.yaml")
	require.NoError(t, os.WriteFile(hooksPath, r4HooksYAML(hooks), 0o600))

	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	snapshot, err := config.CaptureProjectSources(ctx, root, nil)
	require.NoError(t, err)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))
	rc, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.NotNil(t, rc.VerifiedProjectSources())
	rc.Git.WorkDir = root
	rc.Sandbox.AllowedPaths = []string{root}
	return rc
}

func r4HooksYAML(hooks []r4ShellHookSpec) []byte {
	var yaml strings.Builder
	yaml.WriteString("hooks:\n")
	for _, hook := range hooks {
		fmt.Fprintf(&yaml, "  - event: %s\n    command: %s\n    timeout: 2s\n", hook.event, strconv.Quote(hook.command))
	}
	return []byte(yaml.String())
}

func r4MarkerCommand(path string) string {
	return "printf 'ran\\n' >> " + r4ShellQuote(path)
}

func r4ShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func r4SetPlanModeViaSlash(ctx context.Context, t *testing.T, assembly *AgentAssembly, action string) {
	t.Helper()
	var output bytes.Buffer
	interactive := &interactiveCmd{out: &output, slashReg: defaultSlashReg}
	deps := &slashContext{out: &output, planCtrl: assembly.PlanCtrl}
	interactive.handleSlashCommand(ctx, session.SlashCommand{Name: "plan", Args: []string{action, "R4 hook policy test"}}, deps)
	if action == "enter" {
		assert.Contains(t, output.String(), "Entered plan mode")
		require.True(t, assembly.PlanCtrl.IsActive())
		return
	}
	assert.Contains(t, output.String(), "Exited plan mode")
	require.False(t, assembly.PlanCtrl.IsActive())
}

func writeR4ToolCallResponse(w http.ResponseWriter, callID, toolName, arguments string) {
	writeAssemblyStreamHeaders(w)
	writeAssemblyChunk(w, map[string]any{"tool_calls": []any{map[string]any{
		"index": 0,
		"id":    callID,
		"type":  "function",
		"function": map[string]any{
			"name":      toolName,
			"arguments": arguments,
		},
	}}}, nil)
	writeAssemblyChunk(w, map[string]any{}, "tool_calls")
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
