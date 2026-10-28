package tools

import (
	"context"
	"errors"
)

// ReviewedToolRegistrar is the host registration route for the closed set of
// first-party synchronous process and in-memory tools. It grants completion
// only; ordinary Register and substituted dependencies remain unproven.
type ReviewedToolRegistrar interface {
	RegisterReviewedTool(context.Context, ToolDefinition, ProjectTrustBinding) error
}

// RegisterReviewedTool registers an exact first-party implementation, retaining
// its optional verified source binding. It cannot attest arbitrary callbacks.
func RegisterReviewedTool(ctx context.Context, registry ToolRegistry, definition ToolDefinition, binding ProjectTrustBinding) error {
	registrar, ok := registry.(ReviewedToolRegistrar)
	if !ok {
		return ErrPreparedRegistrationUnavailable
	}
	return registrar.RegisterReviewedTool(ctx, definition, binding)
}

func (r *DefaultToolRegistry) RegisterReviewedTool(ctx context.Context, definition ToolDefinition, binding ProjectTrustBinding) error {
	if !reviewedRuntimeDefinition(definition) {
		return errors.New("tools: reviewed registration requires an exact supported first-party implementation")
	}
	if binding != nil && !validProjectTrustBinding(binding) {
		return ErrProjectTrustBindingInvalid
	}
	proof := CompletionInlinePipelineJoined
	if !reviewedStateDefinition(definition) && !ownedCommandPlatformSupported() {
		proof = ""
	}
	return r.registerDefaultToolGuardedWithBindingProof(ctx, definition, binding, proof)
}

func (r *MiddlewareToolRegistry) RegisterReviewedTool(ctx context.Context, definition ToolDefinition, binding ProjectTrustBinding) error {
	return RegisterReviewedTool(ctx, r.inner, definition, binding)
}

func (a *DeferredToolRegistryAdapter) RegisterReviewedTool(ctx context.Context, definition ToolDefinition, binding ProjectTrustBinding) error {
	return a.registerTrustedWithMCPReservation(definition, func() error {
		return RegisterReviewedTool(ctx, a.underlying, definition, binding)
	})
}

func reviewedRuntimeDefinition(definition ToolDefinition) bool {
	if reviewedStateDefinition(definition) {
		return true
	}
	var git GitTool
	switch tool := definition.(type) {
	case *CustomCommandTool:
		return tool != nil && len(tool.command) > 0 && tool.command[0] != ""
	case *GitPRCreateTool:
		return tool != nil
	case *GitDiffTool:
		if tool != nil {
			git = tool.git
		}
	case *GitStatusTool:
		if tool != nil {
			git = tool.git
		}
	case *GitCommitTool:
		if tool != nil {
			git = tool.git
		}
	case *GitLogTool:
		if tool != nil {
			git = tool.git
		}
	case *GitBranchTool:
		if tool != nil {
			git = tool.git
		}
	case *GitCheckoutTool:
		if tool != nil {
			git = tool.git
		}
	case *GitBlameTool:
		if tool != nil {
			git = tool.git
		}
	case *GitPushTool:
		if tool != nil {
			git = tool.git
		}
	case *GitCreateBranchTool:
		if tool != nil {
			git = tool.git
		}
	case *GitMergeTool:
		if tool != nil {
			git = tool.git
		}
	case *GitStashTool:
		if tool != nil {
			git = tool.git
		}
	case *GitStashPopTool:
		if tool != nil {
			git = tool.git
		}
	case *GitResetTool:
		if tool != nil {
			git = tool.git
		}
	case *GitRevertTool:
		if tool != nil {
			git = tool.git
		}
	case *GitFetchTool:
		if tool != nil {
			git = tool.git
		}
	case *GitPullTool:
		if tool != nil {
			git = tool.git
		}
	case *GitRemoteTool:
		if tool != nil {
			git = tool.git
		}
	default:
		return false
	}
	concrete, ok := git.(*DefaultGitTool)
	return ok && concrete != nil
}

func reviewedStateDefinition(definition ToolDefinition) bool {
	memoryGoal := func(store GoalStore) bool {
		concrete, ok := store.(*DefaultGoalStore)
		return ok && concrete != nil && concrete.path == ""
	}
	switch tool := definition.(type) {
	case *TaskCreateTool:
		return tool != nil && tool.store != nil
	case *TaskUpdateTool:
		return tool != nil && tool.store != nil
	case *TaskGetTool:
		return tool != nil && tool.store != nil
	case *TaskListTool:
		return tool != nil && tool.store != nil
	case *GoalCreateTool:
		return tool != nil && memoryGoal(tool.store)
	case *GoalUpdateTool:
		return tool != nil && memoryGoal(tool.store)
	case *GoalGetTool:
		return tool != nil && memoryGoal(tool.store) && tool.taskStore != nil
	case *GoalListTool:
		return tool != nil && memoryGoal(tool.store) && tool.taskStore != nil
	case *TodoWriteTool:
		return tool != nil && tool.store != nil && tool.onChange == nil
	default:
		return false
	}
}

var (
	_ ReviewedToolRegistrar = (*DefaultToolRegistry)(nil)
	_ ReviewedToolRegistrar = (*MiddlewareToolRegistry)(nil)
	_ ReviewedToolRegistrar = (*DeferredToolRegistryAdapter)(nil)
)
