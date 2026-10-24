package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/extension"
)

func TestApprovalModeEntryPrecedence(t *testing.T) {
	for _, test := range []struct {
		name, configured string
		args             []string
		want             ApproveMode
	}{
		{name: "default-ask", want: ApproveAsk},
		{name: "configured-auto", configured: "auto", want: ApproveAuto},
		{name: "configured-deny", configured: "deny", want: ApproveDeny},
		{name: "explicit-ask-over-auto", configured: "auto", args: []string{"--approve", "ask"}, want: ApproveAsk},
		{name: "explicit-auto-over-deny", configured: "deny", args: []string{"--approve", "auto"}, want: ApproveAuto},
		{name: "explicit-deny-over-ask", configured: "ask", args: []string{"--approve", "deny"}, want: ApproveDeny},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := newAssembleTestConfig(t)
			cfg.Approval.Mode = test.configured
			repl := &REPLSession{cfg: cfg, args: test.args, out: io.Discard}
			require.NoError(t, repl.parseFlags())
			require.Equal(t, test.want, repl.approveMode)
			parsed, err := parsePromptArgs(append(append([]string(nil), test.args...), "message"), io.Discard)
			require.NoError(t, err)
			resolved, err := resolveApprovalMode(cfg, parsed.approveMode, parsed.approveExplicit)
			require.NoError(t, err)
			require.Equal(t, test.want, resolved)
		})
	}
}

func TestApprovalModeEntryRejectsInvalidValues(t *testing.T) {
	for _, test := range []struct {
		name, configured string
		args             []string
	}{
		{name: "invalid-flag", args: []string{"--approve", "unexpected"}},
		{name: "invalid-config", configured: "unexpected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := newAssembleTestConfig(t)
			cfg.Approval.Mode = test.configured
			repl := &REPLSession{cfg: cfg, args: test.args, out: io.Discard}
			require.Error(t, repl.parseFlags())
		})
	}
}

func TestApprovalModeAutoAndDenyDoNotUseProvidedModalChannel(t *testing.T) {
	for _, mode := range []ApproveMode{ApproveAuto, ApproveDeny} {
		t.Run(mode.String(), func(t *testing.T) {
			channel := make(chan approval.InteractiveRequest, 1)
			assembly, write, path := newAssembledApprovalWrite(t, WithApprovalChannel(channel), WithApproveMode(mode))
			require.Equal(t, mode, assembly.ApprovalMode)
			require.Nil(t, assembly.ApprovalChannel)
			result, err := write.Execute(context.Background(), assembledWriteCall(path))
			if mode == ApproveAuto {
				require.NoError(t, err)
				require.NotNil(t, result)
				data, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				require.Equal(t, "approved", string(data))
				require.NoError(t, assembly.PlanCtrl.Enter(context.Background(), "auto still honors plan"))
				_, err = write.Execute(context.Background(), assembledWriteCall(filepath.Join(filepath.Dir(path), "blocked.txt")))
				require.ErrorIs(t, err, approval.ErrToolDenied)
			} else {
				require.ErrorIs(t, err, approval.ErrToolDenied)
				require.NoFileExists(t, path)
			}
			require.Empty(t, channel)
		})
	}
}

type approvalModeExtensionProbe struct{ calls int }

func (p *approvalModeExtensionProbe) Name() string { return "approval-mode-probe" }
func (p *approvalModeExtensionProbe) Load(context.Context, string) ([]extension.Extension, error) {
	p.calls++
	return nil, nil
}

func TestInvalidConfiguredApprovalModeRejectsBeforeExtensionLoad(t *testing.T) {
	cfg := newAssembleTestConfig(t)
	cfg.Approval.Mode = "unexpected"
	enabled := true
	cfg.Extensions = config.ExtensionsConfig{Enabled: &enabled, PluginPaths: []string{"probe"}}
	probe := &approvalModeExtensionProbe{}
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, withPluginLoaderForTest(probe))
	require.Nil(t, assembly)
	require.ErrorContains(t, err, "approval.mode")
	require.Zero(t, probe.calls)
}

func TestConfiguredApprovalModeControlsAssemblyWithoutFlag(t *testing.T) {
	for _, mode := range []ApproveMode{ApproveAuto, ApproveDeny} {
		t.Run(mode.String(), func(t *testing.T) {
			cfg := newAssembleTestConfig(t)
			cfg.Approval.Mode = mode.String()
			channel := make(chan approval.InteractiveRequest, 1)
			assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, WithApprovalChannel(channel))
			require.NoError(t, err)
			t.Cleanup(assembly.Cleanup)
			require.Equal(t, mode, assembly.ApprovalMode)
			require.Nil(t, assembly.ApprovalChannel)
			write, err := assembly.ToolRegistry.Get(context.Background(), "write")
			require.NoError(t, err)
			path := filepath.Join(cfg.Git.WorkDir, "configured-mode.txt")
			_, err = write.Execute(context.Background(), assembledWriteCall(path))
			if mode == ApproveAuto {
				require.NoError(t, err)
				require.FileExists(t, path)
			} else {
				require.ErrorIs(t, err, approval.ErrToolDenied)
				require.NoFileExists(t, path)
			}
			require.Empty(t, channel)
		})
	}
}
