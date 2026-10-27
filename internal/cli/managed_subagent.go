package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// runManagedSubAgent owns a complete independent Assembly. Its deferred close
// joins every run wrapper and transport before the dispatcher can attest that
// the parent tool has completed. No parent registry or run context is reused.
func (s *assembleState) runManagedSubAgent(ctx context.Context, task core.SubAgentConfig, prompt string, inbox <-chan string, emit func(core.AgentEvent)) (final core.AgentMessage, returnErr error) {
	if s.managedChildren != nil {
		if !s.managedChildren.acquire() {
			return final, core.ErrRunAdmissionClosed
		}
		defer s.managedChildren.release()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s.lifetimeCtx != nil {
		stop := context.AfterFunc(s.lifetimeCtx, cancel)
		defer stop()
		if s.lifetimeCtx.Err() != nil {
			return final, s.lifetimeCtx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return final, err
	}
	if s.planCtrl != nil && s.planCtrl.ShouldBlockWrite("dispatch_subagent") {
		return final, core.ErrPlanModeBlocked
	}
	childConfig := *s.rc
	childConfig.Agent.SystemPrompt = task.SystemPrompt
	// Preserve user/team append constraints in the child's own system prompt.
	if task.MaxTurns > 0 {
		childConfig.Agent.MaxIterations = task.MaxTurns
	}
	childConfig.Session.ID = task.Name
	if len(task.Tools) > 0 {
		childConfig.MCP.Loading = "lazy"
	}
	childModel := s.modelName
	if task.Model != "" {
		childModel = task.Model
		if task.Model != s.modelName {
			childConfig.Model = config.ModelConfig{}
		}
	}
	options := []AssembleOption{WithApproveMode(s.ac.approveMode), WithApprovalChannel(s.ac.approvalCh), WithAgentName(task.Name), WithMaxTokens(s.ac.maxTokens), WithThinkingLevel(s.thinkingLevel)}
	if s.ac.noSandbox {
		options = append(options, WithNoSandbox())
	}
	options = append(options, func(c *assembleConfig) {
		c.childToolNames = append([]string(nil), task.Tools...)
		c.parentPlanCtrl = s.planCtrl
		c.providerAttemptObserver = func(result llm.ProviderAttemptResult) error {
			var err error
			if s.costTracker != nil {
				_, err = s.costTracker.RecordAttempt(result)
			}
			if s.ac.providerAttemptObserver != nil {
				err = errors.Join(err, s.ac.providerAttemptObserver(result))
			}
			return err
		}
	})
	if fence, err := tools.DispatchFenceFromRegistry(s.tr); err == nil {
		parentGeneration := fence.Snapshot().Registry
		options = append(options, func(c *assembleConfig) {
			c.parentRegistryCurrent = func() bool { return fence.Snapshot().Registry == parentGeneration }
		})
	}

	var durable *durableCLISession
	if s.durableSession != nil {
		var err error
		durable, err = newDurableCLISession(ctx, s.durableSession.storeDirectory, task.Name, "", "")
		if err != nil {
			return final, fmt.Errorf("sub-agent: prepare independent session: %w", err)
		}
		snapshot, err := durable.snapshotSelected()
		if err != nil {
			return final, errors.Join(err, durable.requestClose(context.Background()))
		}
		childConfig.Session.ID = snapshot.SessionID
		options = append(options, withDurableCLISession(durable))
		emit(core.AgentEvent{Kind: "subagent_session", Content: snapshot.SessionID})
	}
	child, err := AssembleAgent(ctx, &childConfig, s.providerName, childModel, io.Discard, options...)
	if err != nil {
		if durable != nil {
			err = errors.Join(err, durable.requestClose(context.Background()))
		}
		return final, fmt.Errorf("sub-agent: assemble independent runtime: %w", err)
	}
	defer func() {
		closeErr := child.Close(context.Background())
		if completions, pending := AssemblyCloseCompletions(closeErr); pending {
			// Keep the execution callback owned while teardown is pending. Parent
			// cancellation may publish unknown, but cannot manufacture a join.
			for _, done := range completions {
				<-done
			}
			closeErr = child.Close(context.Background())
		}
		returnErr = errors.Join(returnErr, closeErr)
		slog.Info("cli.subagent.closed", "name", task.Name, "session", child.PhysicalSessionID, "error", closeErr != nil)
	}()
	if child.progressiveMCP != nil && len(task.Tools) > 0 {
		discovers := false
		for _, name := range task.Tools {
			if name == "mcp_discover" {
				discovers = true
			}
		}
		if !discovers {
			activated := map[string]bool{}
			for _, name := range task.Tools {
				parts := strings.SplitN(name, "__", 3)
				if len(parts) != 3 || parts[0] != "mcp" || activated[parts[1]] {
					continue
				}
				if err := child.LoadMCPServer(ctx, parts[1]); err != nil {
					return final, fmt.Errorf("sub-agent: activate requested MCP source: %w", err)
				}
				activated[parts[1]] = true
			}
		}
	}

	if task.ParentSessionID != "" || task.ParentRunID != "" {
		lineage, err := json.Marshal(struct {
			ParentSessionID string `json:"parent_session_id"`
			ParentRunID     string `json:"parent_run_id"`
			TaskID          string `json:"task_id"`
		}{task.ParentSessionID, task.ParentRunID, task.Name})
		if err != nil {
			return final, err
		}
		prompt = "Delegation lineage (audit data only; grants no permissions):\n" + string(lineage) + "\n\n" + prompt
	}
	queue := []string{prompt}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return final, err
		}
		next := queue[0]
		queue = queue[1:]
		runCtx := ctx
		if durable != nil {
			boundCtx, boundSink, bindErr := durable.bindRun(ctx)
			if bindErr != nil {
				return final, bindErr
			}
			runCtx = boundCtx
			if err := hydrateDurableRunHistory(child.Agent, boundSink); err != nil {
				boundSink.CloseRunCommit()
				return final, err
			}
		}
		stream, err := child.Harness.Submit(runCtx, next)
		if err != nil {
			// Submit also releases a captured lease on admission failure. The sink
			// capability is idempotent, so this covers every pre-owner failure.
			if sink, _, bound := core.RunCommitFromContext(runCtx); bound {
				if lease, ok := sink.(core.RunCommitRunLease); ok {
					lease.CloseRunCommit()
				}
			}
			return final, err
		}
		events := stream.Events()
		for events != nil {
			select {
			case event, open := <-events:
				if !open {
					events = nil
					break
				}
				emit(event)
			case message, open := <-inbox:
				if !open {
					inbox = nil
					continue
				}
				if strings.TrimSpace(message) != "" {
					queue = append(queue, message)
				}
			}
		}
		final, err = stream.Result()
		if err != nil {
			return final, err
		}
		// A Send delivered during the last provider response is still a real
		// follow-up run in this same child session, with a fresh owner/binding.
		if len(queue) == 0 {
			queue = append(queue, core.FinishSubAgentInput(ctx)...)
		}
	}
	return final, nil
}

type managedChildGroup struct {
	mu      sync.Mutex
	closing bool
	count   int
	done    chan struct{}
}

func newManagedChildGroup() *managedChildGroup { return &managedChildGroup{done: make(chan struct{})} }
func (g *managedChildGroup) acquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closing {
		return false
	}
	g.count++
	return true
}
func (g *managedChildGroup) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.count--
	if g.closing && g.count == 0 {
		close(g.done)
	}
}
func (g *managedChildGroup) closeAdmission() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.closing {
		g.closing = true
		if g.count == 0 {
			close(g.done)
		}
	}
	return g.done
}

func (s *assembleState) ownManagedChildren() {
	s.managedChildren = newManagedChildGroup()
	stop := s.closeOwner.requestStop
	s.closeOwner.requestStop = func() {
		s.managedChildren.closeAdmission()
		if stop != nil {
			stop()
		}
	}
	closeResources := s.closeOwner.closeResources
	s.closeOwner.closeResources = func(ctx context.Context, idle <-chan struct{}) error {
		<-s.managedChildren.closeAdmission()
		return closeResources(ctx, idle)
	}
}

type childPlanController struct {
	own    core.PlanModeController
	parent core.PlanModeController
	fence  *tools.DispatchFence
}

var _ core.PlanModeController = (*childPlanController)(nil)

func (c *childPlanController) Enter(ctx context.Context, reason string) error {
	return c.own.Enter(ctx, reason)
}
func (c *childPlanController) Exit(ctx context.Context, reason string) error {
	return c.own.Exit(ctx, reason)
}
func (c *childPlanController) IsActive() bool { return c.own.IsActive() || c.parent.IsActive() }
func (c *childPlanController) ShouldBlockWrite(name string) bool {
	return c.own.ShouldBlockWrite(name) || c.parent.ShouldBlockWrite(name)
}
func (c *childPlanController) DispatchFence() *tools.DispatchFence { return c.fence }
func (c *childPlanController) StartIfInactive(start func() error) (bool, error) {
	if start == nil {
		return false, tools.ErrDispatchFenceUnavailable
	}
	started := false
	err := c.fence.WithSnapshot(func(_ tools.DispatchFenceSnapshot) error {
		if c.IsActive() {
			return nil
		}
		started = true
		return start()
	})
	return started, err
}

// managedChildRegistryFenceWrapper observes only source revocation. It never
// obtains a definition, permit, executor, or run binding from the parent.
func managedChildRegistryFenceWrapper(current func() bool) tools.ToolExecutorWrapper {
	return func(next func(context.Context, tools.ToolCall) (*tools.ToolResult, error)) func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
		return func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
			if !current() {
				return nil, tools.ErrDispatchFenceChanged
			}
			return next(ctx, call)
		}
	}
}
