package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// accumulateOpenAIStreamToolCalls processes SSE events from an OpenAI-compatible
// stream, accumulating tool-call fragments and forwarding content chunks to ch.
// It emits a role-only MessageChunk on the first content or tool-call delta
// (matching streamClaude's message_start behavior) so callers can render an
// assistant placeholder early. It returns an error unless the stream reached
// the required [DONE] marker with no parser or payload errors.
func accumulateOpenAIStreamToolCalls(ctx context.Context, events <-chan SSEEvent, ch chan<- MessageChunk) (toolCalls []ToolCall, finishReason string, usage *Usage, err error) {
	return accumulateOpenAIStreamToolCallsWithProvider(ctx, events, ch, http.StatusOK, "openai")
}

func accumulateOpenAIStreamToolCallsWithProvider(
	ctx context.Context,
	events <-chan SSEEvent,
	ch chan<- MessageChunk,
	statusCode int,
	provider string,
) (toolCalls []ToolCall, finishReason string, usage *Usage, err error) {
	var toolNameByIndex map[int]string
	var toolIDByIndex map[int]string
	var toolArgsBuf []*strings.Builder
	emittedRole := false
	canceled := false
	doneSeen := false
	terminalSeen := false
	validTerminal := false
	eventCount := 0
	toolPayloadBytes := 0
	var streamErr error
	terminalUsageAccepted := false
	acceptUsage := func(candidate *openAIUsage) {
		if candidate == nil || terminalUsageAccepted {
			return
		}
		if reported := candidate.toUsage(); reported != nil && reported.PrimaryCoverage.Normalized() == UsageCoverageComplete {
			// A preterminal snapshot is useful billing evidence, but an empty
			// later stop frame cannot establish that it contains final totals.
			if !validTerminal {
				reported.PrimaryCoverage = UsageCoverageUnknown
			}
			usage = reported
			// Keep the first valid terminal usage if a provider sends another
			// contradictory usage object after it; do not fabricate a replacement.
			if terminalSeen {
				terminalUsageAccepted = true
			}
		} else if candidate.PromptTokens != nil && candidate.CompletionTokens != nil && usage != nil {
			// Present but unrepresentable counts cannot validate an earlier
			// snapshot. Retain its evidence and any proven partial status.
			if usage.PrimaryCoverage == UsageCoverageComplete {
				usage.PrimaryCoverage = UsageCoverageUnknown
			}
			usage.CacheReadInputTokens, usage.ReasoningOutputTokens = 0, 0
			usage.CacheReadInputTokensReported, usage.ReasoningOutputTokensReported = false, false
		}
	}

	for event := range events {
		if event.Error != nil {
			if streamErr == nil {
				streamErr = event.Error
			}
			continue
		}
		// Drain the source after a protocol or transport error, but do not treat
		// subsequent frames as authoritative evidence or publish their deltas.
		if streamErr != nil {
			continue
		}
		if canceled {
			continue
		}
		if doneSeen {
			if event.Data != "" {
				streamErr = fmt.Errorf("llm: unexpected SSE data after OpenAI [DONE] marker")
			}
			continue
		}
		if event.Data == "[DONE]" {
			doneSeen = true
			continue
		}
		if event.Data == "" {
			continue
		}
		eventCount++
		if eventCount > GuardedStreamMaxChunks {
			streamErr = fmt.Errorf("llm: OpenAI stream exceeds %d events", GuardedStreamMaxChunks)
			continue
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
			streamErr = fmt.Errorf("llm: decode OpenAI stream event: %w", err)
			continue
		}
		if chunk.Error != nil {
			streamErr = newOpenAIStreamProviderError(statusCode, provider, event.Data, *chunk.Error)
			continue
		}
		if len(chunk.Choices) == 0 && chunk.Usage == nil {
			streamErr = fmt.Errorf("llm: OpenAI stream event has neither choices nor usage")
			continue
		}
		// OpenAI sends usage in a final chunk with an empty choices array
		// when stream_options.include_usage is true.
		if len(chunk.Choices) == 0 {
			acceptUsage(chunk.Usage)
			continue
		}
		if terminalSeen {
			streamErr = fmt.Errorf("llm: OpenAI stream emitted choices after the terminal finish reason")
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
			terminalSeen = true
		}
		delta := choice.Delta
		emittedActivity := false

		if !emittedRole {
			select {
			case ch <- MessageChunk{Role: RoleAssistant, Content: ""}:
				emittedRole = true
				emittedActivity = true
			case <-ctx.Done():
				canceled = true
				continue
			}
		}

		if delta.Content != "" {
			select {
			case ch <- MessageChunk{Role: RoleAssistant, Content: delta.Content}:
				emittedActivity = true
			case <-ctx.Done():
				canceled = true
				continue
			}
		}

		// Accumulate tool_call fragments emitted across chunks.
		previousToolPayloadBytes := toolPayloadBytes
		for ci, tc := range delta.ToolCalls {
			if tc.Index == nil {
				tc.Index = &ci
			}
			idx := *tc.Index
			if idx < 0 || idx >= GuardedStreamMaxToolCalls {
				if streamErr == nil {
					streamErr = fmt.Errorf("llm: OpenAI tool call index %d is outside the supported range", idx)
				}
				continue
			}
			for len(toolArgsBuf) <= idx {
				toolArgsBuf = append(toolArgsBuf, &strings.Builder{})
			}
			if tc.ID != "" {
				if toolIDByIndex == nil {
					toolIDByIndex = make(map[int]string)
				}
				if prior := toolIDByIndex[idx]; prior == "" {
					next, ok := addBoundedStreamBytes(toolPayloadBytes, len(tc.ID))
					if !ok {
						streamErr = fmt.Errorf("llm: OpenAI tool payload exceeds %d-byte limit", GuardedStreamMaxBytes)
						continue
					}
					toolIDByIndex[idx] = tc.ID
					toolPayloadBytes = next
				} else if prior != tc.ID {
					streamErr = fmt.Errorf("llm: OpenAI tool call %d has conflicting IDs", idx)
				}
			}
			if tc.Function.Name != "" {
				if toolNameByIndex == nil {
					toolNameByIndex = make(map[int]string)
				}
				if prior := toolNameByIndex[idx]; prior == "" {
					next, ok := addBoundedStreamBytes(toolPayloadBytes, len(tc.Function.Name))
					if !ok {
						streamErr = fmt.Errorf("llm: OpenAI tool payload exceeds %d-byte limit", GuardedStreamMaxBytes)
						continue
					}
					toolNameByIndex[idx] = tc.Function.Name
					toolPayloadBytes = next
				} else if prior != tc.Function.Name {
					streamErr = fmt.Errorf("llm: OpenAI tool call %d has conflicting names", idx)
				}
			}
			if tc.Function.Arguments != "" {
				next, ok := addBoundedStreamBytes(toolPayloadBytes, len(tc.Function.Arguments))
				if !ok {
					streamErr = fmt.Errorf("llm: OpenAI tool payload exceeds %d-byte limit", GuardedStreamMaxBytes)
					continue
				}
				toolPayloadBytes = next
				toolArgsBuf[idx].WriteString(tc.Function.Arguments)
			}
		}
		// A terminal choice cannot receive more tool fragments. Validate its
		// complete calls before trusting usage attached to that same frame.
		if streamErr == nil && !canceled && choice.FinishReason != "" {
			toolCalls, streamErr = assembleOpenAIStreamToolCalls(toolArgsBuf, toolNameByIndex, toolIDByIndex, finishReason)
		}
		// Accepted reasoning and partial tool calls are real stream activity,
		// but neither is published as assistant text or a premature tool call.
		if streamErr == nil && !canceled && !emittedActivity &&
			(toolPayloadBytes > previousToolPayloadBytes || streamReasoningActivity(delta.Reasoning) || streamReasoningActivity(delta.ReasoningContent)) {
			select {
			case ch <- MessageChunk{Role: RoleAssistant}:
			case <-ctx.Done():
				canceled = true
			}
		}
		if streamErr == nil && !canceled {
			if choice.FinishReason != "" {
				validTerminal = openAITerminalUsageReason(choice.FinishReason)
			}
			// Only valid output frames can make earlier usage a subtotal. A
			// valid usage on this same frame may refresh it immediately.
			if usage != nil && delta.generated {
				usage.PrimaryCoverage = UsageCoveragePartial
				usage.ReasoningOutputTokens = 0
				usage.ReasoningOutputTokensReported = false
			} else if usage != nil && delta.unknownActivity && usage.PrimaryCoverage == UsageCoverageComplete {
				usage.PrimaryCoverage = UsageCoverageUnknown
			}
			acceptUsage(chunk.Usage)
		}
	}
	if ctx.Err() != nil && streamErr == nil {
		streamErr = ctx.Err()
	}
	// If the stream ended before a terminal reason, an otherwise valid early
	// usage snapshot remains evidence, but cannot prove final coverage.
	if usage != nil && !validTerminal && (streamErr != nil || !doneSeen || finishReason == "") && usage.PrimaryCoverage == UsageCoverageComplete {
		usage.PrimaryCoverage = UsageCoverageUnknown
	}
	if streamErr != nil {
		return nil, finishReason, usage, streamErr
	}
	if !doneSeen {
		return nil, finishReason, usage, fmt.Errorf("llm: OpenAI stream ended without [DONE] marker")
	}
	if finishReason == "" {
		return nil, finishReason, usage, fmt.Errorf("llm: OpenAI stream ended without a terminal finish reason")
	}

	return toolCalls, finishReason, usage, nil
}

// openAIFinishReasonStop is the OpenAI reason for a normal or stop-sequence completion.
const openAIFinishReasonStop = "stop"

// IsNormalFinishReason identifies a normal or stop-sequence provider finish.
// It does not establish whether the requested work was completed correctly.
func IsNormalFinishReason(reason string) bool {
	return reason == openAIFinishReasonStop
}

// Unsupported provider-specific reasons remain usable as terminal output, but
// do not by themselves prove that an attached usage snapshot is final.
func openAITerminalUsageReason(reason string) bool {
	switch reason {
	case openAIFinishReasonStop, "length", "tool_calls", "function_call", "content_filter":
		return true
	default:
		return false
	}
}

// streamJSONActivity detects optional reasoning fragments without assuming a
// provider-specific string or structured representation.
func streamJSONActivity(raw []byte) bool {
	value := strings.TrimSpace(string(raw))
	return value != "" && value != "null" && value != `""` && value != "[]" && value != "{}"
}

// streamReasoningActivity accepts nonempty textual or structured reasoning
// fragments without exposing their contents or treating scalar metadata as work.
func streamReasoningActivity(raw []byte) bool {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch fragment := value.(type) {
	case string:
		return len(fragment) > 0
	case []any:
		return len(fragment) > 0
	case map[string]any:
		return len(fragment) > 0
	default:
		return false
	}
}

// assembleOpenAIStreamToolCalls builds the tool-call slice in provider index
// order. Missing IDs, names, or complete JSON arguments are protocol errors;
// only a response
// explicitly truncated for length may carry incomplete arguments onward so
// the loop can discard them before requesting a continuation.
func assembleOpenAIStreamToolCalls(toolArgsBuf []*strings.Builder, toolNameByIndex, toolIDByIndex map[int]string, finishReason string) ([]ToolCall, error) {
	var toolCalls []ToolCall
	if len(toolArgsBuf) > 0 {
		toolCalls = make([]ToolCall, len(toolArgsBuf))
		for idx := range toolArgsBuf {
			name := toolNameByIndex[idx]
			id := toolIDByIndex[idx]
			if name == "" || id == "" {
				return nil, fmt.Errorf("llm: OpenAI tool call %d is missing an ID or name", idx)
			}

			rawArgs := toolArgsBuf[idx].String()
			var args any
			if rawArgs == "" {
				if finishReason != finishReasonLength {
					return nil, fmt.Errorf("llm: OpenAI tool call %q has no arguments", id)
				}
				args = rawArgs
			} else {
				var object map[string]any
				if err := json.Unmarshal([]byte(rawArgs), &object); err != nil {
					if finishReason != finishReasonLength {
						return nil, fmt.Errorf("llm: decode arguments for OpenAI tool call %q: %w", id, err)
					}
					args = rawArgs
				} else if object == nil {
					if finishReason != finishReasonLength {
						return nil, fmt.Errorf("llm: OpenAI tool call %q arguments must be a JSON object", id)
					}
					args = rawArgs
				} else {
					args = object
				}
			}
			toolCalls[idx] = ToolCall{ID: id, Name: name, Args: args}
		}
	}

	return toolCalls, nil
}
