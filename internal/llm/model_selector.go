package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
)

// ModelBuilder builds a chat model from a model name. It encapsulates
// provider-specific construction logic (BaseURL, APIKey, etc) so the
// selector can switch models at runtime without importing higher-level
// packages.
type ModelBuilder func(ctx context.Context, modelName string) (BaseChatModel, func(), error)

// ModelLister returns the list of models available from the current provider.
type ModelLister func() []ModelInfo

// ModelSpecBuilder builds a model from its provider-scoped identity.
type ModelSpecBuilder func(ctx context.Context, model ModelSpec) (BaseChatModel, func(), error)

// ModelSpecBuildResult carries a built model and the non-sensitive UI metadata
// resolved for that exact provider/model identity.
type ModelSpecBuildResult struct {
	Model         BaseChatModel
	Cleanup       func()
	ContextWindow int
}

// ModelSpecResultBuilder builds a model and returns its UI metadata.
type ModelSpecResultBuilder func(ctx context.Context, model ModelSpec) (ModelSpecBuildResult, error)

// ModelSpecLister returns provider-scoped model entries from the active catalog.
type ModelSpecLister func() []ModelSpec

// ModelSwitchAdmission borrows the caller's run-admission slot while a built
// candidate is published and the previous generation is retired.
type ModelSwitchAdmission func(context.Context) (func(), error)

type modelGeneration struct {
	model            BaseChatModel
	providerID       string
	modelName        string
	contextWindow    int
	cleanup          func()
	cleanupAttempted bool
	cleanupErr       error
}

type modelGenerationDisposer struct {
	mu          sync.Mutex
	finished    chan struct{}
	generations []*modelGeneration
	initialErr  error
	err         error
}

func newModelGenerationDisposer(generations []*modelGeneration, initialErr error) *modelGenerationDisposer {
	return &modelGenerationDisposer{generations: generations, initialErr: initialErr}
}

func (d *modelGenerationDisposer) run() error {
	d.mu.Lock()
	if d.finished != nil {
		finished := d.finished
		d.mu.Unlock()
		<-finished
		d.mu.Lock()
		err := d.err
		d.mu.Unlock()
		return err
	}
	d.finished = make(chan struct{})
	finished := d.finished
	generations := d.generations
	d.mu.Unlock()

	disposeErr := d.initialErr
	retained := make([]*modelGeneration, 0, len(generations))
	for _, generation := range generations {
		if generation == nil {
			continue
		}
		if generation.cleanupAttempted {
			disposeErr = errors.Join(disposeErr, generation.cleanupErr)
			if generation.cleanupErr != nil {
				retained = append(retained, generation)
			} else {
				generation.model = nil
				generation.cleanup = nil
			}
			continue
		}
		if generation.cleanup == nil {
			generation.model = nil
			continue
		}
		generation.cleanupAttempted = true
		if invokeCleanup(generation.cleanup) {
			generation.cleanupErr = ErrModelCleanupPanicked
			disposeErr = errors.Join(disposeErr, generation.cleanupErr)
			retained = append(retained, generation)
		} else {
			generation.model = nil
			generation.cleanup = nil
		}
	}

	d.mu.Lock()
	d.generations = retained
	d.err = disposeErr
	close(finished)
	d.mu.Unlock()
	return disposeErr
}

var ErrModelSwitchBusy = errors.New("llm: model switch is busy")
var ErrModelSelectorClosed = errors.New("llm: model selector is closed")
var ErrModelSwitchAdmissionClosed = errors.New("llm: model switch admission is closed")
var ErrModelSwitchAdmissionMissingRelease = errors.New("llm: model switch admission returned no release handle")
var ErrModelSelectorFailed = errors.New("llm: model selector failed closed")
var ErrModelBuilderReturnedNil = errors.New("llm: model builder returned nil model")
var ErrModelSwitchCallbackPanicked = errors.New("llm: model switch callback panicked")
var ErrModelCleanupPanicked = errors.New("llm: model cleanup panicked")
var ErrModelSelectorFailureHandlerPanicked = errors.New("llm: selector failure handler panicked")
var ErrModelBuilderPanicked = errors.New("llm: model builder panicked")
var ErrModelSwitchAdmissionPanicked = errors.New("llm: model switch admission panicked")
var ErrModelSwitchAdmissionReleasePanicked = errors.New("llm: model switch admission release panicked")

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
	// modelSpecBuilder and modelSpecLister preserve provider identity for
	// catalog-based switching while the name-only API remains available.
	modelSpecBuilder       ModelSpecBuilder
	modelSpecLister        ModelSpecLister
	modelSpecResultBuilder ModelSpecResultBuilder
	switchAdmission        ModelSwitchAdmission
	failureHandler         func()
	// onModelSwitched is called after a successful model switch so callers
	// (e.g. the LoopAgent) can update their reference to the primary model.
	onModelSwitched func(BaseChatModel)

	// switchMu protects the published model identity, lifecycle state, and
	// generation ownership. It is never held across user callbacks.
	switchMu sync.Mutex
	// mutationGate serializes builders, publication, callbacks, and retirement
	// cleanup. Mutations acquire it non-blockingly; shutdown may wait with ctx.
	mutationGate         chan struct{}
	active               *modelGeneration
	retired              []*modelGeneration
	closing              bool
	detached             bool
	disposer             *modelGenerationDisposer
	failure              error
	failureNotified      bool
	unprovenBuildFailure error
}

// Compile-time assertion that DefaultModelSelector satisfies ModelSelector.
var _ ModelSelector = (*DefaultModelSelector)(nil)

// NewDefaultModelSelector creates a ModelSelector with the given primary and
// small models. When small is nil, every task type uses the primary model.
func NewDefaultModelSelector(primary, small BaseChatModel) *DefaultModelSelector {
	return &DefaultModelSelector{
		primary:      primary,
		small:        small,
		active:       &modelGeneration{model: primary},
		mutationGate: make(chan struct{}, 1),
	}
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
	s.switchMu.Lock()
	s.primaryProvider = primaryProvider
	s.primaryName = primaryName
	s.smallProvider = smallProvider
	s.smallName = smallName
	if s.active != nil {
		s.active.providerID = primaryProvider
		s.active.modelName = primaryName
	}
	s.switchMu.Unlock()
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

// WithModelSpecBuilder sets the provider-scoped model builder used by
// SwitchModel and SwitchModelSpec when available.
func (s *DefaultModelSelector) WithModelSpecBuilder(builder ModelSpecBuilder) *DefaultModelSelector {
	s.modelSpecBuilder = builder
	return s
}

// WithModelSpecResultBuilder sets an additive provider-scoped builder that
// returns the active model's non-sensitive UI metadata. It takes precedence
// over the legacy model-spec builder when both are configured.
func (s *DefaultModelSelector) WithModelSpecResultBuilder(builder ModelSpecResultBuilder) *DefaultModelSelector {
	s.modelSpecResultBuilder = builder
	return s
}

// WithPrimaryModelContextWindow records the initial model's known context
// window. Non-positive values remain unknown.
func (s *DefaultModelSelector) WithPrimaryModelContextWindow(contextWindow int) *DefaultModelSelector {
	if contextWindow < 0 {
		contextWindow = 0
	}
	s.switchMu.Lock()
	if s.active != nil {
		s.active.contextWindow = contextWindow
	}
	s.switchMu.Unlock()
	return s
}

// WithModelSpecLister sets the provider-scoped model catalog used by
// AvailableModelSpecs.
func (s *DefaultModelSelector) WithModelSpecLister(lister ModelSpecLister) *DefaultModelSelector {
	s.modelSpecLister = lister
	return s
}

// WithPrimaryCleanup transfers cleanup ownership of the initial primary model
// to the selector's generation lifecycle.
func (s *DefaultModelSelector) WithPrimaryCleanup(cleanup func()) *DefaultModelSelector {
	s.switchMu.Lock()
	if s.active != nil {
		s.active.cleanup = cleanup
	}
	s.switchMu.Unlock()
	return s
}

// WithModelSwitchAdmission borrows shared run admission after building a
// candidate and holds it through publication, callback, and retirement.
func (s *DefaultModelSelector) WithModelSwitchAdmission(admission ModelSwitchAdmission) *DefaultModelSelector {
	s.switchAdmission = admission
	return s
}

// WithModelSelectorFailureHandler closes shared run admission after an
// unrecoverable callback or cleanup panic. The handler runs without state lock.
func (s *DefaultModelSelector) WithModelSelectorFailureHandler(handler func()) *DefaultModelSelector {
	s.failureHandler = handler
	return s
}

// Failure reports a sanitized fail-closed reason, if the selector is poisoned.
func (s *DefaultModelSelector) Failure() error {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	return s.failure
}

// SealAndDetach permanently rejects future switches, then waits for any
// in-flight switch to leave its mutation gate before detaching owned model
// generations. A timed-out caller leaves the selector sealed and may retry.
func (s *DefaultModelSelector) SealAndDetach(ctx context.Context) (func() error, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.switchMu.Lock()
	s.closing = true
	if s.detached {
		disposer := s.disposer
		failure := s.failure
		s.switchMu.Unlock()
		return disposer.run, failure
	}
	s.switchMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.mutationGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		s.endMutation()
		return nil, err
	}
	defer s.endMutation()

	s.switchMu.Lock()
	if s.detached {
		disposer := s.disposer
		failure := s.failure
		s.switchMu.Unlock()
		return disposer.run, failure
	}

	generations := make([]*modelGeneration, 0, len(s.retired)+1)
	if s.active != nil {
		generations = append(generations, s.active)
	}
	generations = append(generations, s.retired...)
	s.active = nil
	s.retired = nil
	s.primary = nil
	s.primaryProvider = ""
	s.primaryName = ""
	s.detached = true
	s.disposer = newModelGenerationDisposer(generations, s.unprovenBuildFailure)
	disposer := s.disposer
	failure := s.failure
	s.switchMu.Unlock()

	return disposer.run, failure
}

// WithModelSwitchCallback sets a callback invoked after a successful model
// switch. The callback receives the new primary model so callers (e.g. the
// LoopAgent) can update their reference.
func (s *DefaultModelSelector) WithModelSwitchCallback(cb func(BaseChatModel)) *DefaultModelSelector {
	s.onModelSwitched = cb
	return s
}

type modelBuildPlan struct {
	providerID     string
	modelName      string
	providerScoped bool
	build          func(context.Context) (ModelSpecBuildResult, error)
}

// ActiveModelSnapshot exposes the active provider/model identity and context
// window needed by user-facing status and cost displays. A zero window means
// the provider metadata did not establish the model's capacity.
type ActiveModelSnapshot struct {
	ProviderID    string
	ModelID       string
	ContextWindow int
}

// SwitchModel replaces the primary model at runtime. Model construction and
// publication are one non-queued mutation, so a concurrent switch is rejected
// before it can construct a candidate.
func (s *DefaultModelSelector) SwitchModel(ctx context.Context, modelName string) error {
	return s.switchPrimary(ctx, func() (modelBuildPlan, error) {
		if s.modelSpecResultBuilder != nil || s.modelSpecBuilder != nil {
			current := s.currentModelSpec()
			model := ModelSpec{
				ProviderID: current.ProviderID,
				ModelID:    strings.TrimSpace(modelName),
			}
			model.Info = ModelInfo{}
			model.Known = false
			if model.ProviderID == "" || model.ModelID == "" {
				return modelBuildPlan{}, fmt.Errorf("llm: provider and model identities are required")
			}
			resultBuilder := s.modelSpecResultBuilder
			legacyBuilder := s.modelSpecBuilder
			return modelBuildPlan{
				providerID:     model.ProviderID,
				modelName:      model.ModelID,
				providerScoped: true,
				build: func(ctx context.Context) (ModelSpecBuildResult, error) {
					if resultBuilder != nil {
						return resultBuilder(ctx, model)
					}
					built, cleanup, err := legacyBuilder(ctx, model)
					return ModelSpecBuildResult{Model: built, Cleanup: cleanup}, err
				},
			}, nil
		}
		if s.modelBuilder == nil {
			return modelBuildPlan{}, fmt.Errorf("llm: model switching not configured")
		}
		current := s.currentModelSpec()
		builder := s.modelBuilder
		return modelBuildPlan{
			providerID: current.ProviderID,
			modelName:  modelName,
			build: func(ctx context.Context) (ModelSpecBuildResult, error) {
				built, cleanup, err := builder(ctx, modelName)
				return ModelSpecBuildResult{Model: built, Cleanup: cleanup}, err
			},
		}, nil
	})
}

// SwitchModelSpec replaces the primary model using an explicit provider/model
// pair. Metadata supplied by the caller is ignored; the assembly builder
// resolves the identity against its provider and credential snapshot.
func (s *DefaultModelSelector) SwitchModelSpec(ctx context.Context, model ModelSpec) error {
	return s.switchPrimary(ctx, func() (modelBuildPlan, error) {
		if s.modelSpecResultBuilder == nil && s.modelSpecBuilder == nil {
			return modelBuildPlan{}, fmt.Errorf("llm: provider-scoped model switching not configured")
		}
		model.ProviderID = canonicalProviderID(model.ProviderID)
		model.ModelID = strings.TrimSpace(model.ModelID)
		model.Info = ModelInfo{}
		model.Known = false
		if model.ProviderID == "" || model.ModelID == "" {
			return modelBuildPlan{}, fmt.Errorf("llm: provider and model identities are required")
		}
		resultBuilder := s.modelSpecResultBuilder
		legacyBuilder := s.modelSpecBuilder
		return modelBuildPlan{
			providerID:     model.ProviderID,
			modelName:      model.ModelID,
			providerScoped: true,
			build: func(ctx context.Context) (ModelSpecBuildResult, error) {
				if resultBuilder != nil {
					return resultBuilder(ctx, model)
				}
				built, cleanup, err := legacyBuilder(ctx, model)
				return ModelSpecBuildResult{Model: built, Cleanup: cleanup}, err
			},
		}, nil
	})
}

func (s *DefaultModelSelector) switchPrimary(ctx context.Context, prepare func() (modelBuildPlan, error)) (resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.beginMutation(ctx); err != nil {
		return err
	}
	defer s.endMutation()

	var admissionRelease func()
	defer func() {
		if admissionRelease == nil {
			return
		}
		if invokeRelease(admissionRelease) {
			resultErr = errors.Join(resultErr, s.recordFailure(ErrModelSwitchAdmissionReleasePanicked))
		}
	}()

	plan, err := prepare()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	built, builderPanicked, buildErr := invokeModelBuilder(ctx, plan.build)
	candidate := &modelGeneration{
		model:         built.Model,
		providerID:    plan.providerID,
		modelName:     plan.modelName,
		contextWindow: max(0, built.ContextWindow),
		cleanup:       built.Cleanup,
	}
	if builderPanicked {
		s.switchMu.Lock()
		s.unprovenBuildFailure = ErrModelBuilderPanicked
		s.switchMu.Unlock()
		return s.recordFailure(ErrModelBuilderPanicked)
	}
	if buildErr != nil {
		wrapped := fmt.Errorf("llm: build model %q: %w", plan.modelName, buildErr)
		if plan.providerScoped {
			wrapped = fmt.Errorf("llm: build model %q for provider %q: %w", plan.modelName, plan.providerID, buildErr)
		}
		return errors.Join(wrapped, s.disposeUnpublished(candidate))
	}
	if isNilChatModel(built.Model) {
		return errors.Join(ErrModelBuilderReturnedNil, s.disposeUnpublished(candidate))
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, s.disposeUnpublished(candidate))
	}
	if err := s.mutationStateError(); err != nil {
		return errors.Join(err, s.disposeUnpublished(candidate))
	}

	if s.switchAdmission != nil {
		release, admissionPanicked, admissionErr := invokeAdmission(ctx, s.switchAdmission)
		if admissionPanicked {
			disposeErr := s.disposeUnpublished(candidate)
			failure := s.recordFailure(ErrModelSwitchAdmissionPanicked)
			return errors.Join(failure, disposeErr)
		}
		if admissionErr != nil {
			admissionRelease = release
			return errors.Join(admissionErr, s.disposeUnpublished(candidate))
		}
		if release == nil {
			return errors.Join(ErrModelSwitchAdmissionMissingRelease, s.disposeUnpublished(candidate))
		}
		admissionRelease = release
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, s.disposeUnpublished(candidate))
	}
	if err := s.mutationStateError(); err != nil {
		return errors.Join(err, s.disposeUnpublished(candidate))
	}

	s.switchMu.Lock()
	if err := s.mutationStateErrorLocked(); err != nil {
		s.switchMu.Unlock()
		return errors.Join(err, s.disposeUnpublished(candidate))
	}
	previous := s.active
	s.active = candidate
	s.primary = candidate.model
	s.primaryProvider = candidate.providerID
	s.primaryName = candidate.modelName
	if previous != nil {
		s.retired = append(s.retired, previous)
	}
	callback := s.onModelSwitched
	s.switchMu.Unlock()

	if callback != nil && invokeSwitchCallback(callback, candidate.model) {
		return s.recordFailure(ErrModelSwitchCallbackPanicked)
	}
	if previous != nil {
		if err := s.disposeRetired(previous); err != nil {
			return err
		}
	}
	slog.Info("llm_model_switched", "provider", plan.providerID, "model", plan.modelName)
	return nil
}

func (s *DefaultModelSelector) beginMutation(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.mutationStateError(); err != nil {
		return err
	}
	select {
	case s.mutationGate <- struct{}{}:
	default:
		return ErrModelSwitchBusy
	}
	if err := s.mutationStateError(); err != nil {
		s.endMutation()
		return err
	}
	if err := ctx.Err(); err != nil {
		s.endMutation()
		return err
	}
	return nil
}

func (s *DefaultModelSelector) endMutation() {
	<-s.mutationGate
}

func (s *DefaultModelSelector) mutationStateError() error {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	return s.mutationStateErrorLocked()
}

func (s *DefaultModelSelector) mutationStateErrorLocked() error {
	if s.failure != nil {
		return s.failure
	}
	if s.closing {
		return ErrModelSelectorClosed
	}
	return nil
}

func (s *DefaultModelSelector) currentModelSpec() ModelSpec {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	return ModelSpec{
		ProviderID: canonicalProviderID(s.primaryProvider),
		ModelID:    strings.TrimSpace(s.primaryName),
	}
}

func (s *DefaultModelSelector) recordFailure(cause error) error {
	s.switchMu.Lock()
	if s.failure == nil {
		s.failure = errors.Join(ErrModelSelectorFailed, cause)
	} else if cause != nil && !errors.Is(s.failure, cause) {
		s.failure = errors.Join(s.failure, cause)
	}
	failure := s.failure
	callHandler := !s.failureNotified
	if callHandler {
		s.failureNotified = true
	}
	handler := s.failureHandler
	s.switchMu.Unlock()

	if callHandler && handler != nil && invokeFailureHandler(handler) {
		s.switchMu.Lock()
		s.failure = errors.Join(s.failure, ErrModelSelectorFailureHandlerPanicked)
		failure = s.failure
		s.switchMu.Unlock()
	}
	return failure
}

func (s *DefaultModelSelector) disposeUnpublished(candidate *modelGeneration) error {
	if candidate == nil || candidate.cleanup == nil {
		return nil
	}
	candidate.cleanupAttempted = true
	if !invokeCleanup(candidate.cleanup) {
		return nil
	}
	candidate.cleanupErr = ErrModelCleanupPanicked
	s.switchMu.Lock()
	s.retired = append(s.retired, candidate)
	s.switchMu.Unlock()
	return s.recordFailure(ErrModelCleanupPanicked)
}

func (s *DefaultModelSelector) disposeRetired(generation *modelGeneration) error {
	if generation == nil || generation.cleanup == nil {
		s.removeRetired(generation)
		return nil
	}
	s.switchMu.Lock()
	if generation.cleanupAttempted {
		s.switchMu.Unlock()
		return nil
	}
	generation.cleanupAttempted = true
	s.switchMu.Unlock()

	if invokeCleanup(generation.cleanup) {
		generation.cleanupErr = ErrModelCleanupPanicked
		return s.recordFailure(ErrModelCleanupPanicked)
	}
	s.removeRetired(generation)
	return nil
}

func (s *DefaultModelSelector) removeRetired(target *modelGeneration) {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	for i, generation := range s.retired {
		if generation == target {
			copy(s.retired[i:], s.retired[i+1:])
			s.retired[len(s.retired)-1] = nil
			s.retired = s.retired[:len(s.retired)-1]
			return
		}
	}
}

func invokeModelBuilder(ctx context.Context, build func(context.Context) (ModelSpecBuildResult, error)) (result ModelSpecBuildResult, panicked bool, err error) {
	defer func() {
		if recover() != nil {
			result = ModelSpecBuildResult{}
			panicked = true
		}
	}()
	result, err = build(ctx)
	return
}

func isNilChatModel(model BaseChatModel) bool {
	if model == nil {
		return true
	}
	value := reflect.ValueOf(model)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func invokeAdmission(ctx context.Context, admission ModelSwitchAdmission) (release func(), panicked bool, err error) {
	defer func() {
		if recover() != nil {
			release = nil
			err = nil
			panicked = true
		}
	}()
	release, err = admission(ctx)
	return
}

func invokeRelease(release func()) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	release()
	return
}

func invokeSwitchCallback(callback func(BaseChatModel), model BaseChatModel) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	callback(model)
	return
}

func invokeCleanup(cleanup func()) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	cleanup()
	return
}

func invokeFailureHandler(handler func()) (panicked bool) {
	defer func() { panicked = recover() != nil }()
	handler()
	return
}

// AvailableModels returns the list of models available from the current
// provider. It returns nil when no ModelLister is configured.
func (s *DefaultModelSelector) AvailableModels() []ModelInfo {
	if s.modelLister != nil {
		return s.modelLister()
	}
	models := s.AvailableModelSpecs()
	if len(models) == 0 {
		return nil
	}
	result := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		result = append(result, model.Info)
	}
	return result
}

// AvailableModelSpecs returns a copy of the provider-scoped model catalog.
func (s *DefaultModelSelector) AvailableModelSpecs() []ModelSpec {
	if s.modelSpecLister == nil {
		return nil
	}
	models := s.modelSpecLister()
	if models == nil {
		return nil
	}
	result := make([]ModelSpec, len(models))
	copy(result, models)
	return result
}

// CurrentModelSpec returns the current provider/model identity.
func (s *DefaultModelSelector) CurrentModelSpec() ModelSpec {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	return ModelSpec{
		ProviderID: canonicalProviderID(s.primaryProvider),
		ModelID:    strings.TrimSpace(s.primaryName),
	}
}

// ActiveModelSnapshot returns safe active metadata for UI status displays. It
// does not expose provider endpoints, credentials, or auth configuration.
func (s *DefaultModelSelector) ActiveModelSnapshot() ActiveModelSnapshot {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	snapshot := ActiveModelSnapshot{
		ProviderID: canonicalProviderID(s.primaryProvider),
		ModelID:    strings.TrimSpace(s.primaryName),
	}
	if s.active != nil {
		snapshot.ContextWindow = s.active.contextWindow
	}
	return snapshot
}

// PrimaryModelName returns the name of the current primary model.
func (s *DefaultModelSelector) PrimaryModelName() string {
	s.switchMu.Lock()
	defer s.switchMu.Unlock()
	return s.primaryName
}

// CloseReleasesCleanups is the legacy, error-discarding selector close helper.
// New callers should use SealAndDetach so cleanup failures remain observable.
func (s *DefaultModelSelector) CloseReleasesCleanups() {
	dispose, _ := s.SealAndDetach(context.Background())
	if dispose != nil {
		_ = dispose()
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
	primaryProvider := s.primaryProvider
	s.switchMu.Unlock()
	if candidate != primary {
		return candidate
	}

	limit := s.modelInputLimit(ctx, primaryProvider, primaryName)
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
