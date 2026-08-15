package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// ContextHandler implements the /context command, which displays a token
// breakdown of the current context window by category: system prompt, tool
// definitions, CLAUDE.md (project context files), memories, and conversation
// messages. Each category shows its estimated token count and percentage of
// the total context window. The total reflects current context window
// occupancy, not cumulative API calls.
type ContextHandler struct{}

var _ SlashCommandHandler = (*ContextHandler)(nil)

func (h *ContextHandler) Name() string        { return "context" }
func (h *ContextHandler) Description() string { return "Show token breakdown of the current context window" }

func (h *ContextHandler) Handle(ctx context.Context, _ []string, deps Dependencies) (string, error) {
	out := deps.Out()

	estimator := deps.Estimator()
	if estimator == nil {
		estimator = compaction.NewHeuristicTokenEstimator()
	}

	contextWindow := deps.ContextWindow()
	if contextWindow <= 0 {
		fmt.Fprintln(out, "Context window size not configured.") //nolint:errcheck
		return "", nil
	}

	// Gather each context component and estimate its token count.
	systemPromptTokens := estimateSystemPrompt(ctx, deps, estimator)
	toolDefTokens := estimateToolDefinitions(ctx, deps, estimator)
	claudeMDTokens := estimateContextFiles(ctx, deps, estimator)
	memoryTokens := estimateMemories(ctx, deps, estimator)
	messageTokens := estimateMessages(deps, estimator)

	total := systemPromptTokens + toolDefTokens + claudeMDTokens + memoryTokens + messageTokens
	remaining := contextWindow - total
	if remaining < 0 {
		remaining = 0
	}

	// Print breakdown table.
	fmt.Fprintln(out, "Context window breakdown:") //nolint:errcheck
	fmt.Fprintln(out)                               //nolint:errcheck

	printContextRow(out, "System prompt", systemPromptTokens, contextWindow)
	printContextRow(out, "Tool definitions", toolDefTokens, contextWindow)
	printContextRow(out, "CLAUDE.md / context files", claudeMDTokens, contextWindow)
	printContextRow(out, "Memories", memoryTokens, contextWindow)
	printContextRow(out, "Messages", messageTokens, contextWindow)

	fmt.Fprintln(out) //nolint:errcheck
	pct := 0
	if contextWindow > 0 {
		pct = total * 100 / contextWindow
	}
	fmt.Fprintf(out, "Total:     %d tokens (%d%% of %d)\n", total, pct, contextWindow) //nolint:errcheck
	fmt.Fprintf(out, "Remaining: %d tokens\n", remaining)                                //nolint:errcheck

	return "", nil
}

// printContextRow prints a single row of the context breakdown table,
// right-aligning the token count and showing the percentage of the context
// window.
func printContextRow(out io.Writer, label string, tokens, window int) {
	pct := 0
	if window > 0 {
		pct = tokens * 100 / window
	}
	fmt.Fprintf(out, "  %-28s %6d tokens  (%d%%)\n", label, tokens, pct) //nolint:errcheck
}

// estimateSystemPrompt builds the system prompt (excluding context files and
// memories, which are measured separately) and estimates its token count.
// When the PromptBuilder is not available, it returns 0.
func estimateSystemPrompt(ctx context.Context, deps Dependencies, estimator compaction.TokenEstimator) int {
	builder := deps.PromptBuilder()
	if builder == nil {
		return 0
	}

	// Build the system prompt with empty ContextFiles and Memories so that
	// those categories are not double-counted. The resulting prompt includes
	// the base prompt, tool names/guidelines, subagent strategy, skills, and
	// date/cwd — everything except CLAUDE.md and memories.
	var toolDefs []tools.ToolDefinition
	if deps.ToolRegistry() != nil {
		if defs, err := deps.ToolRegistry().List(ctx); err == nil {
			toolDefs = defs
		}
	}
	cwd, _ := os.Getwd() //nolint:errcheck
	prompt := builder.Build(ctx, core.SystemPromptOptions{
		Cwd:          cwd,
		Tools:        toolDefs,
		ContextFiles: nil, // measured separately as "CLAUDE.md"
		Memories:     nil, // measured separately as "Memories"
	})
	n, _ := estimator.Estimate(prompt)
	return n
}

// estimateToolDefinitions serializes each tool's name, description, and
// parameters schema to JSON and estimates the combined token count. This
// approximates the tokens consumed by the `tools` field in the API request.
func estimateToolDefinitions(ctx context.Context, deps Dependencies, estimator compaction.TokenEstimator) int {
	if deps.ToolRegistry() == nil {
		return 0
	}
	defs, err := deps.ToolRegistry().List(ctx)
	if err != nil || len(defs) == 0 {
		return 0
	}

	var sb strings.Builder
	for _, def := range defs {
		entry := map[string]any{
			"name":        def.Name(),
			"description": def.Description(),
		}
		if p, ok := def.(tools.Parameterized); ok {
			entry["parameters"] = p.Parameters()
		}
		data, _ := json.Marshal(entry) //nolint:errcheck
		sb.Write(data)
		sb.WriteByte('\n')
	}
	n, _ := estimator.Estimate(sb.String())
	return n
}

// estimateContextFiles loads project context files (AGENTS.md, CLAUDE.md,
// etc.) and estimates their combined token count.
func estimateContextFiles(ctx context.Context, deps Dependencies, estimator compaction.TokenEstimator) int {
	loader := deps.ContextLoader()
	if loader == nil {
		return 0
	}
	cwd, _ := os.Getwd() //nolint:errcheck
	files, err := loader.Load(ctx, cwd)
	if err != nil || len(files) == 0 {
		return 0
	}

	var sb strings.Builder
	for _, cf := range files {
		if cf.Content == "" {
			continue
		}
		sb.WriteString(cf.Content)
		sb.WriteByte('\n')
	}
	n, _ := estimator.Estimate(sb.String())
	return n
}

// estimateMemories lists all memories from the memory store and estimates
// their combined token count.
func estimateMemories(ctx context.Context, deps Dependencies, estimator compaction.TokenEstimator) int {
	store := deps.MemoryStore()
	if store == nil {
		return 0
	}
	memories, err := store.List(ctx)
	if err != nil || len(memories) == 0 {
		return 0
	}

	var sb strings.Builder
	for _, m := range memories {
		sb.WriteString(m.Content)
		sb.WriteByte('\n')
	}
	n, _ := estimator.Estimate(sb.String())
	return n
}

// estimateMessages sums the estimated token count of all conversation
// messages in the agent's history.
func estimateMessages(deps Dependencies, estimator compaction.TokenEstimator) int {
	if deps.Agent() == nil {
		return 0
	}
	msgs := deps.Agent().Messages()
	if len(msgs) == 0 {
		return 0
	}

	var sb strings.Builder
	for _, msg := range msgs {
		sb.WriteString(msg.Content)
		sb.WriteByte('\n')
	}
	n, _ := estimator.Estimate(sb.String())
	return n
}
