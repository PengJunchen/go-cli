package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

const requestEnvelopeDefaultCeiling = 8000

var ErrRequestEnvelopeRejected = errors.New("core: request envelope preflight rejected")

// RequestEnvelopeError is a typed pre-dispatch result. It contains only a
// stable reason and an error chain; it never includes messages, schemas,
// credentials, or provider-reported usage.
type RequestEnvelopeError struct {
	Reason string
	Cause  error
}

func (e *RequestEnvelopeError) Error() string {
	if e == nil {
		return ErrRequestEnvelopeRejected.Error()
	}
	return ErrRequestEnvelopeRejected.Error() + ": " + e.Reason
}

func (e *RequestEnvelopeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return errors.Join(ErrRequestEnvelopeRejected, e.Cause)
}

// NoProviderDispatch marks envelope rejection as a Loop-owned decision made
// before a provider request crossed BeginDispatch.
func (*RequestEnvelopeError) NoProviderDispatch() bool { return true }

type preparedRequestBase struct {
	requested                      llm.GenerationOptions
	multimodalTokensPerItemReserve int
}

func newPreparedRequestBase(options []llm.Option) (preparedRequestBase, error) {
	requested, err := llm.MaterializeGenerationOptions(options...)
	if err != nil {
		return preparedRequestBase{}, &RequestEnvelopeError{Reason: "generation options are not representable", Cause: err}
	}
	return preparedRequestBase{requested: requested}, nil
}

func prepareRequestPlan(
	ctx context.Context,
	proposal llm.CandidateProposal,
	messages []llm.Message,
	requestBase preparedRequestBase,
	estimator compaction.TokenEstimator,
	compactionCeiling int,
) (*llm.PreparedRequestPlan, error) {
	if ctx == nil || ctx.Err() != nil {
		if ctx == nil {
			return nil, &RequestEnvelopeError{Reason: "request context is unavailable"}
		}
		return nil, &RequestEnvelopeError{Reason: "request was canceled before estimation", Cause: ctx.Err()}
	}
	candidate := proposal.Candidate
	if candidate == nil || candidate.CandidateID() == "" {
		return nil, &RequestEnvelopeError{Reason: "active candidate identity is unavailable"}
	}
	modelSpec := candidate.ModelSpec()
	identity := candidate.Identity()
	if identity.Candidate != candidate.CandidateID() || modelSpec.ProviderID != identity.ProviderID || modelSpec.ModelID != identity.ModelID {
		return nil, &RequestEnvelopeError{Reason: "candidate metadata does not match resolver identity"}
	}
	if estimator == nil {
		return nil, &RequestEnvelopeError{Reason: "message and schema estimator is unavailable"}
	}
	if requestBase.multimodalTokensPerItemReserve < 0 {
		return nil, &RequestEnvelopeError{Reason: "multimodal per-item reserve must not be negative"}
	}
	options, err := effectiveCandidateOptions(candidate, modelSpec, requestBase.requested)
	if err != nil {
		return nil, &RequestEnvelopeError{Reason: "generation options are not representable", Cause: err}
	}
	estimate, err := estimateRequestEnvelope(
		ctx, candidate, modelSpec, messages, options, estimator,
		compactionCeiling, requestBase.multimodalTokensPerItemReserve,
	)
	if err != nil {
		return nil, err
	}
	return llm.NewPreparedRequestPlan(proposal, modelSpec, messages, options, estimate)
}

func prepareAdmittedRequestPlan(
	ctx context.Context,
	proposal llm.CandidateProposal,
	messages []llm.Message,
	currentSubmissionIndex int,
	requestBase preparedRequestBase,
	consumer *runCommitConsumer,
	midTurnCompactor *compaction.MidTurnCompact,
	compactor compaction.Compactor,
	estimator compaction.TokenEstimator,
	compactionCeiling int,
) (*llm.PreparedRequestPlan, []llm.Message, int, error) {
	plan, err := prepareRequestPlan(ctx, proposal, messages, requestBase, estimator, compactionCeiling)
	if err != nil {
		return nil, messages, currentSubmissionIndex, err
	}
	if plan.Estimate().AccountedMessages.State != llm.EstimateKnown {
		return nil, messages, currentSubmissionIndex, &RequestEnvelopeError{Reason: "multimodal request accounting is unknown"}
	}
	if envelopeEstimateFits(plan.Estimate()) {
		return plan, messages, currentSubmissionIndex, nil
	}
	compactedMessages, compactedSubmissionIndex, err := compactPreparedMessages(
		ctx, messages, consumer, midTurnCompactor, compactor, estimator,
		plan.Estimate().MessageBudget, plan.Estimate().Multimodal.Tokens,
		currentSubmissionIndex,
	)
	if err != nil {
		return nil, messages, currentSubmissionIndex, err
	}
	plan, err = prepareRequestPlan(ctx, proposal, compactedMessages, requestBase, estimator, compactionCeiling)
	if err != nil {
		return nil, compactedMessages, compactedSubmissionIndex, err
	}
	if plan.Estimate().AccountedMessages.State != llm.EstimateKnown {
		return nil, compactedMessages, compactedSubmissionIndex, &RequestEnvelopeError{Reason: "multimodal request accounting is unknown"}
	}
	if !envelopeEstimateFits(plan.Estimate()) {
		return nil, compactedMessages, compactedSubmissionIndex, &RequestEnvelopeError{Reason: "request still exceeds the message budget after compaction"}
	}
	return plan, compactedMessages, compactedSubmissionIndex, nil
}

func effectiveCandidateOptions(candidate llm.ResolvedCandidateHandle, model llm.ModelSpec, requested llm.GenerationOptions) (llm.GenerationOptions, error) {
	options := candidate.DefaultGenerationOptions()
	if requested.Temperature != nil {
		options.Temperature = requested.Temperature
	}
	if requested.MaxTokens != nil {
		options.MaxTokens = requested.MaxTokens
	}
	if requested.StopStrings != nil {
		options.StopStrings = requested.StopStrings
	}
	if requested.Tools != nil {
		options.Tools = requested.Tools
	}
	if requested.Thinking != nil {
		options.Thinking = requested.Thinking
	}
	options, err := llm.CloneGenerationOptions(options)
	if err != nil {
		return llm.GenerationOptions{}, err
	}
	if options.MaxTokens != nil {
		if *options.MaxTokens < 0 {
			return llm.GenerationOptions{}, errors.New("negative output token cap")
		}
		if *options.MaxTokens > 0 && model.Known && model.Info.MaxOutputTokens > 0 && *options.MaxTokens > model.Info.MaxOutputTokens {
			value := model.Info.MaxOutputTokens
			options.MaxTokens = &value
		}
	}
	return options, nil
}

func estimateRequestEnvelope(
	ctx context.Context,
	candidate llm.ResolvedCandidateHandle,
	model llm.ModelSpec,
	messages []llm.Message,
	options llm.GenerationOptions,
	estimator compaction.TokenEstimator,
	compactionCeiling int,
	multimodalTokensPerItemReserve int,
) (llm.RequestEnvelopeEstimate, error) {
	if err := ctx.Err(); err != nil {
		return llm.RequestEnvelopeEstimate{}, &RequestEnvelopeError{Reason: "request was canceled during estimation", Cause: err}
	}
	messageTokens, multimodal, err := estimateProviderMessages(messages, estimator, multimodalTokensPerItemReserve)
	if err != nil {
		return llm.RequestEnvelopeEstimate{}, &RequestEnvelopeError{Reason: "provider-visible message terms are unknown", Cause: err}
	}
	toolSchemaTokens, err := estimateProviderToolSchemas(options.Tools, estimator)
	if err != nil {
		return llm.RequestEnvelopeEstimate{}, &RequestEnvelopeError{Reason: "final tool-schema estimate failed", Cause: err}
	}
	framingTokens, err := candidate.EstimateProtocolFraming(messages, options)
	if err != nil {
		return llm.RequestEnvelopeEstimate{}, &RequestEnvelopeError{Reason: "enabled protocol framing estimate is unavailable", Cause: err}
	}
	if framingTokens < 0 {
		return llm.RequestEnvelopeEstimate{}, &RequestEnvelopeError{Reason: "enabled protocol framing estimate is invalid"}
	}
	if err := ctx.Err(); err != nil {
		return llm.RequestEnvelopeEstimate{}, &RequestEnvelopeError{Reason: "request was canceled during estimation", Cause: err}
	}
	if compactionCeiling <= 0 {
		compactionCeiling = requestEnvelopeDefaultCeiling
	}
	limitTerm := llm.RequestEnvelopeTerm{State: llm.EstimateUnknown, Reason: "candidate provider limit metadata is missing"}
	providerLimit := 0
	if model.Known && model.Info.InputTokenLimit > 0 {
		providerLimit = model.Info.InputTokenLimit
		limitTerm = llm.RequestEnvelopeTerm{Tokens: providerLimit, State: llm.EstimateKnown}
	} else if model.Known && model.Info.ContextWindow > 0 {
		reserve, reserveKnown, reserveReason := outputTokenReserve(model, options)
		if reserveKnown {
			available := model.Info.ContextWindow - reserve
			if available <= 0 {
				return llm.RequestEnvelopeEstimate{}, &RequestEnvelopeError{Reason: "model context window is exhausted by the output reserve"}
			}
			providerLimit = available
			limitTerm = llm.RequestEnvelopeTerm{Tokens: providerLimit, State: llm.EstimateKnown}
		} else {
			limitTerm.Reason = reserveReason
		}
	}
	envelopeTokens, ok := checkedTokenAdd(toolSchemaTokens, framingTokens)
	if !ok {
		return llm.RequestEnvelopeEstimate{}, &RequestEnvelopeError{Reason: "provider envelope estimate overflow"}
	}
	accountedMessages, accountedKnown, err := accountedMessageEstimate(messageTokens, multimodal)
	if err != nil {
		return llm.RequestEnvelopeEstimate{}, &RequestEnvelopeError{Reason: "accounted message estimate overflow", Cause: err}
	}
	localMessageBudget := fourFifthsFloor(compactionCeiling)
	providerEnvelopeBudget := 0
	providerMessageBudget := 0
	messageBudget := localMessageBudget
	safeInputLimit := 0
	if limitTerm.State == llm.EstimateKnown {
		safeInputLimit = fourFifthsFloor(providerLimit)
		providerEnvelopeBudget = safeInputLimit
		if envelopeTokens > providerEnvelopeBudget {
			return llm.RequestEnvelopeEstimate{}, &RequestEnvelopeError{Reason: "tool schemas and protocol framing exhaust the known provider envelope budget"}
		}
		providerMessageBudget = providerEnvelopeBudget - envelopeTokens
		messageBudget = min(localMessageBudget, providerMessageBudget)
	}
	accountedTerm := llm.RequestEnvelopeTerm{Tokens: accountedMessages, State: llm.EstimateKnown}
	if !accountedKnown {
		accountedTerm.State = llm.EstimateUnknown
		accountedTerm.Reason = multimodal.Reason
	}
	return llm.RequestEnvelopeEstimate{
		Messages:               llm.RequestEnvelopeTerm{Tokens: messageTokens, State: llm.EstimateKnown},
		AccountedMessages:      accountedTerm,
		ToolSchemas:            llm.RequestEnvelopeTerm{Tokens: toolSchemaTokens, State: llm.EstimateKnown},
		ProtocolFraming:        llm.RequestEnvelopeTerm{Tokens: framingTokens, State: llm.EstimateKnown},
		Multimodal:             multimodal,
		ProviderLimit:          limitTerm,
		ProviderFitState:       llm.EstimateUnknown,
		ProviderFitReason:      "request estimates do not prove provider acceptance",
		Ceiling:                compactionCeiling,
		RawInputLimit:          providerLimit,
		SafeInputLimit:         safeInputLimit,
		LocalMessageBudget:     localMessageBudget,
		ProviderEnvelopeBudget: providerEnvelopeBudget,
		ProviderMessageBudget:  providerMessageBudget,
		MessageBudget:          messageBudget,
	}, nil
}

func outputTokenReserve(model llm.ModelSpec, options llm.GenerationOptions) (int, bool, string) {
	if options.MaxTokens != nil {
		if *options.MaxTokens < 0 {
			return 0, false, "negative output token cap is invalid"
		}
		return *options.MaxTokens, true, ""
	}
	if model.Info.MaxOutputTokens > 0 {
		return model.Info.MaxOutputTokens, true, ""
	}
	return 0, false, "context window is known but output reserve is unknown"
}

func fourFifthsFloor(value int) int {
	return value/5*4 + value%5*4/5
}

func checkedTokenAdd(left, right int) (int, bool) {
	if left < 0 || right < 0 || left > int(^uint(0)>>1)-right {
		return 0, false
	}
	return left + right, true
}

func checkedTokenMultiply(left, right int) (int, bool) {
	if left < 0 || right < 0 || (left != 0 && right > int(^uint(0)>>1)/left) {
		return 0, false
	}
	return left * right, true
}

func accountedMessageEstimate(textTokens int, multimodal llm.RequestEnvelopeTerm) (int, bool, error) {
	if textTokens < 0 || multimodal.Tokens < 0 {
		return 0, false, errors.New("negative accounted message term")
	}
	accounted, ok := checkedTokenAdd(textTokens, multimodal.Tokens)
	if !ok {
		return 0, false, errors.New("accounted message estimate overflow")
	}
	known := multimodal.State == llm.EstimateKnown || multimodal.Tokens > 0
	return accounted, known, nil
}

func estimateProviderMessages(messages []llm.Message, estimator compaction.TokenEstimator, multimodalTokensPerItemReserve int) (int, llm.RequestEnvelopeTerm, error) {
	if multimodalTokensPerItemReserve < 0 {
		return 0, llm.RequestEnvelopeTerm{}, errors.New("multimodal per-item reserve must not be negative")
	}
	total := 0
	multimodal := llm.RequestEnvelopeTerm{State: llm.EstimateKnown}
	unknownBlocks := 0
	addText := func(value string) error {
		if value == "" {
			return nil
		}
		tokens, err := estimator.Estimate(value)
		if err != nil {
			return err
		}
		if tokens < 0 {
			return errors.New("token estimator returned a negative count")
		}
		var ok bool
		total, ok = checkedTokenAdd(total, tokens)
		if !ok {
			return errors.New("message estimate overflow")
		}
		return nil
	}
	for _, message := range messages {
		if err := addText(string(message.Role)); err != nil {
			return 0, multimodal, err
		}
		if err := addText(message.Name); err != nil {
			return 0, multimodal, err
		}
		if err := addText(message.ToolCallID); err != nil {
			return 0, multimodal, err
		}
		if message.ContentBlocks == nil {
			if err := addText(message.Content); err != nil {
				return 0, multimodal, err
			}
		} else {
			for _, block := range message.ContentBlocks {
				switch block.Type {
				case "text":
					if err := addText(block.Text); err != nil {
						return 0, multimodal, err
					}
				case "image_url":
					unknownBlocks++
				default:
					unknownBlocks++
				}
			}
		}
		for _, call := range message.ToolCalls {
			if err := addText(call.ID); err != nil {
				return 0, multimodal, err
			}
			if err := addText(call.Name); err != nil {
				return 0, multimodal, err
			}
			encoded, err := json.Marshal(call.Args)
			if err != nil {
				return 0, multimodal, fmt.Errorf("tool-call argument serialization failed: %w", err)
			}
			if err := addText(string(encoded)); err != nil {
				return 0, multimodal, err
			}
		}
	}
	if unknownBlocks > 0 {
		multimodal = llm.RequestEnvelopeTerm{
			State:  llm.EstimateUnknown,
			Reason: "multimodal token accounting is not provider-aware",
		}
		if multimodalTokensPerItemReserve > 0 {
			reserve, ok := checkedTokenMultiply(unknownBlocks, multimodalTokensPerItemReserve)
			if !ok {
				return 0, multimodal, errors.New("multimodal reserve overflow")
			}
			multimodal.Tokens = reserve
			multimodal.Reason += "; a finite configured per-item reserve is included"
		} else {
			multimodal.Reason += "; no finite conservative reserve is configured"
		}
	}
	return total, multimodal, nil
}

func estimateProviderToolSchemas(tools []llm.ToolDefinition, estimator compaction.TokenEstimator) (int, error) {
	if len(tools) == 0 {
		return 0, nil
	}
	type functionSchema struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Parameters  any    `json:"parameters"`
	}
	type protocolTool struct {
		Type     string         `json:"type"`
		Function functionSchema `json:"function"`
	}
	wireTools := make([]protocolTool, 0, len(tools))
	for _, tool := range tools {
		parameters := tool.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		wireTools = append(wireTools, protocolTool{
			Type: "function",
			Function: functionSchema{
				Name: tool.Name, Description: tool.Description, Parameters: parameters,
			},
		})
	}
	encoded, err := json.Marshal(wireTools)
	if err != nil {
		return 0, err
	}
	tokens, err := estimator.Estimate(string(encoded))
	if err != nil {
		return 0, err
	}
	if tokens < 0 {
		return 0, errors.New("token estimator returned a negative count")
	}
	return tokens, nil
}

// preparedCompactionTextBudget reserves protocol fields that the content-only
// Compactor estimator does not count. Reserving the original request metadata
// is conservative: dropping history can shrink it. The final envelope check
// still validates the actual compacted output, including custom summaries.
func preparedCompactionTextBudget(messages []llm.Message, estimator compaction.TokenEstimator, messageBudget, multimodalTokens int) (int, error) {
	if estimator == nil || messageBudget < 0 || multimodalTokens < 0 || multimodalTokens > messageBudget {
		return 0, &RequestEnvelopeError{Reason: "compaction message budget or estimator is unavailable"}
	}
	metadata := append([]llm.Message(nil), messages...)
	for index := range metadata {
		metadata[index].Content = ""
		metadata[index].ContentBlocks = nil
	}
	metadataTokens, _, err := estimateProviderMessages(metadata, estimator, 0)
	if err != nil {
		return 0, &RequestEnvelopeError{Reason: "compaction message metadata estimate failed", Cause: err}
	}
	textBudget := messageBudget - multimodalTokens
	if metadataTokens > textBudget {
		return 0, &RequestEnvelopeError{Reason: "message metadata reserve exceeds the available message budget"}
	}
	return textBudget - metadataTokens, nil
}

func compactPreparedMessages(
	ctx context.Context,
	messages []llm.Message,
	consumer *runCommitConsumer,
	midTurnCompactor *compaction.MidTurnCompact,
	compactor compaction.Compactor,
	estimator compaction.TokenEstimator,
	messageBudget int,
	multimodalTokens int,
	currentSubmissionIndex int,
) ([]llm.Message, int, error) {
	textBudget, err := preparedCompactionTextBudget(messages, estimator, messageBudget, multimodalTokens)
	if err != nil {
		return nil, currentSubmissionIndex, err
	}
	if consumer != nil {
		return preflightDurableRequestAt(ctx, messages, consumer, compactor, estimator, textBudget, currentSubmissionIndex)
	}
	if compactor == nil || estimator == nil {
		return nil, currentSubmissionIndex, &RequestEnvelopeError{Reason: "request exceeds the message budget and compaction is unavailable"}
	}
	if currentSubmissionIndex < 0 || currentSubmissionIndex >= len(messages) {
		return nil, currentSubmissionIndex, &RequestEnvelopeError{Reason: "current submission cannot be preserved during compaction"}
	}
	items := messagesToTurnItems(messages)
	items, err = compaction.NormalizeToolCallGroups(items)
	if err != nil {
		return nil, currentSubmissionIndex, &RequestEnvelopeError{Reason: "tool-call groups cannot be compacted safely", Cause: err}
	}
	if err := compaction.ValidateCompactionEligible(items); err != nil {
		return nil, currentSubmissionIndex, &RequestEnvelopeError{Reason: "request contains non-compaction-safe content", Cause: err}
	}
	result, err := compactor.Compact(ctx, cloneCompactionTurnItems(items), textBudget, estimator)
	if err != nil {
		return nil, currentSubmissionIndex, &RequestEnvelopeError{Reason: "request compaction failed", Cause: err}
	}
	compactedItems, err := compaction.NormalizeToolCallGroups(result.Items)
	if err != nil {
		return nil, currentSubmissionIndex, &RequestEnvelopeError{Reason: "compactor returned invalid tool-call groups", Cause: err}
	}
	currentMessage := cloneLLMMessage(messages[currentSubmissionIndex])
	currentID := items[currentSubmissionIndex].ID
	compactedIndex := -1
	for index, item := range compactedItems {
		if item.ID == currentID {
			if compactedIndex >= 0 {
				return nil, currentSubmissionIndex, &RequestEnvelopeError{Reason: "compactor duplicated the current submission"}
			}
			compactedIndex = index
		}
	}
	if compactedIndex < 0 {
		return nil, currentSubmissionIndex, &RequestEnvelopeError{Reason: "compactor removed the current submission"}
	}
	compactedMessages := turnItemsToMessages(compactedItems)
	if !reflect.DeepEqual(currentMessage, compactedMessages[compactedIndex]) {
		return nil, currentSubmissionIndex, &RequestEnvelopeError{Reason: "compactor changed the current submission"}
	}
	return compactedMessages, compactedIndex, nil
}

func envelopeEstimateFits(estimate llm.RequestEnvelopeEstimate) bool {
	if estimate.AccountedMessages.State != llm.EstimateKnown || estimate.AccountedMessages.Tokens > estimate.LocalMessageBudget {
		return false
	}
	if estimate.ProviderLimit.State != llm.EstimateKnown {
		return true
	}
	total, ok := checkedTokenAdd(estimate.AccountedMessages.Tokens, estimate.ToolSchemas.Tokens)
	if !ok {
		return false
	}
	total, ok = checkedTokenAdd(total, estimate.ProtocolFraming.Tokens)
	return ok && total <= estimate.ProviderEnvelopeBudget
}
