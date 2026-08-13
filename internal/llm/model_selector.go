package llm

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// ModelBuilder builds a chat model from a model name. It encapsulates
// provider-specific construction logic (BaseURL, APIKey, etc) so the
// selector can switch models at runtime without importing higher-level
// packages.
type ModelBuilder func(ctx context.Context, modelName string) (BaseChatModel, func(), error)

// ModelLister returns the list of models available from the current provider.
type ModelLister func() []ModelInfo

// DefaultModelSelector is a ModelSelector that holds a primary model for full
// chat turns and an optional small model for lightweight tasks (summary, title,
// extraction). When the small model is nil, all task types route to the
// primary model.
//
// When a ModelRegistry is configured (via WithModelRegistry), the selector can
// query it for model limits (InputTokenLimit / ContextWindow) to make
// token-aware routing decisions via SelectModelWithTokens. The base SelectModel
// method always uses the static task-type routing and never contacts the
// registry, preserving backward compatibility.
//
// When a ModelBuilder is configured (via WithModelBuilder), the selector can
// switch the primary model at runtime via SwitchModel without restarting the
// process. An optional ModelLister (via WithModelLister) provides the list of
// available models for display.
type DefaultModelSelector struct {
	primary BaseChatModel
	small   BaseChatModel

	// registry is an optional external model metadata source. When nil,
	// SelectModelWithTokens falls back to static task-type routing without
	// any token-limit checking.
	registry ModelRegistry
	// primaryProvider/primaryName identify the primary model in the registry.
	primaryProvider string
	primaryName     string
	// smallProvider/smallName identify the small model in the registry.
	smallProvider string
	smallName     string

	// modelBuilder builds a new chat model from a model name for runtime
	// switching. When nil, SwitchModel returns an error.
	modelBuilder ModelBuilder
	// modelLister returns the list of models available from the current
	// provider. When nil, AvailableModels returns nil.
	modelLister ModelLister
	// onModelSwitched is called after a successful model switch so callers
	// (e.g. the LoopAgent) can update their reference to the primary model.
	onModelSwitched func(BaseChatModel)

	// switchMu protects the primary model and the cleanup list during
	// runtime model switching.
	switchMu sync.Mutex
	// cleanups holds cleanup functions for switched-out models so they can
	// be released when the selector is disposed or a new switch replaces
	// them.
	cleanups []func()
}

// Compile-time assertion that DefaultModelSelector satisfies ModelSelector.
var _ ModelSelector = (*DefaultModelSelector)(nil)

// NewDefaultModelSelector creates a ModelSelector with the given primary and
// small models. When small is nil, every task type uses the primary model.
func NewDefaultModelSelector(primary, small BaseChatModel) *DefaultModelSelector {
	return &DefaultModelSelector{primary: primary, small: small}
}

// WithModelRegistry sets the external ModelRegistry used for token-aware model
// selection. When set, SelectModelWithTokens queries the registry for model
// limits (InputTokenLimit / ContextWindow) to decide whether the estimated
// token count fits within the primary model's capacity.
func (s *DefaultModelSelector) WithModelRegistry(reg ModelRegistry) *DefaultModelSelector {
	s.registry = reg
	return s
}

// WithModelNames sets the provider and model identifiers used to look up
// metadata in the registry. These must match the keys the registry uses
// (e.g. the provider ID and model ID from models.dev).
func (s *DefaultModelSelector) WithModelNames(primaryProvider, primaryName, smallProvider, smallName string) *DefaultModelSelector {
	s.primaryProvider = primaryProvider
	s.primaryName = primaryName
	s.smallProvider = smallProvider
	s.smallName = smallName
	return s
}

// WithModelBuilder sets the function used to build a new chat model from a
// model name at runtime. When set, SwitchModel can be used to switch the
// primary model without restarting the process.
func (s *DefaultModelSelector) WithModelBuilder(builder ModelBuilder) *DefaultModelSelector {
	s.modelBuilder = builder
	return s
}

// WithModelLister sets the function used to list available models from the
// current provider. When set, AvailableModels returns the list; otherwise it
// returns nil.
func (s *DefaultModelSelector) WithModelLister(lister ModelLister) *DefaultModelSelector {
	s.modelLister = lister
	return s
}

// WithModelSwitchCallback sets a callback invoked after a successful model
// switch. The callback receives the new primary model so callers (e.g. the
// LoopAgent) can update their reference.
func (s *DefaultModelSelector) WithModelSwitchCallback(cb func(BaseChatModel)) *DefaultModelSelector {
	s.onModelSwitched = cb
	return s
}

// SwitchModel replaces the primary model at runtime. It calls the configured
// ModelBuilder to construct a new chat model from modelName, updates the
// selector's primary model and name, and invokes the onModelSwitched callback
// so downstream consumers (e.g. the LoopAgent) pick up the new model.
func (s *DefaultModelSelector) SwitchModel(ctx context.Context, modelName string) error {
	if s.modelBuilder == nil {
		return fmt.Errorf("llm: model switching not configured")
	}
	newModel, cleanup, err := s.modelBuilder(ctx, modelName)
	if err != nil {
		return fmt.Errorf("llm: build model %q: %w", modelName, err)
	}

	s.switchMu.Lock()
	oldCleanup := s.cleanups
	s.cleanups = nil
	if cleanup != nil {
		s.cleanups = append(s.cleanups, cleanup)
	}
	s.primary = newModel
	s.primaryName = modelName
	cb := s.onModelSwitched
	s.switchMu.Unlock()

	// Release the previous model's resources outside the lock to avoid
	// blocking concurrent SelectModel calls during cleanup.
	for _, fn := range oldCleanup {
		fn()
	}

	if cb != nil {
		cb(newModel)
	}
	slog.Info("llm_model_switched", "model", modelName)
	return nil
}

// AvailableModels returns the list of models available from the current
// provider. It returns nil when no ModelLister is configured.
func (s *DefaultModelSelector) AvailableModels() []ModelInfo {
	if s.modelLister == nil {
		return nil
	}
	return s.modelLister()
}

// PrimaryModelName returns the name of the current primary model.
func (s *DefaultModelSelector) PrimaryModelName() string {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	return s.primaryName
}

// CloseReleasesCleanups runs any pending cleanup functions for switched-out
// models. Call this when the selector is being disposed.
func (s *DefaultModelSelector) CloseReleasesCleanups() {
	s.switchMu.Lock()
	cleanups := s.cleanups
	s.cleanups = nil
	s.switchMu.Unlock()
	for _, fn := range cleanups {
		fn()
	}
}

// SelectModel returns the model appropriate for the given task type. Lightweight
// tasks (summary, title, extraction) use the small model when available; chat
// and any unrecognized type use the primary model.
//
// This method never contacts the registry. For token-aware selection that
// queries the registry for model limits, use SelectModelWithTokens.
func (s *DefaultModelSelector) SelectModel(taskType TaskType) BaseChatModel {
	if s.small != nil {
		switch taskType {
		case TaskTypeSummary, TaskTypeTitle, TaskTypeExtraction:
			return s.small
		}
	}
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	return s.primary
}

// SelectModelWithTokens returns the model appropriate for the given task type,
// taking the estimated token count into account when a registry is configured.
//
// When the registry is nil or the estimated tokens are zero/negative, the
// method falls back to the same static task-type routing as SelectModel.
//
// When the registry is available and the primary model is the candidate, the
// method queries the registry for the primary model's InputTokenLimit (falling
// back to ContextWindow). If the estimated tokens exceed the limit and a small
// model is available, the small model is returned. If no small model is
// available, the primary model is returned with an slog.Warn indicating the
// context overflow.
func (s *DefaultModelSelector) SelectModelWithTokens(ctx context.Context, taskType TaskType, estimatedTokens int) BaseChatModel {
	candidate := s.SelectModel(taskType)

	// No registry or no token estimate: keep the static routing result.
	if s.registry == nil || estimatedTokens <= 0 {
		return candidate
	}

	// Only check token limits when the candidate is the primary model.
	s.switchMu.Lock()
	primary := s.primary
	primaryName := s.primaryName
	s.switchMu.Unlock()
	if candidate != primary {
		return candidate
	}

	limit := s.modelInputLimit(ctx, s.primaryProvider, primaryName)
	if limit <= 0 || estimatedTokens <= limit {
		return candidate
	}

	// Primary model overflow: switch to small if available.
	if s.small != nil {
		slog.Warn("model_selector_primary_overflow_switching_to_small",
			"estimated_tokens", estimatedTokens,
			"primary_limit", limit,
			"primary_model", primaryName,
		)
		return s.small
	}

	slog.Warn("model_selector_primary_overflow_no_small",
		"estimated_tokens", estimatedTokens,
		"primary_limit", limit,
		"primary_model", primaryName,
	)
	return primary
}

// modelInputLimit queries the registry for the model's InputTokenLimit,
// falling back to ContextWindow when InputTokenLimit is not reported.
func (s *DefaultModelSelector) modelInputLimit(ctx context.Context, provider, model string) int {
	if s.registry == nil || provider == "" || model == "" {
		return 0
	}
	info, ok := s.registry.Lookup(ctx, provider, model)
	if !ok {
		return 0
	}
	if info.InputTokenLimit > 0 {
		return info.InputTokenLimit
	}
	return info.ContextWindow
}

// PrimaryModel returns the primary (chat) model.
func (s *DefaultModelSelector) PrimaryModel() BaseChatModel {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	return s.primary
}

// SmallModel returns the small model, or nil when not configured.
func (s *DefaultModelSelector) SmallModel() BaseChatModel { return s.small }

// HasSmallModel reports whether a small model is configured.
func (s *DefaultModelSelector) HasSmallModel() bool { return s.small != nil }

// Registry returns the configured ModelRegistry, or nil when none is set.
func (s *DefaultModelSelector) Registry() ModelRegistry { return s.registry }
