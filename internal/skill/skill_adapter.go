package skill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// SkillAdapter wraps a SkillDefinition as a tools.ToolDefinition so the agent
// loop can execute a skill as if it were a tool. Invoking the adapter runs the
// skill's prompt and surfaces its declared tools and parameters through the
// description.
type SkillAdapter struct {
	def SkillDefinition
}

// Compile-time assertion that SkillAdapter satisfies tools.ToolDefinition.
var _ tools.ToolDefinition = (*SkillAdapter)(nil)

// NewSkillAdapter wraps def as a tool. def must not be nil.
func NewSkillAdapter(def SkillDefinition) *SkillAdapter {
	return &SkillAdapter{def: def}
}

// Name returns the skill name, which serves as the tool name.
func (a *SkillAdapter) Name() string { return a.def.Name() }

// Description returns the skill description augmented with the tool list and
// parameter names the skill declares.
func (a *SkillAdapter) Description() string {
	var sb strings.Builder
	sb.WriteString(a.def.Description())
	if toolsSlice := a.def.Tools(); len(toolsSlice) > 0 {
		sb.WriteString("\ntools: ")
		sb.WriteString(strings.Join(toolsSlice, ", "))
	}
	if params := a.def.Parameters(); len(params) > 0 {
		names := make([]string, 0, len(params))
		for k := range params {
			names = append(names, k)
		}
		sort.Strings(names)
		sb.WriteString("\nparameters: ")
		sb.WriteString(strings.Join(names, ", "))
	}
	return sb.String()
}

// Execute runs the skill by returning its prompt as the tool output. It emits
// a `skill.execute` span with the skill name and success status.
func (a *SkillAdapter) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "skill.execute", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	if a.def == nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, "skill: nil definition")
		return nil, errors.New("skill: cannot execute a nil skill definition")
	}

	if err := spanCtx.Err(); err != nil {
		span.SetAttributes(
			tracing.Attribute{Key: "skill_name", Value: ""},
			tracing.Attribute{Key: "success", Value: false},
		)
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, err
	}

	name := a.def.Name()
	prompt := a.def.Prompt()
	span.SetAttributes(
		tracing.Attribute{Key: "skill_name", Value: name},
		tracing.Attribute{Key: "success", Value: true},
	)
	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("skill.execute",
		"skill", name,
		"tool_call_id", call.ID,
		"prompt_chars", len(prompt))

	return &tools.ToolResult{
		Output: fmt.Sprintf("[skill %s]\n%s", name, prompt),
		Metadata: map[string]any{
			"skill":      name,
			"tools":      a.def.Tools(),
			"parameters": a.def.Parameters(),
		},
		ToolCallID: call.ID,
	}, nil
}
