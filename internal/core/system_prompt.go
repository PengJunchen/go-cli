package core //exempt:scan009

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// ContextFile represents a project context file (e.g. AGENTS.md, CLAUDE.md)
// loaded from disk and injected into the system prompt.
type ContextFile struct {
	// Path is the absolute file system path of the context file.
	Path string
	// Content is the raw text content of the file.
	Content string
}

// SkillInfo is a lightweight description of a registered skill, used to
// populate the skills section of the system prompt without pulling in the
// full skill package dependency.
type SkillInfo struct {
	// Name is the unique name of the skill.
	Name string
	// Description summarizes what the skill does and when to use it.
	Description string
	// Category is the coarse grouping the skill belongs to.
	Category string
}

// MemoryEntry is the core-layer representation of a memory, used to inject
// cross-session memories into the system prompt without importing the memory package.
type MemoryEntry struct {
	ID       string
	Content  string
	Category string // preference | fact | decision | convention
}

// SystemPromptOptions carries all the data the SystemPromptBuilder needs to
// assemble the final system prompt string.
type SystemPromptOptions struct {
	// Cwd is the current working directory, included at the end of the
	// prompt so the model knows where it is operating.
	Cwd string
	// Tools is the list of registered tool definitions. Tool names are
	// listed in the prompt and tools implementing PromptGuideliner
	// contribute usage guidelines.
	Tools []tools.ToolDefinition
	// ContextFiles holds project context files (AGENTS.md, CLAUDE.md, etc.)
	// loaded from the file system.
	ContextFiles []ContextFile
	// Skills lists registered skills rendered as XML in the prompt.
	Skills []SkillInfo
	// Memories lists cross-session memories injected as a <memory> XML block
	// into the prompt, grouped by category.
	Memories []MemoryEntry
	// CustomPrompt, when non-empty, replaces the default base prompt
	// entirely. This typically comes from a SYSTEM.md file or the
	// agent.system_prompt config field.
	CustomPrompt string
	// AppendPrompt, when non-empty, is appended to the very end of the
	// assembled system prompt. This typically comes from an
	// APPEND_SYSTEM.md file or the agent.append_system_prompt config field.
	AppendPrompt string
}

// SystemPromptBuilder builds the system prompt dynamically from structured
// options. Implementations assemble the base prompt, tool snippets, tool
// guidelines, project context, skills, and runtime metadata into a single
// string suitable for the LLM system message.
type SystemPromptBuilder interface {
	// Build returns the fully assembled system prompt.
	Build(ctx context.Context, opts SystemPromptOptions) string
}

// defaultBasePrompt is the built-in system prompt used when no custom prompt
// is provided. It is kept in sync with the legacy systemPrompt() function.
const defaultBasePrompt = `You are a helpful AI assistant embedded in a developer CLI. When the user asks you to perform an action, you MUST use the available tools to accomplish it and persist until the task is fully complete.

Rules:
1. NEVER stop with a statement like "Let me install..." or "I need to..." and then do nothing. If you say you will do something, immediately call a tool to do it in the same turn.
2. When a tool call fails or returns an error, diagnose the cause and try an alternative approach. Do not give up after a single failure.
3. Keep iterating (call tools, observe results, adjust) until the user's request is fully satisfied. Only produce a final text answer with NO tool calls when the task is genuinely complete.
4. Do not guess or fabricate information when a tool can provide the answer.
5. If a skill tool is available and relevant, call it first to obtain expert instructions, then follow those instructions using other tools.`

// subagentStrategy is the SubAgent usage guidance injected into the system
// prompt. It teaches the model when to delegate, the available roles, how to
// aggregate results, and the recursion constraints.
const subagentStrategy = `
Sub-Agent Delegation Strategy

You have access to the dispatch_subagent tool, which delegates a task to a focused sub-agent and returns its result. Use sub-agents as an architecture-level parallel capability, not just an occasional convenience.

When to delegate:
- Delegate when a sub-task is self-contained and can be described in a single prompt without requiring your ongoing judgment.
- Delegate research-heavy tasks (codebase exploration, API discovery) to avoid consuming your own context window.
- Delegate parallelizable tasks by setting parallel=true or providing a tasks array, so independent work runs concurrently.
- Do NOT delegate trivial one-liner tasks that you can complete faster with a direct tool call.
- Do NOT delegate when the result requires tight back-and-forth iteration with the user.

Available roles:
1. researcher — investigates and gathers information; returns a structured summary of findings.
2. implementer — writes code following existing patterns; returns the code changes and a brief explanation.
3. reviewer — reviews code changes for correctness, security, performance, and maintainability; returns issues with severity ratings.
4. tester — writes tests focusing on edge cases, error paths, and coverage; returns the test code and coverage notes.

Result aggregation guidelines:
- When dispatching multiple sub-agents in parallel, collect all results, then synthesize them before responding.
- Merge findings into a single coherent answer; do not simply concatenate raw sub-agent outputs.
- If sub-agent results conflict, investigate the discrepancy yourself before presenting the answer.
- Cite which sub-agent (by role) produced each key finding when it adds clarity.

Recursion constraints:
- Sub-agents operate with a recursion depth limit (default 3). A sub-agent may itself dispatch further sub-agents up to this limit.
- Do not attempt to bypass the depth limit by restructuring tasks; instead, break the work into smaller independent pieces.
- Each sub-agent has its own tool whitelist based on its role, so it cannot perform actions outside its scope.`

// DefaultSystemPromptBuilder is the default SystemPromptBuilder implementation.
// It caches the assembled prompt and only rebuilds when the inputs that affect
// the prompt change. It is safe for concurrent use.
type DefaultSystemPromptBuilder struct {
	mu           sync.Mutex
	cachedPrompt string
	cacheVersion string // hash of inputs that affect the prompt
}

// Compile-time assertion that DefaultSystemPromptBuilder satisfies
// SystemPromptBuilder.
var _ SystemPromptBuilder = (*DefaultSystemPromptBuilder)(nil)

// NewDefaultSystemPromptBuilder returns a new DefaultSystemPromptBuilder.
func NewDefaultSystemPromptBuilder() *DefaultSystemPromptBuilder {
	return &DefaultSystemPromptBuilder{}
}

// Build assembles the system prompt in the following order:
//  1. Base prompt (or CustomPrompt if non-empty, which replaces it entirely)
//  2. Tool snippets (list of available tool names)
//  3. Tool guidelines (from tools implementing PromptGuideliner)
//  4. Project context (from ContextFiles)
//  5. Skills (rendered as XML)
//  6. Memories (rendered as a <memory> XML block, grouped by category)
//  7. Current date and working directory
//  8. Append prompt (if non-empty)
//
// The assembled prompt is cached: if the inputs (tools, context files, skills,
// memories, custom/append prompts, cwd, and the calendar day) have not changed
// since the last Build, the cached prompt is returned without rebuilding.
func (b *DefaultSystemPromptBuilder) Build(_ context.Context, opts SystemPromptOptions) string {
	version := computeCacheVersion(opts)

	b.mu.Lock()
	defer b.mu.Unlock()

	if version == b.cacheVersion && b.cachedPrompt != "" {
		slog.Debug("system_prompt.cache", "hit", true)
		return b.cachedPrompt
	}

	slog.Debug("system_prompt.cache", "hit", false)
	prompt := b.buildInner(opts)
	b.cachedPrompt = prompt
	b.cacheVersion = version
	return prompt
}

// computeCacheVersion returns a fixed-length digest that uniquely represents
// all inputs affecting the assembled prompt: tool names, context files,
// skills, memories, custom/append prompts, cwd, and the calendar day. Each
// field is length-prefixed before hashing so that distinct inputs can never
// collide (e.g. one tool named "a,b" vs two tools "a","b", or an empty
// context-file content vs a path equal to another path+content).
//
// Two identical digests guarantee the same prompt output, assuming each
// tool's PromptGuidelines are stable for a given tool name (true for all
// built-in tools — guidelines are intentionally excluded so cache hits avoid
// re-invoking them). The calendar day is included so a long-running process
// rebuilds with the correct date when the day changes.
func computeCacheVersion(opts SystemPromptOptions) string {
	h := sha256.New()
	writeField := func(s string) {
		fmt.Fprintf(h, "%d:%s", len(s), s) //nolint:errcheck
	}
	for _, t := range opts.Tools {
		writeField(t.Name())
	}
	for _, cf := range opts.ContextFiles {
		writeField(cf.Path)
		writeField(cf.Content)
	}
	for _, s := range opts.Skills {
		writeField(s.Name)
		writeField(s.Description)
		writeField(s.Category)
	}
	for _, m := range opts.Memories {
		writeField(m.ID)
		writeField(m.Content)
		writeField(m.Category)
	}
	writeField(opts.CustomPrompt)
	writeField(opts.AppendPrompt)
	writeField(opts.Cwd)
	writeField(time.Now().Format("2006-01-02"))
	return hex.EncodeToString(h.Sum(nil))
}

// buildInner assembles the system prompt from opts without consulting the
// cache. It is called by Build when the cache version does not match.
func (b *DefaultSystemPromptBuilder) buildInner(opts SystemPromptOptions) string {
	var sb strings.Builder

	// 1. Base prompt or custom replacement.
	if opts.CustomPrompt != "" {
		sb.WriteString(opts.CustomPrompt)
	} else {
		sb.WriteString(defaultBasePrompt)
	}

	// 2. Tool snippets: list available tool names.
	if len(opts.Tools) > 0 {
		names := make([]string, 0, len(opts.Tools))
		for _, t := range opts.Tools {
			names = append(names, t.Name())
		}
		sb.WriteString("\n\nYou have access to these tools: ")
		sb.WriteString(strings.Join(names, ", "))
		sb.WriteString(".")
	}

	// 3. Tool guidelines: collect PromptGuideliner hints.
	var guidelines []string
	for _, t := range opts.Tools {
		if g, ok := t.(tools.PromptGuideliner); ok {
			guidelines = append(guidelines, g.PromptGuidelines()...)
		}
	}
	if len(guidelines) > 0 {
		sb.WriteString("\n\nTool guidelines:")
		for _, g := range guidelines {
			sb.WriteString("\n- ")
			sb.WriteString(g)
		}
	}

	// 3b. Sub-Agent delegation strategy.
	sb.WriteString("\n\n")
	sb.WriteString(subagentStrategy)

	// 4. Project context files.
	for _, cf := range opts.ContextFiles {
		if cf.Content == "" {
			continue
		}
		sb.WriteString("\n\n--- ")
		sb.WriteString(cf.Path)
		sb.WriteString(" ---\n")
		sb.WriteString(cf.Content)
	}

	// 5. Skills as XML.
	if len(opts.Skills) > 0 {
		sb.WriteString("\n\n<skills>")
		for _, s := range opts.Skills {
			sb.WriteString(fmt.Sprintf(`<skill name="%s"`, s.Name))
			if s.Category != "" {
				sb.WriteString(fmt.Sprintf(` category="%s"`, s.Category))
			}
			if s.Description != "" {
				sb.WriteString(fmt.Sprintf(` description=%q`, s.Description))
			}
			sb.WriteString(`/>`)
		}
		sb.WriteString("</skills>")
	}

	// 6. Memories as XML, grouped by category.
	if len(opts.Memories) > 0 {
		sb.WriteString("\n\n<memory>")
		memoryCategories := []struct {
			category string
			header   string
		}{
			{"preference", "## User Preferences"},
			{"convention", "## Project Conventions"},
			{"fact", "## Facts"},
			{"decision", "## Decisions"},
		}
		for _, mc := range memoryCategories {
			var lines []string
			for _, m := range opts.Memories {
				if m.Category == mc.category {
					lines = append(lines, "- "+m.Content)
				}
			}
			if len(lines) > 0 {
				sb.WriteString("\n")
				sb.WriteString(mc.header)
				for _, l := range lines {
					sb.WriteString("\n")
					sb.WriteString(l)
				}
			}
		}
		// Any category that is not one of the four known ones.
		var otherLines []string
		for _, m := range opts.Memories {
			switch m.Category {
			case "preference", "convention", "fact", "decision":
				continue
			default:
				otherLines = append(otherLines, "- "+m.Content)
			}
		}
		if len(otherLines) > 0 {
			sb.WriteString("\n## Other")
			for _, l := range otherLines {
				sb.WriteString("\n")
				sb.WriteString(l)
			}
		}
		sb.WriteString("\n</memory>")
	}

	// 7. Current date and working directory.
	sb.WriteString("\n\nCurrent date: ")
	sb.WriteString(time.Now().Format("2006-01-02"))
	if opts.Cwd != "" {
		sb.WriteString("\nCurrent working directory: ")
		sb.WriteString(opts.Cwd)
	}

	// 8. Append prompt.
	if opts.AppendPrompt != "" {
		sb.WriteString("\n\n")
		sb.WriteString(opts.AppendPrompt)
	}

	return sb.String()
}
