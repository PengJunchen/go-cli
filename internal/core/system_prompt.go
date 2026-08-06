package core //exempt:scan009

import (
	"context"
	"fmt"
	"strings"
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

// DefaultSystemPromptBuilder is the default SystemPromptBuilder implementation.
// It is stateless and safe for concurrent use.
type DefaultSystemPromptBuilder struct{}

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
//  6. Current date and working directory
//  7. Append prompt (if non-empty)
func (b *DefaultSystemPromptBuilder) Build(_ context.Context, opts SystemPromptOptions) string {
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

	// 6. Current date and working directory.
	sb.WriteString("\n\nCurrent date: ")
	sb.WriteString(time.Now().Format("2006-01-02"))
	if opts.Cwd != "" {
		sb.WriteString("\nCurrent working directory: ")
		sb.WriteString(opts.Cwd)
	}

	// 7. Append prompt.
	if opts.AppendPrompt != "" {
		sb.WriteString("\n\n")
		sb.WriteString(opts.AppendPrompt)
	}

	return sb.String()
}
