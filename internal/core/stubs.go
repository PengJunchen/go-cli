package core

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// This file holds the default (seed) implementations of every interface
// defined in this package, plus lightweight defaults backing the optional
// service/extension interfaces imported from other packages. Each stub returns
// benign zero values so the Registry is always fully wired, and logs a real
// slog message so SCAN-008 is satisfied. Later phases replace these stubs with
// full implementations.

// LoopAgent, AgentImpl and HarnessImpl are defined in loop.go, agent.go and
// harness.go respectively with their full implementations. This file retains
// the remaining default stubs for the other core and service interfaces.

// SessionStoreImpl is the default SessionStore stub.
type SessionStoreImpl struct{}

var _ SessionStore = (*SessionStoreImpl)(nil)

// Save logs the persisted session id.
func (SessionStoreImpl) Save(_ context.Context, session Session) error {
	slog.Info("core.session.save", "id", session.ID)
	return nil
}

// Load returns an empty session.
func (SessionStoreImpl) Load(_ context.Context, _ string) (Session, error) {
	return Session{}, nil
}

// List returns no sessions.
func (SessionStoreImpl) List(_ context.Context) ([]Session, error) {
	return []Session{}, nil
}

// SessionTreeImpl is the default SessionTree stub.
type SessionTreeImpl struct{}

var _ SessionTree = (*SessionTreeImpl)(nil)

// Create logs the new root session.
func (SessionTreeImpl) Create(_ context.Context, sessionID string) error {
	slog.Info("core.session.tree.create", "id", sessionID)
	return nil
}

// Branch logs the new branch.
func (SessionTreeImpl) Branch(_ context.Context, parentID, sessionID string) error {
	slog.Info("core.session.tree.branch", "parent", parentID, "child", sessionID)
	return nil
}

// Leaves returns no leaves.
func (SessionTreeImpl) Leaves(_ context.Context, _ string) ([]string, error) {
	return []string{}, nil
}

// ContextManagerImpl is the default ContextManager stub.
type ContextManagerImpl struct{}

var _ ContextManager = (*ContextManagerImpl)(nil)

// Build logs the session and returns an empty context.
func (ContextManagerImpl) Build(_ context.Context, sessionID string) ([]AgentMessage, error) {
	slog.Info("core.context.build", "session", sessionID)
	return []AgentMessage{}, nil
}

// UnifiedCompactor is the default Compactor stub.
type UnifiedCompactor struct{}

var _ Compactor = (*UnifiedCompactor)(nil)

// Compact logs the message count and returns the input unchanged.
func (UnifiedCompactor) Compact(_ context.Context, messages []AgentMessage, _ int) ([]AgentMessage, error) {
	slog.Info("core.compaction.compact", "count", len(messages))
	return messages, nil
}

// HeuristicTokenEstimator is the default TokenEstimator stub.
type HeuristicTokenEstimator struct{}

var _ TokenEstimator = (*HeuristicTokenEstimator)(nil)

// Estimate estimates tokens as characters divided by four.
func (HeuristicTokenEstimator) Estimate(text string) int {
	slog.Info("core.compaction.estimate", "len", len(text))
	return len(text) / 4
}

// EstimateMessages sums per-message estimates.
func (HeuristicTokenEstimator) EstimateMessages(msgs []AgentMessage) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content) / 4
	}
	return total
}

// SafetyPolicyClassifier is the default ApprovalClassifier stub.
type SafetyPolicyClassifier struct{}

var _ ApprovalClassifier = (*SafetyPolicyClassifier)(nil)

// Name returns the classifier name.
func (SafetyPolicyClassifier) Name() string { return "safety_policy" }

// Classify allows every call by default.
func (SafetyPolicyClassifier) Classify(_ context.Context, toolName string) Classification {
	slog.Info("core.approval.classify", "tool", toolName)
	return ClassificationAllow
}

// InMemoryApprovalStore is the default ApprovalStore stub.
type InMemoryApprovalStore struct{}

var _ ApprovalStore = (*InMemoryApprovalStore)(nil)

// Remember logs the decision.
func (InMemoryApprovalStore) Remember(_ context.Context, toolName string, allowed bool) error {
	slog.Info("core.approval.remember", "tool", toolName, "allowed", allowed)
	return nil
}

// IsAllowed reports that nothing is pre-approved.
func (InMemoryApprovalStore) IsAllowed(_ context.Context, _ string) bool {
	return false
}

// PluginLoaderImpl is the default PluginLoader stub.
type PluginLoaderImpl struct{}

var _ PluginLoader = (*PluginLoaderImpl)(nil)

// Load logs the path and reports plugin loading as unsupported.
func (PluginLoaderImpl) Load(_ context.Context, path string) (Extension, error) {
	slog.Info("core.plugin.load", "path", path)
	return nil, errPluginsUnsupported
}

// DefaultToolRegistry is the default tools.ToolRegistry implementation.
type DefaultToolRegistry struct{}

var _ tools.ToolRegistry = (*DefaultToolRegistry)(nil)

// Register logs the tool name.
func (DefaultToolRegistry) Register(_ context.Context, def tools.ToolDefinition) error {
	slog.Info("core.tools.register", "tool", def.Name())
	return nil
}

// Get reports the tool is unknown.
func (DefaultToolRegistry) Get(_ context.Context, _ string) (tools.ToolDefinition, error) {
	return nil, errToolUnknown
}

// List returns no tools.
func (DefaultToolRegistry) List(_ context.Context) ([]tools.ToolDefinition, error) {
	return []tools.ToolDefinition{}, nil
}

// DefaultModelProvider is the default llm.ModelProvider implementation.
type DefaultModelProvider struct{}

var _ llm.ModelProvider = (*DefaultModelProvider)(nil)

// Name returns the provider name.
func (DefaultModelProvider) Name() string { return "default" }

// Build returns a nil model and reports it as unsupported.
func (DefaultModelProvider) Build(_ context.Context, _ llm.ModelConfig) (llm.BaseChatModel, func(), error) {
	return nil, func() {}, errModelUnsupported
}

// Models returns no models.
func (DefaultModelProvider) Models() []llm.ModelInfo { return nil }

// NoopTraceExporter is the default tracing.TraceExporter implementation. It
// discards every span.
type NoopTraceExporter struct{}

var _ tracing.TraceExporter = (*NoopTraceExporter)(nil)

// ExportSpan logs the span id and returns nil.
func (NoopTraceExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	slog.Info("core.tracing.export", "span", span.SpanID())
	return nil
}

// Shutdown is a no-op.
func (NoopTraceExporter) Shutdown(_ context.Context) error { return nil }

// DefaultConfigProvider is the default extension.ConfigProvider implementation.
type DefaultConfigProvider struct{}

var _ extension.ConfigProvider = (*DefaultConfigProvider)(nil)

// Name returns the provider name.
func (DefaultConfigProvider) Name() string { return "default" }

// Load logs the key and reports config loading as unsupported.
func (DefaultConfigProvider) Load(_ context.Context, key string, _ any) error {
	slog.Info("core.config.load", "key", key)
	return errConfigUnsupported
}

// Watch reports config watching as unsupported.
func (DefaultConfigProvider) Watch(_ context.Context, _ string) (<-chan extension.ConfigChange, error) {
	return nil, errConfigUnsupported
}
