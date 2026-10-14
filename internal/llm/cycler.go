// Package llm cycler.go — ModelCycler rotates model selection across multiple
// providers using configurable strategies (round-robin, weighted, cost
// priority). It implements ModelMiddleware so it can be registered in a
// ModelMiddlewareChain.
package llm

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
)

// sessionIDKey is the context key used to carry a session identifier for
// ModelCycler session affinity.
type sessionIDKey struct{}

// WithSessionID returns a copy of ctx that carries the given sessionID. The
// sessionID is used by ModelCycler (when SessionAffinity is enabled) to pin a
// conversation to a consistent model.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// sessionIDFromContext extracts the session ID from ctx, or returns "" when no
// session ID is present.
func sessionIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sessionIDKey{}).(string); ok {
		return v
	}
	return ""
}

// Strategy constants for ModelCyclerConfig.Strategy.
const (
	// StrategyRoundRobin cycles through models in order.
	StrategyRoundRobin = "round_robin"
	// StrategyWeighted selects models proportional to their Weight.
	StrategyWeighted = "weighted"
	// StrategyCostPriority selects the model with the lowest cost (highest
	// weight) first.
	StrategyCostPriority = "cost_priority"
)

// ModelCyclerConfig configures model rotation.
type ModelCyclerConfig struct {
	Models          []ModelEntry
	Strategy        string // round_robin | weighted | cost_priority
	SessionAffinity bool   // same session keeps same model
}

// ModelEntry represents a model in the rotation pool.
type ModelEntry struct {
	Provider string // openai | claude | gemini
	Model    string // model name
	Weight   int    // weight for weighted strategy
	// TaskType optionally tags this model for a specific task type
	// (chat, summary, title, extraction). When non-empty, the cycler
	// prefers this model for calls with a matching task type from context.
	TaskType TaskType
}

// ModelCycler implements model rotation across multiple providers. It is
// safe for concurrent use.
type ModelCycler struct {
	config            ModelCyclerConfig
	registry          *ProviderRegistry
	resolvedModels    []ResolvedModel
	counter           atomic.Int64
	mu                sync.Mutex
	selectionRevision uint64
	// sessions maps sessionID -> model index when SessionAffinity is enabled.
	sessions map[string]int
	// maxSessions bounds the sessions map to prevent unbounded memory growth.
	// When the limit is reached, the oldest entries are evicted.
	maxSessions int
}

// Compile-time assertion that ModelCycler satisfies ModelMiddleware.
var _ ModelMiddleware = (*ModelCycler)(nil)

// NewModelCycler creates a ModelCycler with the given configuration.
func NewModelCycler(config ModelCyclerConfig) *ModelCycler {
	return &ModelCycler{
		config:      config,
		sessions:    make(map[string]int),
		maxSessions: 1024,
	}
}

// WithRegistry sets the ProviderRegistry used to build selected models on
// demand. Without a registry the cycler falls back to the primary (wrapped)
// model for every call. Returns the cycler for chaining.
func (c *ModelCycler) WithRegistry(r *ProviderRegistry) *ModelCycler {
	c.registry = r
	c.resolvedModels = nil
	return c
}

// WithResolvedModels binds the cycler to a finite resolver-issued candidate
// set. Each selected model remains guarded and can only dispatch with the
// current Loop operation's permit. The models are built on demand so their
// cleanup remains scoped to one provider call.
func (c *ModelCycler) WithResolvedModels(models []ResolvedModel) *ModelCycler {
	c.resolvedModels = append([]ResolvedModel(nil), models...)
	c.registry = nil
	return c
}

// Name returns "model_cycler".
func (c *ModelCycler) Name() string { return "model_cycler" }

// WrapModel returns a BaseChatModel that routes each call to the model selected
// by the configured strategy. If the registry is unavailable, the selected
// model cannot be built, or the selected model returns an error, the call
// falls back to the wrapped primary model.
func (c *ModelCycler) WrapModel(next BaseChatModel) BaseChatModel {
	return &cycledModel{
		cycler:  c,
		primary: next,
	}
}

// selectModel returns the index of the selected model based on the configured
// strategy. When session affinity is enabled and sessionID is non-empty, the
// same sessionID always maps to the same model index. When taskType is non-empty
// and a model with a matching TaskType tag exists, that model is preferred
// regardless of strategy or session affinity.
func (c *ModelCycler) selectModel(sessionID string, taskType TaskType) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	index, changed := c.selectModelLocked(sessionID, taskType)
	if changed {
		c.selectionRevision++
	}
	return index
}

func (c *ModelCycler) selectModelLocked(sessionID string, taskType TaskType) (int, bool) {
	n := len(c.config.Models)
	if n == 0 {
		return 0, false
	}
	// Prefer a model explicitly tagged for the requested task type.
	if taskType != "" && taskType != TaskTypeChat {
		for i, m := range c.config.Models {
			if m.TaskType == taskType {
				return i, false
			}
		}
	}
	if c.config.SessionAffinity && sessionID != "" {
		if idx, ok := c.sessions[sessionID]; ok && idx < n {
			return idx, false
		}
		idx := c.selectByStrategy()
		// Evict oldest entries when the sessions map exceeds the bound.
		if len(c.sessions) >= c.maxSessions {
			for k := range c.sessions {
				delete(c.sessions, k)
				break
			}
		}
		c.sessions[sessionID] = idx
		return idx, true
	}
	return c.selectByStrategy(), c.config.Strategy == StrategyRoundRobin || (c.config.Strategy == StrategyWeighted && c.totalWeight() <= 0)
}

func (c *ModelCycler) peekModel(sessionID string, taskType TaskType) (int, *modelCyclerSelectionLease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.config.Models)
	if n == 0 {
		return 0, nil, errors.New("llm: model cycler has no candidates")
	}
	index, advance, installSession, err := c.peekModelIndexLocked(sessionID, taskType)
	if err != nil {
		return 0, nil, err
	}
	resolved := c.resolvedModels
	if index < 0 || index >= len(resolved) || resolved[index].candidate == nil {
		return 0, nil, errors.New("llm: selected resolver candidate is unavailable")
	}
	return index, &modelCyclerSelectionLease{
		cycler:         c,
		revision:       c.selectionRevision,
		candidateID:    resolved[index].candidate.id,
		index:          index,
		sessionID:      sessionID,
		advanceCounter: advance,
		installSession: installSession,
	}, nil
}

func (c *ModelCycler) peekModelIndexLocked(sessionID string, taskType TaskType) (index int, advanceCounter, installSession bool, err error) {
	for candidateIndex, entry := range c.config.Models {
		if taskType != "" && taskType != TaskTypeChat && entry.TaskType == taskType {
			return candidateIndex, false, false, nil
		}
	}
	if c.config.SessionAffinity && sessionID != "" {
		if candidateIndex, ok := c.sessions[sessionID]; ok && candidateIndex < len(c.config.Models) {
			return candidateIndex, false, false, nil
		}
		index, advanceCounter, err = c.peekStrategyLocked()
		return index, advanceCounter, true, err
	}
	index, advanceCounter, err = c.peekStrategyLocked()
	return index, advanceCounter, false, err
}

func (c *ModelCycler) peekStrategyLocked() (int, bool, error) {
	n := len(c.config.Models)
	if n == 0 {
		return 0, false, errors.New("llm: model cycler has no candidates")
	}
	switch c.config.Strategy {
	case StrategyWeighted:
		total := c.totalWeight()
		if total <= 0 {
			return int(c.counter.Load()) % n, true, nil
		}
		r := rand.IntN(total)
		cumulative := 0
		for index, model := range c.config.Models {
			if model.Weight > 0 {
				cumulative += model.Weight
			}
			if r < cumulative {
				return index, false, nil
			}
		}
		return n - 1, false, nil
	case StrategyCostPriority:
		best := 0
		for index := 1; index < n; index++ {
			if c.config.Models[index].Weight > c.config.Models[best].Weight {
				best = index
			}
		}
		return best, false, nil
	default:
		return int(c.counter.Load()) % n, true, nil
	}
}

func (c *ModelCycler) totalWeight() int {
	total := 0
	for _, model := range c.config.Models {
		if model.Weight > 0 {
			total += model.Weight
		}
	}
	return total
}

type modelCyclerSelectionLease struct {
	cycler         *ModelCycler
	revision       uint64
	candidateID    CandidateID
	index          int
	sessionID      string
	advanceCounter bool
	installSession bool
	state          atomic.Uint32
}

func (l *modelCyclerSelectionLease) CandidateID() CandidateID {
	if l == nil {
		return ""
	}
	return l.candidateID
}

func (l *modelCyclerSelectionLease) candidateSelectionLease() {}

func (l *modelCyclerSelectionLease) Commit() error {
	if l == nil || l.cycler == nil || l.candidateID == "" || !l.state.CompareAndSwap(0, 1) {
		return ErrStaleSelectionLease
	}
	c := l.cycler
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.selectionRevision != l.revision || l.index < 0 || l.index >= len(c.resolvedModels) || c.resolvedModels[l.index].candidate == nil || c.resolvedModels[l.index].candidate.id != l.candidateID {
		l.state.Store(2)
		return ErrStaleSelectionLease
	}
	changed := false
	if l.advanceCounter {
		c.counter.Add(1)
		changed = true
	}
	if l.installSession && c.config.SessionAffinity && l.sessionID != "" {
		if len(c.sessions) >= c.maxSessions {
			for key := range c.sessions {
				delete(c.sessions, key)
				break
			}
		}
		c.sessions[l.sessionID] = l.index
		changed = true
	}
	if changed {
		c.selectionRevision++
	}
	l.state.Store(2)
	return nil
}

// selectByStrategy dispatches to the strategy-specific selection method.
func (c *ModelCycler) selectByStrategy() int {
	switch c.config.Strategy {
	case StrategyWeighted:
		return c.selectWeighted()
	case StrategyCostPriority:
		return c.selectCostPriority()
	default: // StrategyRoundRobin or empty
		return c.selectRoundRobin()
	}
}

// selectRoundRobin returns the next index in round-robin order.
func (c *ModelCycler) selectRoundRobin() int {
	n := len(c.config.Models)
	if n == 0 {
		return 0
	}
	return int(c.counter.Add(1)-1) % n
}

// selectWeighted returns an index chosen proportionally to each model's
// Weight. If all weights are zero or negative it falls back to round-robin.
func (c *ModelCycler) selectWeighted() int {
	n := len(c.config.Models)
	if n == 0 {
		return 0
	}
	totalWeight := 0
	for _, m := range c.config.Models {
		if m.Weight > 0 {
			totalWeight += m.Weight
		}
	}
	if totalWeight <= 0 {
		return c.selectRoundRobin()
	}
	r := rand.IntN(totalWeight)
	cumWeight := 0
	for i, m := range c.config.Models {
		if m.Weight > 0 {
			cumWeight += m.Weight
		}
		if r < cumWeight {
			return i
		}
	}
	return n - 1
}

// selectCostPriority returns the index of the model with the lowest cost.
// For now, weight is used as the inverse of cost: the model with the highest
// weight is considered the lowest cost and is selected.
func (c *ModelCycler) selectCostPriority() int {
	n := len(c.config.Models)
	if n == 0 {
		return 0
	}
	bestIdx := 0
	bestWeight := c.config.Models[0].Weight
	for i := 1; i < n; i++ {
		if c.config.Models[i].Weight > bestWeight {
			bestWeight = c.config.Models[i].Weight
			bestIdx = i
		}
	}
	return bestIdx
}

// cycledModel is the BaseChatModel produced by ModelCycler.WrapModel. It
// delegates each call to the model selected by the cycler's strategy, falling
// back to the primary model when the selected model is unavailable or errors.
type cycledModel struct {
	cycler  *ModelCycler
	primary BaseChatModel
}

var _ BaseChatModel = (*cycledModel)(nil)

func (m *cycledModel) ProviderCandidates() []ResolvedCandidateHandle {
	if m == nil {
		return nil
	}
	candidates := ProviderCandidatesFromModel(m.primary)
	if m.cycler == nil {
		return candidates
	}
	for _, resolved := range m.cycler.resolvedModels {
		if resolved.candidate != nil {
			candidates = append(candidates, resolved.candidate)
		}
	}
	return dedupeProviderCandidates(candidates)
}

// PeekCandidate exposes the selected resolver candidate without advancing the
// cycler counter or installing session affinity. Its lease is committed only
// from the Loop operation's candidate authorization path.
func (m *cycledModel) PeekCandidate(ctx context.Context) (CandidateProposal, error) {
	if m == nil || m.cycler == nil || len(m.cycler.resolvedModels) == 0 {
		return CandidateProposal{}, ErrGuardedCandidateUnavailable
	}
	authority, ok := OperationDispatchAuthorityFromContext(ctx)
	if !ok {
		return CandidateProposal{}, ErrOperationAuthorityRequired
	}
	index, lease, err := m.cycler.peekModel(sessionIDFromContext(ctx), TaskTypeFromContext(ctx))
	if err != nil {
		return CandidateProposal{}, err
	}
	return CandidateProposal{
		OperationID:    authority.ID(),
		Candidate:      m.cycler.resolvedModels[index].CandidateHandle(),
		Reason:         CandidateCyclerSelected,
		Antecedent:     CandidateAntecedent{Kind: AntecedentInitial},
		SelectionLease: lease,
	}, nil
}

// buildSelectedModel resolves the model at idx via the registry. It returns an
// error when no registry is configured, the index is out of range, or the
// registry fails to build the model.
func (m *cycledModel) buildSelectedModel(ctx context.Context, idx int) (BaseChatModel, func(), error) {
	if m.cycler.registry == nil {
		return nil, nil, errors.New("llm: model cycler has no registry")
	}
	models := m.cycler.config.Models
	if idx < 0 || idx >= len(models) {
		return nil, nil, errors.New("llm: model index out of range")
	}
	entry := models[idx]
	resolver := m.cycler.registry.SnapshotResolver()
	if resolver == nil {
		return nil, nil, ErrMissingProviderSpec
	}
	resolved, err := resolver.ResolveModel(ctx, ModelRequest{ProviderID: entry.Provider, ModelID: entry.Model})
	if err != nil {
		return nil, nil, err
	}
	return resolved.Build(ctx)
}

// Generate routes the call to the model selected by the cycler's strategy. On
// build failure or generation error it falls back to the primary model.
func (m *cycledModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	if len(m.cycler.resolvedModels) > 0 {
		return m.generateResolved(ctx, msgs, opts...)
	}
	if m.cycler.registry == nil {
		return m.primary.Generate(ctx, msgs, opts...)
	}
	sessionID := sessionIDFromContext(ctx)
	taskType := TaskTypeFromContext(ctx)
	idx := m.cycler.selectModel(sessionID, taskType)

	model, cleanup, err := m.buildSelectedModel(ctx, idx)
	if err != nil {
		return m.generatePrimaryAfterSelectionFailure(ctx, err, msgs, opts...)
	}
	if cleanup != nil {
		cleanup = onceCleanup(cleanup)
		ctx = withCandidateCleanup(ctx, cleanup)
		defer cleanup()
	}

	resp, err := model.Generate(ctx, msgs, opts...)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return m.generatePrimaryAfterSelectionFailure(ctx, err, msgs, opts...)
	}
	return resp, nil
}

// Stream routes the call to the model selected by the cycler's strategy. On
// build failure or stream initialization error it falls back to the primary
// model. The returned channel is wrapped in a forwarding goroutine so attempt
// cancellation and any model cleanup finish before the output channel closes.
func (m *cycledModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	if len(m.cycler.resolvedModels) > 0 {
		return m.streamResolved(ctx, msgs, opts...)
	}
	if m.cycler.registry == nil {
		return m.primary.Stream(ctx, msgs, opts...)
	}
	sessionID := sessionIDFromContext(ctx)
	taskType := TaskTypeFromContext(ctx)
	idx := m.cycler.selectModel(sessionID, taskType)

	model, cleanup, err := m.buildSelectedModel(ctx, idx)
	if err != nil {
		return m.streamPrimaryAfterSelectionFailure(ctx, err, msgs, opts...)
	}
	if cleanup != nil {
		cleanup = onceCleanup(cleanup)
	}
	selectedCtx := withCandidateCleanup(ctx, cleanup)

	attemptCtx, cancel := context.WithCancelCause(selectedCtx)
	ch, err := model.Stream(attemptCtx, msgs, opts...)
	if err != nil {
		cancelStreamAttempt(ctx, cancel, err)
		drainMessageChunks(ch)
		if cleanup != nil {
			cleanup()
		}
		return m.streamPrimaryAfterSelectionFailure(ctx, err, msgs, opts...)
	}
	if ch == nil {
		err = errors.New("llm: selected model returned a nil stream")
		cancelStreamAttempt(ctx, cancel, err)
		if cleanup != nil {
			cleanup()
		}
		return nil, err
	}

	// Join the source before cleanup, attempt cancellation, and output close.
	out := make(chan MessageChunk, 16)
	go func() {
		finish := func() {
			if cleanup != nil {
				cleanup()
			}
			cancel(nil)
			close(out)
		}
		for {
			var chunk MessageChunk
			var ok bool
			select {
			case <-ctx.Done():
				cause := context.Cause(ctx)
				if cause == nil {
					cause = ctx.Err()
				}
				cancelStreamAttempt(ctx, cancel, cause)
				drainMessageChunks(ch)
				finish()
				return
			case chunk, ok = <-ch:
			}
			if !ok {
				finish()
				return
			}
			if ctx.Err() != nil {
				cause := context.Cause(ctx)
				if cause == nil {
					cause = ctx.Err()
				}
				cancelStreamAttempt(ctx, cancel, cause)
				drainMessageChunks(ch)
				finish()
				return
			}
			if chunk.Error != nil {
				cancelStreamAttempt(ctx, cancel, chunk.Error)
				select {
				case out <- chunk:
				case <-ctx.Done():
					cause := context.Cause(ctx)
					if cause == nil {
						cause = ctx.Err()
					}
					cancelStreamAttempt(ctx, cancel, cause)
					drainMessageChunks(ch)
					finish()
					return
				}
				drainMessageChunks(ch)
				finish()
				return
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				cause := context.Cause(ctx)
				if cause == nil {
					cause = ctx.Err()
				}
				cancelStreamAttempt(ctx, cancel, cause)
				drainMessageChunks(ch)
				finish()
				return
			}
		}
	}()
	return out, nil
}

func (m *cycledModel) generateResolved(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	if _, ok := OperationDispatchAuthorityFromContext(ctx); !ok {
		return nil, ErrOperationAuthorityRequired
	}
	if proposal, ok := CandidateProposalFromContext(ctx); ok {
		return m.generateProposedCandidate(ctx, proposal, msgs, opts...)
	}
	index, lease, err := m.cycler.peekModel(sessionIDFromContext(ctx), TaskTypeFromContext(ctx))
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(m.cycler.resolvedModels) {
		return nil, errors.New("llm: selected resolver candidate is unavailable")
	}
	selected := m.cycler.resolvedModels[index]
	ctx, err = withCyclerSelectedProposal(ctx, selected.CandidateHandle(), lease)
	if err != nil {
		return nil, err
	}
	return m.generateResolvedCandidate(ctx, selected, msgs, opts...)
}

func (m *cycledModel) generateProposedCandidate(ctx context.Context, proposal CandidateProposal, msgs []Message, opts ...Option) (*Message, error) {
	if proposal.Candidate == nil {
		return nil, ErrDispatchPermitRequired
	}
	if primaryCandidate := singleProviderCandidate(m.primary); primaryCandidate != nil && primaryCandidate == proposal.Candidate {
		return m.primary.Generate(ctx, msgs, opts...)
	}
	resolved, found := m.resolvedModelForCandidate(proposal.Candidate)
	if !found {
		return nil, ErrDispatchPermitRequired
	}
	return m.generateResolvedCandidate(ctx, resolved, msgs, opts...)
}

func (m *cycledModel) generateResolvedCandidate(ctx context.Context, resolved ResolvedModel, msgs []Message, opts ...Option) (*Message, error) {
	model, cleanup, err := resolved.Build(ctx)
	if err != nil {
		return nil, err
	}
	cleanup = onceCleanup(cleanup)
	callCtx := withCandidateCleanup(ctx, cleanup)
	defer finishCandidateCleanup(callCtx)
	return model.Generate(callCtx, msgs, opts...)
}

func (m *cycledModel) generatePrimaryAfterSelectionFailure(ctx context.Context, failedErr error, msgs []Message, opts ...Option) (*Message, error) {
	primaryCandidate := singleProviderCandidate(m.primary)
	if primaryCandidate == nil {
		return nil, failedErr
	}
	if failure, ok := PreDispatchCandidateFailureFromError(failedErr); ok {
		if failure.Class != PreDispatchCandidateUnavailable {
			return nil, failedErr
		}
		ctx, err := withPreDispatchCandidateProposal(ctx, m.primary, failedErr, CandidateCyclerPrimary)
		if err != nil {
			return nil, errors.Join(failedErr, err)
		}
		return m.primary.Generate(ctx, msgs, opts...)
	}
	result, ok := FinishedProviderAttemptFromError(failedErr)
	if !ok || result.Failure != FailureTransientProvider || result.Identity.Candidate == primaryCandidate.CandidateID() {
		return nil, failedErr
	}
	ctx, err := withFinishedAttemptProposal(ctx, m, CandidateCyclerPrimary, result, primaryCandidate.CandidateID())
	if err != nil {
		return nil, errors.Join(failedErr, err)
	}
	return m.primary.Generate(ctx, msgs, opts...)
}

func (m *cycledModel) streamResolved(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	if _, ok := OperationDispatchAuthorityFromContext(ctx); !ok {
		return nil, ErrOperationAuthorityRequired
	}
	if proposal, ok := CandidateProposalFromContext(ctx); ok {
		return m.streamProposedCandidate(ctx, proposal, msgs, opts...)
	}
	index, lease, err := m.cycler.peekModel(sessionIDFromContext(ctx), TaskTypeFromContext(ctx))
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(m.cycler.resolvedModels) {
		return nil, errors.New("llm: selected resolver candidate is unavailable")
	}
	selected := m.cycler.resolvedModels[index]
	ctx, err = withCyclerSelectedProposal(ctx, selected.CandidateHandle(), lease)
	if err != nil {
		return nil, err
	}
	return m.streamResolvedCandidate(ctx, selected, msgs, opts...)
}

func (m *cycledModel) streamProposedCandidate(ctx context.Context, proposal CandidateProposal, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	if proposal.Candidate == nil {
		return nil, ErrDispatchPermitRequired
	}
	if primaryCandidate := singleProviderCandidate(m.primary); primaryCandidate != nil && primaryCandidate == proposal.Candidate {
		return m.primary.Stream(ctx, msgs, opts...)
	}
	resolved, found := m.resolvedModelForCandidate(proposal.Candidate)
	if !found {
		return nil, ErrDispatchPermitRequired
	}
	return m.streamResolvedCandidate(ctx, resolved, msgs, opts...)
}

func (m *cycledModel) streamResolvedCandidate(ctx context.Context, resolved ResolvedModel, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	model, cleanup, err := resolved.Build(ctx)
	if err != nil {
		return nil, err
	}
	cleanup = onceCleanup(cleanup)
	callCtx := withCandidateCleanup(ctx, cleanup)
	stream, err := model.Stream(callCtx, msgs, opts...)
	if err != nil {
		drainMessageChunks(stream)
		finishCandidateCleanup(callCtx)
		return nil, err
	}
	if stream == nil {
		finishCandidateCleanup(callCtx)
		return nil, errors.New("llm: selected resolver candidate returned a nil stream")
	}
	return stream, nil
}

func (m *cycledModel) streamPrimaryAfterSelectionFailure(ctx context.Context, failedErr error, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	primaryCandidate := singleProviderCandidate(m.primary)
	if primaryCandidate == nil {
		return nil, failedErr
	}
	if failure, ok := PreDispatchCandidateFailureFromError(failedErr); ok {
		if failure.Class != PreDispatchCandidateUnavailable {
			return nil, failedErr
		}
		ctx, err := withPreDispatchCandidateProposal(ctx, m.primary, failedErr, CandidateCyclerPrimary)
		if err != nil {
			return nil, errors.Join(failedErr, err)
		}
		return m.primary.Stream(ctx, msgs, opts...)
	}
	result, ok := FinishedProviderAttemptFromError(failedErr)
	if !ok || result.Failure != FailureTransientProvider || result.Identity.Candidate == primaryCandidate.CandidateID() {
		return nil, failedErr
	}
	ctx, err := withFinishedAttemptProposal(ctx, m, CandidateCyclerPrimary, result, primaryCandidate.CandidateID())
	if err != nil {
		return nil, errors.Join(failedErr, err)
	}
	return m.primary.Stream(ctx, msgs, opts...)
}

func (m *cycledModel) resolvedModelForCandidate(candidate ResolvedCandidateHandle) (ResolvedModel, bool) {
	for _, resolved := range m.cycler.resolvedModels {
		if resolved.CandidateHandle() == candidate {
			return resolved, true
		}
	}
	return ResolvedModel{}, false
}

func singleProviderCandidate(model BaseChatModel) ResolvedCandidateHandle {
	candidates := ProviderCandidatesFromModel(model)
	if len(candidates) != 1 {
		return nil
	}
	return candidates[0]
}

func withCyclerSelectedProposal(ctx context.Context, candidate ResolvedCandidateHandle, lease CandidateSelectionLease) (context.Context, error) {
	authority, ok := OperationDispatchAuthorityFromContext(ctx)
	if !ok {
		return nil, ErrOperationAuthorityRequired
	}
	if candidate == nil || lease == nil || lease.CandidateID() != candidate.CandidateID() {
		return nil, ErrDispatchPermitRequired
	}
	return WithCandidateProposal(ctx, CandidateProposal{
		OperationID:    authority.ID(),
		Candidate:      candidate,
		Reason:         CandidateCyclerSelected,
		Antecedent:     CandidateAntecedent{Kind: AntecedentInitial},
		SelectionLease: lease,
	}), nil
}

func onceCleanup(cleanup func()) func() {
	if cleanup == nil {
		return nil
	}
	var once sync.Once
	return func() { once.Do(cleanup) }
}
