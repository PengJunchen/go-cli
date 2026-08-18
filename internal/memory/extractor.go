//exempt:scan012
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// MemoryExtractor extracts key facts from conversations.
type MemoryExtractor interface {
	Extract(ctx context.Context, messages []llm.Message) ([]Memory, error)
}

// LLMMemoryExtractor implements MemoryExtractor using an LLM to analyze
// conversations and extract key facts. Extracted memories are deduplicated
// against the store but are NOT written to it; the caller decides what to
// persist.
type LLMMemoryExtractor struct {
	model llm.BaseChatModel
	store MemoryStore
}

// NewLLMMemoryExtractor creates an LLMMemoryExtractor backed by the given model
// and store. The store is used only for deduplication during extraction.
func NewLLMMemoryExtractor(model llm.BaseChatModel, store MemoryStore) *LLMMemoryExtractor {
	return &LLMMemoryExtractor{model: model, store: store}
}

// Compile-time assertion that LLMMemoryExtractor satisfies MemoryExtractor.
var _ MemoryExtractor = (*LLMMemoryExtractor)(nil)

const extractionPrompt = `Analyze the following conversation and extract key facts that would be useful for future sessions.
Focus on:
- User preferences (how the user likes things done)
- Project conventions (coding standards, patterns used)
- Important decisions made
- Key facts learned

Return ONLY a JSON array, no other text. Each element should have:
- "content": the fact (concise, actionable)
- "category": one of "preference", "fact", "decision", "convention"

If no memorable facts are found, return an empty array [].

Conversation:
`

// extractedFact is the JSON shape produced by the extraction prompt.
type extractedFact struct {
	Content  string `json:"content"`
	Category string `json:"category"`
}

// Extract analyzes the given conversation and returns extracted memories.
// It returns an empty slice (no error) when the conversation is too short or
// when the model response cannot be parsed as JSON. Extracted memories are
// deduplicated against existing memories in the store (exact content match)
// but are NOT written to the store.
func (e *LLMMemoryExtractor) Extract(ctx context.Context, messages []llm.Message) ([]Memory, error) {
	if len(messages) < 2 {
		return []Memory{}, nil
	}

	prompt := extractionPrompt + formatConversation(messages)
	extractionMessages := []llm.Message{
		{Role: llm.RoleUser, Content: prompt},
	}

	ctx = llm.WithTaskType(ctx, llm.TaskTypeExtraction)
	resp, err := e.model.Generate(ctx, extractionMessages)
	if err != nil {
		return nil, fmt.Errorf("memory: extract: %w", err)
	}
	if resp == nil {
		return []Memory{}, nil
	}

	facts, err := parseExtractionResponse(resp.Content)
	if err != nil {
		// Graceful degradation: unparsable JSON yields no memories, no error.
		// Log the failure so it is not silently swallowed.
		slog.Warn("memory: failed to parse extraction response as JSON",
			"err", err,
			"response", resp.Content,
		)
		return []Memory{}, nil
	}

	existing, err := e.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("memory: list existing: %w", err)
	}

	seen := make(map[string]struct{}, len(existing))
	for _, m := range existing {
		seen[m.Content] = struct{}{}
	}

	out := make([]Memory, 0, len(facts))
	for _, f := range facts {
		if f.Content == "" {
			continue
		}
		if _, dup := seen[f.Content]; dup {
			continue
		}
		seen[f.Content] = struct{}{}
		out = append(out, Memory{
			Content:  f.Content,
			Category: f.Category,
			Source:   "auto",
		})
	}
	return out, nil
}

// formatConversation renders the conversation messages as text for the
// extraction prompt.
func formatConversation(messages []llm.Message) string {
	var b strings.Builder
	for _, m := range messages {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content) //nolint:errcheck
	}
	return b.String()
}

// parseExtractionResponse parses the JSON array returned by the model. On any
// parse failure it returns an error, allowing the caller to degrade gracefully.
func parseExtractionResponse(content string) ([]extractedFact, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, fmt.Errorf("empty response")
	}

	// Strip markdown code block wrapping (```json ... ``` or ``` ... ```).
	if strings.HasPrefix(trimmed, "```") {
		// Remove opening fence.
		if idx := strings.Index(trimmed[3:], "\n"); idx >= 0 {
			trimmed = trimmed[3+idx+1:]
		}
		// Remove closing fence.
		if idx := strings.LastIndex(trimmed, "```"); idx >= 0 {
			trimmed = trimmed[:idx]
		}
		trimmed = strings.TrimSpace(trimmed)
	}

	var facts []extractedFact
	if err := json.Unmarshal([]byte(trimmed), &facts); err != nil {
		return nil, err
	}
	return facts, nil
}
