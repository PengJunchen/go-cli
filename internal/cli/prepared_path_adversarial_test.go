package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestAssemblePreparedScopedGrepAndFindKeepAdversarialArgumentsInert(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX helper shims are unavailable on Windows")
	}

	ctx := context.Background()
	allowedRoot := t.TempDir()
	outsideRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(allowedRoot, "needle.txt"), []byte("needle\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(allowedRoot, "--exec=touch"), nil, 0o600))

	helperDir := t.TempDir()
	grepArgsPath := filepath.Join(helperDir, "grep-args")
	grepConfigPath := filepath.Join(helperDir, "grep-config-env")
	grepOptionMarker := filepath.Join(helperDir, "grep-option-ran")
	findArgsPath := filepath.Join(helperDir, "find-args")
	findOptionMarker := filepath.Join(helperDir, "find-option-ran")
	findMatch := filepath.Join(allowedRoot, "--exec=touch")
	grepShim := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$R4_GREP_ARGS\"\n" +
		"if [ \"${RIPGREP_CONFIG_PATH+x}\" = x ]; then printf present > \"$R4_GREP_CONFIG_ENV\"; else printf '<unset>' > \"$R4_GREP_CONFIG_ENV\"; fi\n" +
		"saw_pattern_value=0\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$saw_pattern_value\" = 1 ]; then saw_pattern_value=0; continue; fi\n" +
		"  case \"$arg\" in\n" +
		"    -e) saw_pattern_value=1 ;;\n" +
		"    --pre|--pre=*) : > \"$R4_GREP_OPTION_MARKER\" ;;\n" +
		"  esac\n" +
		"done\n" +
		"printf '%s\\n' '{\"type\":\"match\",\"data\":{\"path\":{\"text\":\"needle.txt\"},\"lines\":{\"text\":\"needle\\n\"},\"line_number\":1}}'\n"
	findShim := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$R4_FIND_ARGS\"\n" +
		"after_separator=0\n" +
		"for arg in \"$@\"; do\n" +
		"  if [ \"$arg\" = -- ]; then after_separator=1; continue; fi\n" +
		"  if [ \"$after_separator\" = 0 ]; then\n" +
		"    case \"$arg\" in --exec|--exec=*) : > \"$R4_FIND_OPTION_MARKER\" ;; esac\n" +
		"  fi\n" +
		"done\n" +
		"printf '%s\\000' \"$R4_FIND_MATCH\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "rg"), []byte(grepShim), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "fd"), []byte(findShim), 0o700))
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("R4_GREP_ARGS", grepArgsPath)
	t.Setenv("R4_GREP_CONFIG_ENV", grepConfigPath)
	t.Setenv("R4_GREP_OPTION_MARKER", grepOptionMarker)
	t.Setenv("R4_FIND_ARGS", findArgsPath)
	t.Setenv("R4_FIND_OPTION_MARKER", findOptionMarker)
	t.Setenv("R4_FIND_MATCH", findMatch)
	// The path is deliberately hostile. The prepared Grep executor must remove
	// it from the child environment and pass --no-config explicitly.
	t.Setenv("RIPGREP_CONFIG_PATH", filepath.Join(helperDir, "hostile-rg-config"))

	cfg := newAssembleTestConfig(t)
	cfg.Git.WorkDir = allowedRoot
	cfg.Sandbox.AllowedPaths = []string{allowedRoot}
	assembly, err := AssembleAgent(ctx, cfg, "openai", "prepared-path-adversarial", io.Discard, WithApproveMode(ApproveDeny))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NoError(t, assembly.PlanCtrl.Enter(ctx, "verify scoped helper argument handling"))
	preparer, ok := assembly.ToolRegistry.(tools.ToolCallPreparer)
	require.True(t, ok)

	grepCall := tools.ToolCall{Name: "grep", Args: map[string]any{"pattern": "--pre=touch", "path": allowedRoot}}
	findCall := tools.ToolCall{Name: "find", Args: map[string]any{"pattern": "--exec=touch", "path": allowedRoot, "type": "f"}}
	for _, call := range []tools.ToolCall{
		{Name: "grep", Args: map[string]any{"pattern": "needle", "path": outsideRoot}},
		{Name: "find", Args: map[string]any{"pattern": "--exec=touch", "path": outsideRoot}},
	} {
		invocation, prepareErr := preparer.PrepareToolCall(ctx, call)
		require.Error(t, prepareErr, call.Name)
		require.Nil(t, invocation, call.Name)
	}
	assert.NoFileExists(t, grepArgsPath, "out-of-scope Grep must be denied before its helper starts")
	assert.NoFileExists(t, findArgsPath, "out-of-scope Find must be denied before its helper starts")

	for _, call := range []tools.ToolCall{grepCall, findCall} {
		invocation, prepareErr := preparer.PrepareToolCall(ctx, call)
		require.NoError(t, prepareErr, call.Name)
		require.NotNil(t, invocation)
		assert.Equal(t, tools.RegistrationSourceBuiltin, invocation.ToolIdentity().RegistrationSource(), call.Name)
		assert.Equal(t, call.Name, invocation.ToolIdentity().ToolName(), call.Name)
		assert.Equal(t, []tools.TrustedToolEffect{tools.EffectWorkspaceRead}, invocation.EffectSet().Values(), call.Name)
		assert.NotEqual(t, [32]byte{}, invocation.ToolIdentity().Scope().Fingerprint(), call.Name)
		_, dispatchErr := invocation.Dispatch(ctx)
		require.NoError(t, dispatchErr, call.Name)
	}

	grepArgs, err := os.ReadFile(grepArgsPath)
	require.NoError(t, err)
	assert.Equal(t, []string{"--no-config", "--json", "-n", "-e", "--pre=touch", "--", allowedRoot}, splitCapturedArguments(t, grepArgs))
	grepConfigValue, err := os.ReadFile(grepConfigPath)
	require.NoError(t, err)
	assert.Equal(t, "<unset>", string(grepConfigValue), "ambient ripgrep configuration must not reach the prepared helper")
	assert.NoFileExists(t, grepOptionMarker, "the shim recognizes a command option only outside the -e pattern value")
	findArgs, err := os.ReadFile(findArgsPath)
	require.NoError(t, err)
	assert.Equal(t, []string{"--print0", "--glob", "--case-sensitive", "--hidden", "--no-ignore", "--type", "f", "--", "--exec=touch", allowedRoot}, splitCapturedArguments(t, findArgs))
	assert.NoFileExists(t, findOptionMarker, "fd's execution option must remain after the option separator as a pattern")
}

func TestAssemblePreparedGrepIgnoresHostileRipgrepConfigWhenInstalled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ripgrep process fixture requires POSIX")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg is not installed")
	}

	ctx := context.Background()
	root := t.TempDir()
	file := filepath.Join(root, "needle.txt")
	require.NoError(t, os.WriteFile(file, []byte("needle\n"), 0o600))
	oldTime := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.Local)
	require.NoError(t, os.Chtimes(file, oldTime, oldTime))
	configPath := filepath.Join(t.TempDir(), "hostile-rg-config")
	require.NoError(t, os.WriteFile(configPath, []byte("--pre=touch\n"), 0o600))
	t.Setenv("RIPGREP_CONFIG_PATH", configPath)

	rgPath, err := exec.LookPath("rg")
	require.NoError(t, err)
	helperDir := t.TempDir()
	rgStatusPath := filepath.Join(helperDir, "rg-exit-status")
	rgArgsPath := filepath.Join(helperDir, "rg-args")
	rgConfigPath := filepath.Join(helperDir, "rg-config-env")
	rgWrapper := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$R4_NATIVE_RG_ARGS\"\n" +
		"if [ \"${RIPGREP_CONFIG_PATH+x}\" = x ]; then printf present > \"$R4_NATIVE_RG_CONFIG\"; else printf '<unset>' > \"$R4_NATIVE_RG_CONFIG\"; fi\n" +
		"\"$R4_NATIVE_RG_REAL\" \"$@\"\n" +
		"status=$?\n" +
		"printf '%s' \"$status\" > \"$R4_NATIVE_RG_STATUS\"\n" +
		"exit \"$status\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "rg"), []byte(rgWrapper), 0o700))
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("R4_NATIVE_RG_REAL", rgPath)
	t.Setenv("R4_NATIVE_RG_STATUS", rgStatusPath)
	t.Setenv("R4_NATIVE_RG_ARGS", rgArgsPath)
	t.Setenv("R4_NATIVE_RG_CONFIG", rgConfigPath)

	cfg := newAssembleTestConfig(t)
	cfg.Git.WorkDir = root
	cfg.Sandbox.AllowedPaths = []string{root}
	assembly, err := AssembleAgent(ctx, cfg, "openai", "prepared-rg-native", io.Discard, WithApproveMode(ApproveDeny))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NoError(t, assembly.PlanCtrl.Enter(ctx, "verify hostile ripgrep configuration is ignored"))
	preparer := assembly.ToolRegistry.(tools.ToolCallPreparer)
	invocation, err := preparer.PrepareToolCall(ctx, tools.ToolCall{Name: "grep", Args: map[string]any{"pattern": "needle", "path": root}})
	require.NoError(t, err)
	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, strings.Contains(result.Output, "needle.txt:1:needle"), "the hostile preprocessor config must not suppress the normal grep result")
	rgStatus, err := os.ReadFile(rgStatusPath)
	require.NoError(t, err, "the native rg wrapper must run, even if Grep can fall back")
	assert.Equal(t, "0", string(rgStatus), "the native rg helper must accept the prepared arguments and return a match")
	rgArgs, err := os.ReadFile(rgArgsPath)
	require.NoError(t, err)
	assert.Contains(t, splitCapturedArguments(t, rgArgs), "--no-config")
	rgConfigEnv, err := os.ReadFile(rgConfigPath)
	require.NoError(t, err)
	assert.Equal(t, "<unset>", string(rgConfigEnv), "the native helper must not inherit RIPGREP_CONFIG_PATH")
	info, err := os.Stat(file)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(oldTime), "the hostile preprocessor command must not run against the searched file")
}

func TestAssemblePreparedFindWithInstalledFDKeepsDashPatternInert(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fd integration fixture requires POSIX filenames")
	}
	if _, err := exec.LookPath("fd"); err != nil {
		t.Skip("fd is not installed")
	}

	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "--exec=touch")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	oldTime := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.Local)
	require.NoError(t, os.Chtimes(path, oldTime, oldTime))

	fdPath, err := exec.LookPath("fd")
	require.NoError(t, err)
	helperDir := t.TempDir()
	fdStatusPath := filepath.Join(helperDir, "fd-exit-status")
	fdArgsPath := filepath.Join(helperDir, "fd-args")
	fdWrapper := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$R4_NATIVE_FD_ARGS\"\n" +
		"\"$R4_NATIVE_FD_REAL\" \"$@\"\n" +
		"status=$?\n" +
		"printf '%s' \"$status\" > \"$R4_NATIVE_FD_STATUS\"\n" +
		"exit \"$status\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "fd"), []byte(fdWrapper), 0o700))
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("R4_NATIVE_FD_REAL", fdPath)
	t.Setenv("R4_NATIVE_FD_STATUS", fdStatusPath)
	t.Setenv("R4_NATIVE_FD_ARGS", fdArgsPath)

	cfg := newAssembleTestConfig(t)
	cfg.Git.WorkDir = root
	cfg.Sandbox.AllowedPaths = []string{root}
	assembly, err := AssembleAgent(ctx, cfg, "openai", "prepared-fd-native", io.Discard, WithApproveMode(ApproveDeny))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NoError(t, assembly.PlanCtrl.Enter(ctx, "verify dash pattern is inert for installed fd"))
	preparer := assembly.ToolRegistry.(tools.ToolCallPreparer)
	invocation, err := preparer.PrepareToolCall(ctx, tools.ToolCall{Name: "find", Args: map[string]any{"pattern": "--exec=touch", "path": root, "type": "f"}})
	require.NoError(t, err)
	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1, result.Metadata["matches"], "the dash-prefixed filename remains a pattern match")
	fdStatus, err := os.ReadFile(fdStatusPath)
	require.NoError(t, err, "the native fd wrapper must run, even if Find can fall back")
	assert.Equal(t, "0", string(fdStatus), "the native fd helper must accept the prepared arguments and find the file")
	fdArgs, err := os.ReadFile(fdArgsPath)
	require.NoError(t, err)
	fdArgumentList := splitCapturedArguments(t, fdArgs)
	separatorIndex := -1
	for index, argument := range fdArgumentList {
		if argument == "--" {
			separatorIndex = index
			break
		}
	}
	require.GreaterOrEqual(t, separatorIndex, 0, "the native helper call must include an option separator")
	require.Less(t, separatorIndex+1, len(fdArgumentList), "the pattern must follow the helper option separator")
	assert.Equal(t, "--exec=touch", fdArgumentList[separatorIndex+1], "the dash-prefixed pattern follows -- as data")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(oldTime), "if the pattern became fd --exec, touch would update the file's timestamp marker")
}

func splitCapturedArguments(t *testing.T, data []byte) []string {
	t.Helper()
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}
