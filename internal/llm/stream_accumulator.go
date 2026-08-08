package llm

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// accumulateOpenAIStreamToolCalls processes SSE events from an OpenAI-compatible
// stream, accumulating tool-call fragments and forwarding content chunks to ch.
// It emits a role-only MessageChunk on the first content or tool-call delta
// (matching streamClaude's message_start behaviour) so callers can render an
// assistant placeholder early. Returns the accumulated tool calls (nil when no
// tool calls were seen), the finish reason from the stream, and the usage
// reported by the API (nil when the stream did not include usage).
func accumulateOpenAIStreamToolCalls(events <-chan SSEEvent, ch chan<- MessageChunk) (toolCalls []ToolCall, finishReason string, usage *Usage) {
	var toolNameByIndex map[int]string
	var toolArgsBuf []string
	emittedRole := false

	for event := range events {
		if event.Data == "[DONE]" {
			break
		}
		if event.Data == "" {
			continue
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
			slog.Warn("llm_stream_parse_skip", "err", err)
			continue
		}
		// OpenAI sends usage in a final chunk with an empty choices array
		// when stream_options.include_usage is true.
		if chunk.Usage != nil {
			usage = &Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.PromptTokens + chunk.Usage.CompletionTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
		}
		delta := choice.Delta

		if !emittedRole {
			ch <- MessageChunk{Role: RoleAssistant, Content: ""}
			emittedRole = true
		}

		if delta.Content != "" {
			ch <- MessageChunk{Role: RoleAssistant, Content: delta.Content}
		}

		// Accumulate tool_call fragments emitted across chunks.
		for ci, tc := range delta.ToolCalls {
			if tc.Index == nil {
				tc.Index = &ci
			}
			idx := *tc.Index
			for len(toolArgsBuf) <= idx {
				toolArgsBuf = append(toolArgsBuf, "")
			}
			if tc.Function.Name != "" {
				if toolNameByIndex == nil {
					toolNameByIndex = make(map[int]string)
				}
				if toolNameByIndex[idx] == "" {
					toolNameByIndex[idx] = tc.Function.Name
				}
			}
			if tc.Function.Arguments != "" {
				toolArgsBuf[idx] += tc.Function.Arguments
			}
		}
	}

	// Build the final tool-call slice from accumulated fragments.
	if toolNameByIndex != nil {
		toolCalls = make([]ToolCall, len(toolArgsBuf))
		for idx, name := range toolNameByIndex {
			var args any
			if idx < len(toolArgsBuf) && toolArgsBuf[idx] != "" {
				var decoded any
				if err := json.Unmarshal([]byte(toolArgsBuf[idx]), &decoded); err == nil {
					args = decoded
				} else {
					args = toolArgsBuf[idx]
				}
			}
			toolCalls[idx] = ToolCall{
				ID:   fmt.Sprintf("call_%d", idx),
				Name: name,
				Args: args,
			}
		}
	}

	return toolCalls, finishReason, usage
}
