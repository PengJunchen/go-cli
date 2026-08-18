package tools

import (
	"context"
	"fmt"
	"log/slog"
)

// RegisterDefaultsOption configures RegisterDefaults behavior.
type RegisterDefaultsOption func(*registerDefaultsConfig)

type registerDefaultsConfig struct {
	fileTracker      *FileTracker
	diffGenerator    DiffGenerator
	bashSandbox      BashSandbox
	resourceLimits   ResourceLimits
	gitTool          GitTool
	builtinWhitelist map[string]bool
	pathWhitelist    []string
}

// WithRegisteredFileTracker wires a FileTracker into the WriteTool and
// EditFileTool registered by RegisterDefaults.
func WithRegisteredFileTracker(ft *FileTracker) RegisterDefaultsOption {
	return func(c *registerDefaultsConfig) { c.fileTracker = ft }
}

// WithRegisteredDiffGenerator wires a DiffGenerator into the WriteTool and
// EditFileTool registered by RegisterDefaults.
func WithRegisteredDiffGenerator(dg DiffGenerator) RegisterDefaultsOption {
	return func(c *registerDefaultsConfig) { c.diffGenerator = dg }
}

// WithRegisteredBashSandbox wires a BashSandbox into the BashTool registered by
// RegisterDefaults.
func WithRegisteredBashSandbox(sb BashSandbox) RegisterDefaultsOption {
	return func(c *registerDefaultsConfig) { c.bashSandbox = sb }
}

// WithRegisteredResourceLimits wires ResourceLimits into the BashTool
// registered by RegisterDefaults.
func WithRegisteredResourceLimits(limits ResourceLimits) RegisterDefaultsOption {
	return func(c *registerDefaultsConfig) { c.resourceLimits = limits }
}

// WithRegisteredGitTool wires a GitTool into the git tools (diff, status,
// commit, log, branch, checkout, blame, push, create_branch, merge, stash,
// stash_pop, reset, revert, fetch, pull, remote) registered by RegisterDefaults.
func WithRegisteredGitTool(git GitTool) RegisterDefaultsOption {
	return func(c *registerDefaultsConfig) { c.gitTool = git }
}

// WithRegisteredBuiltinWhitelist restricts RegisterDefaults to only the named
// builtin tools. When names is empty or nil, all builtins are registered (the
// default behavior). The whitelist is matched by tool name (e.g. "bash",
// "read", "git_diff").
func WithRegisteredBuiltinWhitelist(names []string) RegisterDefaultsOption {
	return func(c *registerDefaultsConfig) {
		if len(names) == 0 {
			return
		}
		c.builtinWhitelist = make(map[string]bool, len(names))
		for _, n := range names {
			c.builtinWhitelist[n] = true
		}
	}
}

// WithRegisteredPathWhitelist wires a path whitelist into the WriteTool and
// EditFileTool registered by RegisterDefaults. When configured, write and edit
// operations are restricted to paths within the allowed base directories,
// matching the defense-in-depth provided by the bash sandbox.
func WithRegisteredPathWhitelist(paths []string) RegisterDefaultsOption {
	return func(c *registerDefaultsConfig) { c.pathWhitelist = paths }
}

// RegisterDefaults registers the built-in read, bash, write, edit, grep, find
// and ls tools into the given registry. It returns an error if any
// registration conflicts with an existing tool name. Options may be passed to
// wire dependencies (FileTracker, DiffGenerator, BashSandbox) into the
// registered tools; when no options are provided the tools use their defaults.
func RegisterDefaults(ctx context.Context, reg ToolRegistry, opts ...RegisterDefaultsOption) error {
	if reg == nil {
		return fmt.Errorf("tools: nil registry")
	}

	cfg := registerDefaultsConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	// Build WriteTool options.
	writeOpts := []WriteToolOption{}
	if cfg.fileTracker != nil {
		writeOpts = append(writeOpts, WithFileTracker(cfg.fileTracker))
	}
	if cfg.diffGenerator != nil {
		writeOpts = append(writeOpts, WithDiffGenerator(cfg.diffGenerator))
	}
	if len(cfg.pathWhitelist) > 0 {
		writeOpts = append(writeOpts, WithWritePathWhitelist(cfg.pathWhitelist))
	}

	// Build EditFileTool options.
	editOpts := []EditFileToolOption{}
	if cfg.fileTracker != nil {
		editOpts = append(editOpts, WithEditFileTracker(cfg.fileTracker))
	}
	if cfg.diffGenerator != nil {
		editOpts = append(editOpts, WithEditDiffGenerator(cfg.diffGenerator))
	}
	if len(cfg.pathWhitelist) > 0 {
		editOpts = append(editOpts, WithEditPathWhitelist(cfg.pathWhitelist))
	}

	// Build BashTool options.
	bashOpts := []BashToolOption{}
	if cfg.bashSandbox != nil {
		bashOpts = append(bashOpts, WithBashSandbox(cfg.bashSandbox))
	}
	if cfg.resourceLimits.MaxMemory > 0 || cfg.resourceLimits.MaxCPU > 0 {
		bashOpts = append(bashOpts, WithResourceLimits(cfg.resourceLimits))
	}

	// Build GrepTool options.
	grepOpts := []GrepToolOption{}
	if len(cfg.pathWhitelist) > 0 {
		grepOpts = append(grepOpts, WithGrepPathWhitelist(cfg.pathWhitelist))
	}

	// Build FindTool options.
	findOpts := []FindToolOption{}
	if len(cfg.pathWhitelist) > 0 {
		findOpts = append(findOpts, WithFindPathWhitelist(cfg.pathWhitelist))
	}

	// Build LSTool options.
	lsOpts := []LSToolOption{}
	if len(cfg.pathWhitelist) > 0 {
		lsOpts = append(lsOpts, WithLSPathWhitelist(cfg.pathWhitelist))
	}

	defs := []ToolDefinition{
		NewReadTool(),
		NewStreamingBashTool(bashOpts...),
		NewWriteTool(writeOpts...),
		NewEditFileTool(editOpts...),
		NewGrepTool(grepOpts...),
		NewFindTool(findOpts...),
		NewLSTool(lsOpts...),
	}

	// Register git tools when a GitTool is wired.
	if cfg.gitTool != nil {
		defs = append(defs,
			NewGitDiffTool(cfg.gitTool),
			NewGitStatusTool(cfg.gitTool),
			NewGitCommitTool(cfg.gitTool),
			NewGitLogTool(cfg.gitTool),
			NewGitBranchTool(cfg.gitTool),
			NewGitCheckoutTool(cfg.gitTool),
			NewGitBlameTool(cfg.gitTool),
			NewGitPushTool(cfg.gitTool),
			NewGitCreateBranchTool(cfg.gitTool),
			NewGitMergeTool(cfg.gitTool),
			NewGitStashTool(cfg.gitTool),
			NewGitStashPopTool(cfg.gitTool),
			NewGitResetTool(cfg.gitTool),
			NewGitRevertTool(cfg.gitTool),
			NewGitFetchTool(cfg.gitTool),
			NewGitPullTool(cfg.gitTool),
			NewGitRemoteTool(cfg.gitTool),
		)
	}

	// When a builtin whitelist is configured, keep only the named builtins.
	if cfg.builtinWhitelist != nil {
		filtered := make([]ToolDefinition, 0, len(defs))
		for _, def := range defs {
			if cfg.builtinWhitelist[def.Name()] {
				filtered = append(filtered, def)
			} else {
				slog.Debug("tools.builtin_skipped", "tool", def.Name())
			}
		}
		defs = filtered
	}

	for _, def := range defs {
		if err := reg.Register(ctx, def); err != nil {
			return fmt.Errorf("tools: register %s: %w", def.Name(), err)
		}
		slog.Info("tools.register_default", "tool", def.Name())
	}

	return nil
}
