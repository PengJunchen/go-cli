package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// accumulateOpenAIStreamToolCalls processes SSE events from an OpenAI-compatible
// stream, accumulating tool-call fragments and forwarding content chunks to ch.
// It emits a role-only MessageChunk on the first content or tool-call delta
// (matching streamClaude's message_start behavior) so callers can render an
// assistant placeholder early. It returns an error unless the stream reached
// the required [DONE] marker with no parser or payload errors.
func accumulateOpenAIStreamToolCalls(ctx context.Context, events <-chan SSEEvent, ch chan<- MessageChunk) (toolCalls []ToolCall, finishReason string, usage *Usage, err error) {
	var toolNameByIndex map[int]string
	var toolIDByIndex map[int]string
	var toolArgsBuf []*strings.Builder
	emittedRole := false
	canceled := false
	doneSeen := false
	terminalSeen := false
	eventCount := 0
	toolPayloadBytes := 0
	var streamErr error

	for event := range events {
		if event.Error != nil {
			if streamErr == nil {
				streamErr = event.Error
			}
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
			if streamErr == nil {
				streamErr = fmt.Errorf("llm: decode OpenAI stream event: %w", err)
			}
			continue
		}
		if len(chunk.Choices) == 0 && chunk.Usage == nil {
			if streamErr == nil {
				streamErr = fmt.Errorf("llm: OpenAI stream event has neither choices nor usage")
			}
			continue
		}
		// OpenAI sends usage in a final chunk with an empty choices array
		// when stream_options.include_usage is true.
		if chunk.Usage != nil {
			usage = chunk.Usage.toUsage()
		}
		if len(chunk.Choices) == 0 {
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
		// Keep the chunk-idle watchdog informed of accepted tool activity,
		// without publishing partial tool calls or fabricated assistant text.
		if streamErr == nil && toolPayloadBytes > previousToolPayloadBytes && !emittedActivity {
			select {
			case ch <- MessageChunk{Role: RoleAssistant}:
			case <-ctx.Done():
				canceled = true
			}
		}
	}
	if ctx.Err() != nil && streamErr == nil {
		streamErr = ctx.Err()
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

	// Build the final tool-call slice in provider index order. Missing IDs,
	// names, or complete JSON arguments are protocol errors; only a response
	// explicitly truncated for length may carry incomplete arguments onward so
	// the loop can discard them before requesting a continuation.
	if len(toolArgsBuf) > 0 {
		toolCalls = make([]ToolCall, len(toolArgsBuf))
		for idx := range toolArgsBuf {
			name := toolNameByIndex[idx]
			id := toolIDByIndex[idx]
			if name == "" || id == "" {
				return nil, finishReason, usage, fmt.Errorf("llm: OpenAI tool call %d is missing an ID or name", idx)
			}

			rawArgs := toolArgsBuf[idx].String()
			var args any
			if rawArgs == "" {
				if finishReason != finishReasonLength {
					return nil, finishReason, usage, fmt.Errorf("llm: OpenAI tool call %q has no arguments", id)
				}
				args = rawArgs
			} else {
				var object map[string]any
				if err := json.Unmarshal([]byte(rawArgs), &object); err != nil {
					if finishReason != finishReasonLength {
						return nil, finishReason, usage, fmt.Errorf("llm: decode arguments for OpenAI tool call %q: %w", id, err)
					}
					args = rawArgs
				} else if object == nil {
					if finishReason != finishReasonLength {
						return nil, finishReason, usage, fmt.Errorf("llm: OpenAI tool call %q arguments must be a JSON object", id)
					}
					args = rawArgs
				} else {
					args = object
				}
			}
			toolCalls[idx] = ToolCall{ID: id, Name: name, Args: args}
		}
	}

	return toolCalls, finishReason, usage, nil
}
