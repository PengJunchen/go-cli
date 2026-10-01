package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
)

var (
	// ErrGenerationOptionsUnsupported means an option produced values that the
	// finite provider request snapshot cannot safely copy or encode.
	ErrGenerationOptionsUnsupported = errors.New("llm: generation options are unsupported")
	// ErrPreparedRequestMismatch means a prepared request was presented to a
	// different candidate or with changed request values.
	ErrPreparedRequestMismatch = errors.New("llm: prepared request does not match dispatch")
	// ErrPreparedRequestConsumed means a prepared request has already been used.
	ErrPreparedRequestConsumed = errors.New("llm: prepared request already consumed")
	// ErrRequestEnvelopeEstimatorUnavailable means the active resolver candidate
	// has no estimator for its enabled wire protocol.
	ErrRequestEnvelopeEstimatorUnavailable = errors.New("llm: request envelope estimator unavailable")
)

// EstimateState says whether an envelope term has a usable local estimate.
type EstimateState uint8

const (
	EstimateUnknown EstimateState = iota
	EstimateKnown
)

// RequestEnvelopeTerm is one disjoint estimated part of a provider request.
// It is an estimate, never a guarantee that a provider will accept the request.
type RequestEnvelopeTerm struct {
	Tokens int
	State  EstimateState
	Reason string
}

// RequestEnvelopeEstimate records the input terms and local budget used to
// admit one prepared provider request. No field is provider-reported usage.
type RequestEnvelopeEstimate struct {
	// Messages is the estimated provider-visible text term, including roles,
	// names, IDs, text, and serialized tool-call data. It excludes multimodal
	// payload, final tool schemas, and protocol framing.
	Messages RequestEnvelopeTerm
	// AccountedMessages is the bounded text-plus-multimodal amount compared with
	// both the local message budget and any known provider-derived message budget.
	AccountedMessages RequestEnvelopeTerm
	ToolSchemas       RequestEnvelopeTerm
	ProtocolFraming   RequestEnvelopeTerm
	// Multimodal retains an unknown state when its exact provider-aware amount
	// is unavailable. Tokens may still carry a configured finite reserve used
	// by AccountedMessages.
	Multimodal RequestEnvelopeTerm
	// ProviderLimit is the candidate-specific input capacity after subtracting
	// an output reserve only when deriving it from ContextWindow.
	ProviderLimit RequestEnvelopeTerm
	// ProviderFitState remains unknown because local estimates do not prove
	// that a provider will accept the request.
	ProviderFitState       EstimateState
	ProviderFitReason      string
	Ceiling                int
	RawInputLimit          int
	SafeInputLimit         int
	LocalMessageBudget     int
	ProviderEnvelopeBudget int
	ProviderMessageBudget  int
	MessageBudget          int
}

// ProtocolEnvelopeEstimator is implemented by an enabled provider adapter
// owner. It estimates only protocol framing; message content and schema JSON
// remain separate terms estimated by the caller's tokenizer.
type ProtocolEnvelopeEstimator interface {
	EstimateProtocolFraming(messages []Message, options GenerationOptions) (int, error)
}

// MaterializeGenerationOptions runs each caller option once, then returns an
// isolated copy of the finite option value passed to provider adapters.
func MaterializeGenerationOptions(options ...Option) (result GenerationOptions, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = GenerationOptions{}
			err = fmt.Errorf("%w: option panicked", ErrGenerationOptionsUnsupported)
		}
	}()
	for _, option := range options {
		if option != nil {
			option(&result)
		}
	}
	return CloneGenerationOptions(result)
}

// CloneGenerationOptions returns an isolated JSON-compatible schema graph and
// independent pointer/slice values.
func CloneGenerationOptions(options GenerationOptions) (GenerationOptions, error) {
	cloned := options
	if options.Temperature != nil {
		if math.IsNaN(*options.Temperature) || math.IsInf(*options.Temperature, 0) {
			return GenerationOptions{}, fmt.Errorf("%w: temperature is not finite", ErrGenerationOptionsUnsupported)
		}
		value := *options.Temperature
		cloned.Temperature = &value
	}
	if options.MaxTokens != nil {
		value := *options.MaxTokens
		cloned.MaxTokens = &value
	}
	cloned.StopStrings = append([]string(nil), options.StopStrings...)
	if options.Tools != nil {
		cloned.Tools = make([]ToolDefinition, len(options.Tools))
		for index, tool := range options.Tools {
			cloned.Tools[index] = tool
			parameters, err := cloneJSONValue(tool.Parameters)
			if err != nil {
				return GenerationOptions{}, fmt.Errorf("%w: tool schema %q: %v", ErrGenerationOptionsUnsupported, tool.Name, err)
			}
			cloned.Tools[index].Parameters = parameters
		}
	}
	if options.Thinking != nil {
		value := *options.Thinking
		cloned.Thinking = &value
	}
	return cloned, nil
}

// OptionFromGenerationOptions creates the one canonical adapter option for an
// already materialized snapshot. It never invokes the original option closures.
func OptionFromGenerationOptions(options GenerationOptions) (Option, error) {
	frozen, err := CloneGenerationOptions(options)
	if err != nil {
		return nil, err
	}
	return func(destination *GenerationOptions) {
		if destination == nil {
			return
		}
		copy, copyErr := CloneGenerationOptions(frozen)
		if copyErr != nil {
			return
		}
		*destination = copy
	}, nil
}

func cloneJSONValue(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var cloned any
	if err := decoder.Decode(&cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

type preparedRequestPlanContextKey struct{}
type candidateSelectionSourceContextKey struct{}

// PreparedRequestPlan is the immutable request value shared by estimation and
// one provider dispatch. Its fields are exposed through defensive snapshots.
type PreparedRequestPlan struct {
	proposal CandidateProposal
	model    ModelSpec
	messages []Message
	options  GenerationOptions
	estimate RequestEnvelopeEstimate
	consumed atomic.Bool
}

// NewPreparedRequestPlan clones the finite request graph so caller-owned
// messages, tool schemas, and pointer-valued options cannot change after
// estimation.
func NewPreparedRequestPlan(proposal CandidateProposal, model ModelSpec, messages []Message, options GenerationOptions, estimate RequestEnvelopeEstimate) (*PreparedRequestPlan, error) {
	if proposal.Candidate == nil || proposal.Candidate.CandidateID() == "" {
		return nil, fmt.Errorf("%w: candidate identity is missing", ErrPreparedRequestMismatch)
	}
	frozenOptions, err := CloneGenerationOptions(options)
	if err != nil {
		return nil, err
	}
	frozenMessages, err := cloneMessages(messages)
	if err != nil {
		return nil, err
	}
	return &PreparedRequestPlan{
		proposal: proposal,
		model:    model,
		messages: frozenMessages,
		options:  frozenOptions,
		estimate: estimate,
	}, nil
}

// Proposal returns the resolver-owned proposal bound to this request plan.
func (p *PreparedRequestPlan) Proposal() CandidateProposal {
	if p == nil {
		return CandidateProposal{}
	}
	return p.proposal
}

// Model returns the provider-scoped model metadata bound to this plan.
func (p *PreparedRequestPlan) Model() ModelSpec {
	if p == nil {
		return ModelSpec{}
	}
	return p.model
}

// Messages returns an isolated copy of the provider-visible messages.
func (p *PreparedRequestPlan) Messages() []Message {
	if p == nil {
		return nil
	}
	messages, _ := cloneMessages(p.messages)
	return messages
}

// Options returns an isolated copy of the effective generation options.
func (p *PreparedRequestPlan) Options() GenerationOptions {
	if p == nil {
		return GenerationOptions{}
	}
	options, _ := CloneGenerationOptions(p.options)
	return options
}

// Estimate returns the non-secret local request-envelope estimate.
func (p *PreparedRequestPlan) Estimate() RequestEnvelopeEstimate {
	if p == nil {
		return RequestEnvelopeEstimate{}
	}
	return p.estimate
}

// Consume verifies identity and exact request values before allowing the
// candidate adapter to dispatch the plan once.
func (p *PreparedRequestPlan) Consume(candidate ResolvedCandidateHandle, messages []Message, options ...Option) error {
	if p == nil || candidate == nil || p.proposal.Candidate != candidate || candidate.CandidateID() != p.proposal.Candidate.CandidateID() {
		return ErrPreparedRequestMismatch
	}
	if !equalMessages(p.messages, messages) {
		return ErrPreparedRequestMismatch
	}
	materialized, err := MaterializeGenerationOptions(options...)
	if err != nil {
		return err
	}
	if !equalGenerationOptions(p.options, materialized) {
		return ErrPreparedRequestMismatch
	}
	if !p.consumed.CompareAndSwap(false, true) {
		return ErrPreparedRequestConsumed
	}
	return nil
}

// WithPreparedRequestPlan binds the same proposal and request plan to the
// provider call context so wrappers can forward it without rebuilding values.
func WithPreparedRequestPlan(ctx context.Context, plan *PreparedRequestPlan) context.Context {
	if plan == nil {
		return ctx
	}
	ctx = WithCandidateProposal(ctx, plan.proposal)
	return context.WithValue(ctx, preparedRequestPlanContextKey{}, plan)
}

// PreparedRequestPlanFromContext retrieves a Loop-authored plan, if present.
func PreparedRequestPlanFromContext(ctx context.Context) (*PreparedRequestPlan, bool) {
	if ctx == nil {
		return nil, false
	}
	plan, ok := ctx.Value(preparedRequestPlanContextKey{}).(*PreparedRequestPlan)
	return plan, ok && plan != nil
}

// WithCandidateSelectionSource attaches the unwrapped resolver-backed model
// that can perform a side-effect-free initial candidate peek.
func WithCandidateSelectionSource(ctx context.Context, source CandidateSelectionSource) context.Context {
	if source == nil {
		return ctx
	}
	return context.WithValue(ctx, candidateSelectionSourceContextKey{}, source)
}

// CandidateSelectionSourceFromContext returns a previously attached
// metadata-only candidate peeker.
func CandidateSelectionSourceFromContext(ctx context.Context) (CandidateSelectionSource, bool) {
	if ctx == nil {
		return nil, false
	}
	source, ok := ctx.Value(candidateSelectionSourceContextKey{}).(CandidateSelectionSource)
	return source, ok && source != nil
}

func cloneMessages(messages []Message) ([]Message, error) {
	if messages == nil {
		return nil, nil
	}
	cloned := make([]Message, len(messages))
	for index, message := range messages {
		cloned[index] = message
		cloned[index].ContentBlocks = append([]ContentBlock(nil), message.ContentBlocks...)
		for blockIndex := range cloned[index].ContentBlocks {
			if message.ContentBlocks[blockIndex].ImageURL != nil {
				imageURL := *message.ContentBlocks[blockIndex].ImageURL
				cloned[index].ContentBlocks[blockIndex].ImageURL = &imageURL
			}
		}
		cloned[index].ToolCalls = append([]ToolCall(nil), message.ToolCalls...)
		for callIndex := range cloned[index].ToolCalls {
			args, err := cloneJSONValue(message.ToolCalls[callIndex].Args)
			if err != nil {
				return nil, fmt.Errorf("%w: tool call arguments: %v", ErrPreparedRequestMismatch, err)
			}
			cloned[index].ToolCalls[callIndex].Args = args
		}
		if message.Usage != nil {
			usage := *message.Usage
			cloned[index].Usage = &usage
		}
	}
	return cloned, nil
}

func equalMessages(left, right []Message) bool {
	leftCopy, leftErr := cloneMessages(left)
	rightCopy, rightErr := cloneMessages(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftJSON, leftErr := json.Marshal(leftCopy)
	rightJSON, rightErr := json.Marshal(rightCopy)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func equalGenerationOptions(left, right GenerationOptions) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
