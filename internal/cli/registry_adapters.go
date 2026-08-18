package cli

import (
	"context"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// This file holds adapters that bridge the concrete service-layer
// implementations (from the approval, compaction, and llm packages) to the
// abstraction-layer interfaces stored in core.DefaultRegistry. AssembleAgent
// constructs the concrete types exactly as before; these adapters only exist so
// the assembled components can also be registered in the Registry for
// dependency-injection / future overrides. They perform no new logic beyond
// type translation.

// chatModelProvider adapts an already-built llm.BaseChatModel to the
// llm.ModelProvider interface so it can be stored in the Registry.
type chatModelProvider struct {
	name  string
	model llm.BaseChatModel
}

var _ llm.ModelProvider = (*chatModelProvider)(nil)

func (p *chatModelProvider) Name() string { return p.name }

func (p *chatModelProvider) Build(_ context.Context, _ llm.ModelConfig) (llm.BaseChatModel, func(), error) {
	return p.model, func() {}, nil
}

func (p *chatModelProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{Name: p.name}}
}

// approvalClassifierAdapter adapts an approval.ApprovalClassifier (which
// classifies a full tools.ToolCall and returns an approval.Classification) to
// the core.ApprovalClassifier interface (which classifies by tool name and
// returns a core.Classification).
type approvalClassifierAdapter struct {
	inner approval.ApprovalClassifier
}

var _ core.ApprovalClassifier = (*approvalClassifierAdapter)(nil)

func (a *approvalClassifierAdapter) Name() string { return a.inner.Name() }

func (a *approvalClassifierAdapter) Classify(ctx context.Context, toolName string) core.Classification {
	switch a.inner.Classify(ctx, tools.ToolCall{Name: toolName}) {
	case approval.Deny:
		return core.ClassificationDeny
	case approval.Ask:
		return core.ClassificationRequireApproval
	default:
		return core.ClassificationAllow
	}
}

// approvalStoreAdapter adapts an approval.ApprovalStore (keyed Get/Set of
// approval.Classification) to the core.ApprovalStore interface
// (Remember/IsAllowed by tool name).
type approvalStoreAdapter struct {
	inner approval.ApprovalStore
}

var _ core.ApprovalStore = (*approvalStoreAdapter)(nil)

func (a *approvalStoreAdapter) Remember(ctx context.Context, toolName string, allowed bool) error {
	cls := approval.Deny
	if allowed {
		cls = approval.Allow
	}
	return a.inner.Set(ctx, toolName, cls)
}

func (a *approvalStoreAdapter) IsAllowed(ctx context.Context, toolName string) bool {
	cls, ok, err := a.inner.Get(ctx, toolName)
	if err != nil || !ok {
		return false
	}
	return cls == approval.Allow
}

// tokenEstimatorAdapter adapts a compaction.TokenEstimator (Estimate returns
// (int, error)) to the core.TokenEstimator interface (Estimate returns int,
// plus EstimateMessages).
type tokenEstimatorAdapter struct {
	inner compaction.TokenEstimator
}

var _ core.TokenEstimator = (*tokenEstimatorAdapter)(nil)

func (e *tokenEstimatorAdapter) Estimate(text string) int {
	n, _ := e.inner.Estimate(text) //nolint:errcheck
	return n
}

func (e *tokenEstimatorAdapter) EstimateMessages(msgs []core.AgentMessage) int {
	total := 0
	for _, m := range msgs {
		total += e.Estimate(m.Content)
	}
	return total
}

// compactorAdapter adapts a compaction.Compactor (TurnItem-based, requires an
// estimator argument) to the core.Compactor interface (AgentMessage-based, no
// estimator argument). The conversion mirrors newCompactionHook.
type compactorAdapter struct {
	inner     compaction.Compactor
	estimator compaction.TokenEstimator
}

var _ core.Compactor = (*compactorAdapter)(nil)

func (a *compactorAdapter) Compact(ctx context.Context, messages []core.AgentMessage, maxTokens int) ([]core.AgentMessage, error) {
	if len(messages) == 0 {
		return messages, nil
	}
	items := make([]compaction.TurnItem, len(messages))
	for i, msg := range messages {
		items[i] = compaction.TurnItem{
			ID:            fmt.Sprintf("msg-%d", i),
			Role:          msg.Role,
			Content:       msg.Content,
			ContentBlocks: msg.ContentBlocks,
			ToolCalls:     msg.ToolCalls,
			ToolCallID:    msg.ToolCallID,
			ToolName:      msg.ToolName,
		}
	}
	compacted, err := a.inner.Compact(ctx, items, maxTokens, a.estimator)
	if err != nil {
		return nil, fmt.Errorf("registry compactor adapter: %w", err)
	}
	result := make([]core.AgentMessage, len(compacted))
	for i, item := range compacted {
		result[i] = core.AgentMessage{
			Role:          item.Role,
			Content:       item.Content,
			ContentBlocks: item.ContentBlocks,
			ToolCalls:     item.ToolCalls,
			ToolCallID:    item.ToolCallID,
			ToolName:      item.ToolName,
		}
	}
	return result, nil
}
