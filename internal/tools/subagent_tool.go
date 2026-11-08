package tools //exempt:scan009

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// requestIDCounter guarantees unique request ids across concurrent callers.
var requestIDCounter atomic.Uint64

// SubagentTask describes a unit of work delegated to a sub-agent. This is the
// tools-package consumer-side contract; it mirrors core.SubagentTask. A local
// copy is required because the tools package cannot import core (core already
// depends on tools), so the tool depends on its own interface and the core
// package provides an adapter (core.AdaptSubagentDispatcher).
type SubagentTask struct {
	// ContextMode selects independent (default) or inherited conversation data.
	ContextMode string
	// ID uniquely identifies the task and names the spawned sub-agent.
	ID string
	// Prompt is the instruction the sub-agent executes.
	Prompt string
	// SystemPrompt is an explicit system prompt for the sub-agent. When empty,
	// Role (if set) selects a role template; otherwise the dispatcher applies a
	// default. system_prompt takes precedence over role.
	SystemPrompt string
	// Role optionally selects a role template (researcher, implementer,
	// reviewer, tester) when SystemPrompt is empty.
	Role string
	// Tools lists the tool names available to the sub-agent.
	Tools []string
	// Model optionally overrides the LLM model used by the sub-agent. When
	// empty, the sub-agent inherits the parent model.
	Model string
	// MaxTurns bounds the sub-agent turn loop. Zero leaves it unset.
	MaxTurns int
}

// SubagentResult is the outcome of dispatching a SubagentTask.
type SubagentResult struct {
	// PhysicalSessionID identifies an independently resumable managed child.
	PhysicalSessionID string
	// TaskID links the result back to the originating task.
	TaskID string
	// Content is the final message produced by the sub-agent.
	Content string
	// Error carries any error the sub-agent reported.
	Error error
	// Duration is how long the dispatch took end to end.
	Duration time.Duration
}

// SubagentDispatcher manages sub-agent lifecycle and task dispatch. It is the
// tools-package consumer-side contract; core.DefaultSubagentDispatcher plus
// core.AdaptSubagentDispatcher satisfy it.
//
//exempt:scan012 // consumer-side interface; default impl in core package
type SubagentDispatcher interface {
	// Dispatch creates a sub-agent for the task, runs it to completion, and
	// returns the result.
	Dispatch(ctx context.Context, task SubagentTask) (SubagentResult, error)
	// ParallelDispatch dispatches all tasks concurrently and returns results
	// in input order. The first error encountered is returned (if any),
	// though all tasks are still attempted.
	ParallelDispatch(ctx context.Context, tasks []SubagentTask) ([]SubagentResult, error)
	// ListRunning returns the tasks currently in flight.
	ListRunning() []SubagentTask
}

// SubagentTool is a ToolDefinition that delegates a prompt to a sub-agent via
// a SubagentDispatcher.
type SubagentTool struct {
	dispatcher SubagentDispatcher
}

var _ ToolDefinition = (*SubagentTool)(nil)
var _ Parameterized = (*SubagentTool)(nil)
var _ PromptGuideliner = (*SubagentTool)(nil)

// NewSubagentTool builds a SubagentTool backed by dispatcher.
func NewSubagentTool(dispatcher SubagentDispatcher) *SubagentTool {
	return &SubagentTool{dispatcher: dispatcher}
}

// Name returns the tool name.
func (t *SubagentTool) Name() string { return "dispatch_subagent" }

// Description returns guidance for the model on when and how to delegate work
// to a sub-agent.
func (t *SubagentTool) Description() string {
	return "dispatch_subagent: delegates a task to a focused sub-agent and returns its final answer.\n" +
		"\n" +
		"Use this tool to delegate a task to a focused sub-agent when the work is self-contained (e.g. research, implementation, review, or testing).\n" +
		"For independent investigations, include the objective, observed reproduction or failure evidence, expected versus actual behavior, locations already checked, constraints and prohibitions, and questions to answer. Mark missing details unknown; do not invent facts.\n" +
		"\n" +
		"Args:\n" +
		"- prompt (string, required for a single task): a clear, specific task description. Write it so the sub-agent can execute without extra context.\n" +
		"- id (string, optional): a unique task identifier; generated when omitted.\n" +
		"- system_prompt (string, optional): an explicit system prompt for the sub-agent. Takes precedence over role.\n" +
		"- role (string, optional): one of researcher, implementer, reviewer, tester. Selects a built-in role template when system_prompt is empty.\n" +
		"- tools ([]string, optional): tool names available to the sub-agent.\n" +
		"- model (string, optional): model name to override the sub-agent's LLM. When omitted, the sub-agent inherits the parent model.\n" +
		"- max_turns (int, optional): bound on the sub-agent turn loop.\n" +
		"- context_mode (string, optional): independent (default) or inherit parent conversation as reference data.\n" +
		"- execution_mode (string, optional): parallel or sequential for a tasks array.\n" +
		"Choose sequential for shared-workspace writes or dependent stages. Later sub-agents can read shared files but do not automatically receive earlier answers.\n" +
		"\n" +
		"The sub-agent will return its result as content. When present, Child physical session ID (physical_session_id) is the child's physical session ID and can be restored with --resume-session <physical_session_id>."
}

// Parameters returns the OpenAI-compatible JSON Schema describing the tool's
// input parameters.
func (t *SubagentTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"context_mode": subagentContextModeSchema(),
			"prompt": map[string]any{
				"type":        "string",
				"description": "A clear, specific task description. Write it so the sub-agent can execute without extra context.",
			},
			"role": map[string]any{
				"type":        "string",
				"enum":        []string{"researcher", "implementer", "reviewer", "tester"},
				"description": "Selects a built-in role template when system_prompt is empty.",
			},
			"tools": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Tool names available to the sub-agent. When omitted, the role-based whitelist is used.",
			},
			"model": map[string]any{
				"type":        "string",
				"description": "Model name to override the sub-agent's LLM. When omitted, inherits the parent model.",
			},
			"max_turns": map[string]any{
				"type":        "integer",
				"description": "Bound on the sub-agent turn loop. Zero leaves it unset.",
			},
			"parallel": map[string]any{
				"type":        "boolean",
				"description": "When true, dispatch the tasks array concurrently instead of a single prompt.",
			},
			"execution_mode": map[string]any{
				"type":        "string",
				"enum":        []string{"parallel", "sequential"},
				"description": "Select sequential for dependent tasks or shared-workspace writes; later tasks do not automatically receive earlier answers.",
			},
			"tasks": map[string]any{
				"type":        "array",
				"description": "Array of tasks for parallel or sequential mode. Each task has prompt, role, tools, model, and max_turns fields.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"context_mode": subagentContextModeSchema(),
						"prompt": map[string]any{
							"type":        "string",
							"description": "Task description for this sub-task.",
						},
						"role": map[string]any{
							"type":        "string",
							"enum":        []string{"researcher", "implementer", "reviewer", "tester"},
							"description": "Role for this sub-task.",
						},
						"tools": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "Tools for this sub-task.",
						},
						"model": map[string]any{
							"type":        "string",
							"description": "Model override for this sub-task.",
						},
						"max_turns": map[string]any{
							"type":        "integer",
							"description": "Turn limit for this sub-task.",
						},
					},
				},
			},
			"system_prompt": map[string]any{
				"type":        "string",
				"description": "An explicit system prompt for the sub-agent. Takes precedence over role.",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "A unique task identifier; generated when omitted.",
			},
		},
		"required":             []string{},
		"additionalProperties": false,
	}
}

// PromptGuidelines returns usage hints injected into the system prompt.
func (t *SubagentTool) PromptGuidelines() []string {
	return []string{
		"Use dispatch_subagent to delegate self-contained tasks to a focused sub-agent (roles: researcher, implementer, reviewer, tester)",
		"Set parallel=true with a tasks array to run independent sub-tasks concurrently and aggregate their results",
		"Set execution_mode=sequential for sub-agent tasks with shared-workspace writes or stage dependencies; later tasks use shared files rather than automatically receiving earlier answers",
		"Use the returned Child physical session ID (physical_session_id) with --resume-session <physical_session_id> to restore that sub-agent's physical session",
		"Avoid dispatching trivial tasks that a single direct tool call can handle",
	}
}

// Execute parses a sub-agent task from call.Args, dispatches it via the
// dispatcher, and returns the sub-agent's final answer. A tasks array uses
// parallel execution by default, unless execution_mode is sequential; in that
// mode each child finishes before the next starts.
func (t *SubagentTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if err := validateSubagentContextMode(call.Args); err != nil {
		return nil, err
	}
	if value, present := call.Args["execution_mode"]; present {
		mode, ok := value.(string)
		if !ok || (mode != "parallel" && mode != "sequential") {
			return nil, errors.New("dispatch_subagent: execution_mode must be parallel or sequential")
		}
		if value, present := call.Args["parallel"]; present {
			parallel, ok := value.(bool)
			if !ok {
				return nil, errors.New("dispatch_subagent: parallel must be a boolean when execution_mode is set")
			}
			if mode == "sequential" && parallel {
				return nil, errors.New("dispatch_subagent: sequential execution conflicts with parallel=true")
			}
		}
		if mode == "sequential" {
			return t.executeSequential(ctx, call)
		}
		return t.executeParallel(ctx, call, call.Args["tasks"])
	}
	parallel, _ := call.Args["parallel"].(bool) //nolint:errcheck
	tasksRaw := call.Args["tasks"]

	// Parallel mode: dispatch multiple tasks concurrently.
	if parallel || tasksRaw != nil {
		return t.executeParallel(ctx, call, tasksRaw)
	}

	prompt, ok := call.Args["prompt"].(string)
	if !ok || prompt == "" {
		return nil, errors.New("dispatch_subagent: missing string argument 'prompt'")
	}

	id, _ := call.Args["id"].(string) //nolint:errcheck
	if id == "" {
		id = nextRequestID("task")
	}

	// system_prompt is optional and takes precedence over role. role selects a
	// built-in role template (resolved by the dispatcher) when system_prompt is
	// empty. Both are passed through unchanged; the dispatcher applies the
	// default when neither is set.
	systemPrompt, _ := call.Args["system_prompt"].(string) //nolint:errcheck
	role, _ := call.Args["role"].(string)                  //nolint:errcheck
	model, _ := call.Args["model"].(string)                //nolint:errcheck

	task := SubagentTask{
		ID:           id,
		Prompt:       prompt,
		SystemPrompt: systemPrompt,
		Role:         role,
		Tools:        toStringSlice(call.Args["tools"]),
		Model:        model,
		MaxTurns:     toInt(call.Args["max_turns"]),
		ContextMode:  getString(call.Args, "context_mode"),
	}

	slog.Debug("tools.subagent.execute",
		"task_id", task.ID,
		"prompt_len", len(task.Prompt),
		"tools", len(task.Tools),
		"max_turns", task.MaxTurns,
	)

	res, err := t.dispatcher.Dispatch(ctx, task)
	if err != nil {
		return &ToolResult{Output: subagentContentWithSession(res), Metadata: map[string]any{"task_id": res.TaskID, "physical_session_id": res.PhysicalSessionID}}, fmt.Errorf("dispatch_subagent: %w%s", err, subagentSessionLine(res.PhysicalSessionID))
	}

	metadata := map[string]any{
		"task_id":             res.TaskID,
		"physical_session_id": res.PhysicalSessionID,
		"duration":            res.Duration,
	}
	if res.Error != nil {
		metadata["error"] = res.Error.Error()
		return &ToolResult{Output: subagentContentWithSession(res), Metadata: metadata}, fmt.Errorf("dispatch_subagent: %w%s", res.Error, subagentSessionLine(res.PhysicalSessionID))
	}

	return &ToolResult{Output: subagentContentWithSession(res), Metadata: metadata}, nil
}

// sequentialTasks validates the entire new-mode batch before any task starts.
// Legacy parallel parsing intentionally keeps its existing permissive behavior.
func sequentialTasks(call ToolCall) ([]map[string]any, []int, error) {
	raw, ok := toAnySlice(call.Args["tasks"])
	if !ok || len(raw) == 0 {
		return nil, nil, errors.New("dispatch_subagent: sequential mode requires a non-empty tasks array")
	}
	tasks := make([]map[string]any, 0, len(raw))
	turns := make([]int, 0, len(raw))
	for i, value := range raw {
		task, ok := value.(map[string]any)
		if !ok || task == nil {
			return nil, nil, fmt.Errorf("dispatch_subagent: task %d must be an object", i+1)
		}
		prompt, ok := task["prompt"].(string)
		if !ok || strings.TrimSpace(prompt) == "" {
			return nil, nil, fmt.Errorf("dispatch_subagent: task %d requires a non-empty prompt", i+1)
		}
		for _, key := range []string{"id", "system_prompt", "model"} {
			if value, present := task[key]; present {
				if _, ok := value.(string); !ok {
					return nil, nil, fmt.Errorf("dispatch_subagent: task %d %s must be a string", i+1, key)
				}
			}
		}
		if value, present := task["role"]; present {
			role, ok := value.(string)
			if !ok || (role != "researcher" && role != "implementer" && role != "reviewer" && role != "tester") {
				return nil, nil, fmt.Errorf("dispatch_subagent: task %d role is invalid", i+1)
			}
		}
		if err := validateSubagentContextMode(task); err != nil {
			return nil, nil, fmt.Errorf("dispatch_subagent: task %d: %w", i+1, err)
		}
		if value, present := task["tools"]; present {
			switch values := value.(type) {
			case []string:
			case []any:
				for _, item := range values {
					if _, ok := item.(string); !ok {
						return nil, nil, fmt.Errorf("dispatch_subagent: task %d tools must contain strings", i+1)
					}
				}
			default:
				return nil, nil, fmt.Errorf("dispatch_subagent: task %d tools must be an array", i+1)
			}
		}
		maxTurns := 0
		if value, present := task["max_turns"]; present {
			number, ok := schemaRational(value)
			if !ok || !number.IsInt() || !number.Num().IsInt64() {
				return nil, nil, fmt.Errorf("dispatch_subagent: task %d max_turns must be a representable integer", i+1)
			}
			int64Value := number.Num().Int64()
			maxTurns = int(int64Value)
			if int64(maxTurns) != int64Value {
				return nil, nil, fmt.Errorf("dispatch_subagent: task %d max_turns is out of range", i+1)
			}
		}
		tasks = append(tasks, task)
		turns = append(turns, maxTurns)
	}
	return tasks, turns, nil
}

// sequentialUnknownOutcome preserves partial batch details when cancellation
// leaves the surrounding tool outcome unknown to the caller.
type sequentialUnknownOutcome struct{ cause error }

func (e sequentialUnknownOutcome) Error() string      { return e.cause.Error() }
func (e sequentialUnknownOutcome) Unwrap() error      { return e.cause }
func (sequentialUnknownOutcome) OutcomeUnknown() bool { return true }

func (t *SubagentTool) executeSequential(ctx context.Context, call ToolCall) (*ToolResult, error) {
	taskMaps, turns, err := sequentialTasks(call)
	if err != nil {
		return nil, err
	}
	slog.Debug("tools.subagent.execute_sequential", "task_count", len(taskMaps))
	states := make([]map[string]any, len(taskMaps))
	for i := range states {
		states[i] = map[string]any{"state": "not_started"}
	}
	ids := make([]string, 0, len(taskMaps))
	var failure error
	for i, item := range taskMaps {
		if err := ctx.Err(); err != nil {
			failure = err
			break
		}
		id := getString(item, "id")
		if id == "" {
			id = nextRequestID(fmt.Sprintf("task-%d", i+1))
		}
		mode := getString(item, "context_mode")
		if mode == "" {
			mode = getString(call.Args, "context_mode")
		}
		task := SubagentTask{
			ID:           id,
			Prompt:       getString(item, "prompt"),
			SystemPrompt: getString(item, "system_prompt"),
			Role:         getString(item, "role"),
			Tools:        toStringSlice(item["tools"]),
			Model:        getString(item, "model"),
			MaxTurns:     turns[i],
			ContextMode:  mode,
		}
		result, dispatchErr := t.dispatcher.Dispatch(ctx, task)
		if result.Error != nil {
			if dispatchErr == nil {
				dispatchErr = result.Error
			} else if !errors.Is(dispatchErr, result.Error) {
				dispatchErr = errors.Join(dispatchErr, result.Error)
			}
		}
		state := map[string]any{"state": "completed", "task_id": task.ID, "content": result.Content}
		if result.PhysicalSessionID != "" {
			state["physical_session_id"] = result.PhysicalSessionID
			ids = append(ids, result.PhysicalSessionID)
		}
		if dispatchErr != nil {
			state["state"] = "failed"
			state["error"] = dispatchErr.Error()
			failure = dispatchErr
		}
		states[i] = state
		if failure != nil {
			break
		}
	}
	if failure == nil {
		failure = ctx.Err()
	}
	var output strings.Builder
	for i, state := range states {
		fmt.Fprintf(&output, "Task %d (%s)", i+1, state["state"]) //nolint:errcheck
		if content, ok := state["content"].(string); ok && content != "" {
			fmt.Fprintf(&output, ": %s", content) //nolint:errcheck
		}
		if id, ok := state["physical_session_id"].(string); ok {
			output.WriteString(subagentSessionLine(id))
		}
		if message, ok := state["error"].(string); ok {
			fmt.Fprintf(&output, "\nError: %s", message) //nolint:errcheck
		}
		output.WriteByte('\n')
	}
	result := &ToolResult{Output: strings.TrimRight(output.String(), "\n"), Metadata: map[string]any{"execution_mode": "sequential", "tasks": states, "physical_session_ids": ids}}
	if failure != nil {
		// Put recovery details first because tool failure receipts can be bounded.
		var recovery strings.Builder
		recovery.WriteString("Sequential recovery:")
		for i, state := range states {
			fmt.Fprintf(&recovery, " %d:%s", i+1, state["state"]) //nolint:errcheck
			if id, ok := state["physical_session_id"].(string); ok {
				fmt.Fprintf(&recovery, "(physical_session_id=%q)", id) //nolint:errcheck
			}
		}
		wrapped := fmt.Errorf("%s\ndispatch_subagent: sequential dispatch: %w\nPartial results:\n%s", recovery.String(), failure, result.Output)
		var outcome interface{ OutcomeUnknown() bool }
		if (errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded)) && !errors.As(failure, &outcome) {
			return result, sequentialUnknownOutcome{cause: wrapped}
		}
		return result, wrapped
	}
	return result, nil
}

// executeParallel builds tasks from the tasks array (or a single prompt in
// parallel mode), dispatches them concurrently, and returns a structured
// summary of the results.
func (t *SubagentTool) executeParallel(ctx context.Context, call ToolCall, tasksRaw any) (*ToolResult, error) {
	if raw, ok := toAnySlice(tasksRaw); ok {
		for _, value := range raw {
			if item, ok := value.(map[string]any); ok {
				if err := validateSubagentContextMode(item); err != nil {
					return nil, err
				}
			}
		}
	}
	tasks := buildParallelTasks(call, tasksRaw)
	for i := range tasks {
		if tasks[i].ContextMode == "" {
			tasks[i].ContextMode = getString(call.Args, "context_mode")
		}
	}
	if len(tasks) == 0 {
		return nil, errors.New("dispatch_subagent: parallel mode requires at least one task")
	}

	slog.Debug("tools.subagent.execute_parallel",
		"task_count", len(tasks),
	)

	for _, task := range tasks {
		if task.ContextMode != "" && task.ContextMode != "independent" && task.ContextMode != "inherit" {
			return nil, fmt.Errorf("dispatch_subagent: invalid context_mode %q", task.ContextMode)
		}
	}
	results, err := t.dispatcher.ParallelDispatch(ctx, tasks)
	if err != nil {
		sessions := parallelSubagentSessionLines(results)
		taskIDs := make(map[string]int, len(tasks))
		resultIDs := make(map[string]int, len(results))
		for _, task := range tasks {
			taskIDs[task.ID]++
		}
		for _, result := range results {
			resultIDs[result.TaskID]++
		}
		var completed strings.Builder
		for i, result := range results {
			if i >= len(tasks) || result.TaskID == "" || result.TaskID != tasks[i].ID || taskIDs[result.TaskID] != 1 || resultIDs[result.TaskID] != 1 || result.Error != nil || strings.TrimSpace(result.Content) == "" {
				continue
			}
			fmt.Fprintf(&completed, "\nCompleted task %d: %s", i+1, result.Content) //nolint:errcheck
		}
		return &ToolResult{Output: sessions, Metadata: map[string]any{"physical_session_ids": subagentResultSessionIDs(results)}}, fmt.Errorf("dispatch_subagent: parallel dispatch: %w%s%s", err, sessions, completed.String())
	}

	// Format results as structured text.
	var sb strings.Builder
	var firstErr error
	totalDuration := time.Duration(0)
	for i, res := range results {
		role := tasks[i].Role
		if role == "" {
			role = "default"
		}
		fmt.Fprintf(&sb, "Task %d (%s): ", i+1, role) //nolint:errcheck
		if res.Error != nil {
			sb.WriteString(res.Error.Error())
			if firstErr == nil {
				firstErr = res.Error
			}
		} else {
			sb.WriteString(res.Content)
		}
		sb.WriteString(subagentSessionLine(res.PhysicalSessionID))
		sb.WriteString("\n")
		totalDuration += res.Duration
	}

	metadata := map[string]any{
		"task_count":           len(results),
		"duration":             totalDuration,
		"parallel":             true,
		"physical_session_ids": subagentResultSessionIDs(results),
	}
	if firstErr != nil {
		metadata["first_error"] = firstErr.Error()
	}

	output := strings.TrimRight(sb.String(), "\n")
	return &ToolResult{Output: output, Metadata: metadata}, nil
}

// buildParallelTasks constructs a slice of SubagentTask from the tasks array
// in the tool call. When parallel is true but no tasks array is provided, it
// builds a single task from the top-level prompt.
func buildParallelTasks(call ToolCall, tasksRaw any) []SubagentTask {
	// If a tasks array is provided, build tasks from it.
	if taskMaps, ok := toAnySlice(tasksRaw); ok && len(taskMaps) > 0 {
		tasks := make([]SubagentTask, 0, len(taskMaps))
		for i, tm := range taskMaps {
			m, ok := tm.(map[string]any)
			if !ok || m == nil {
				continue
			}
			id, ok := m["id"].(string)
			if !ok || id == "" {
				id = nextRequestID(fmt.Sprintf("task-%d", i+1))
			}
			tasks = append(tasks, SubagentTask{
				ID:           id,
				Prompt:       getString(m, "prompt"),
				SystemPrompt: getString(m, "system_prompt"),
				Role:         getString(m, "role"),
				Tools:        toStringSlice(m["tools"]),
				Model:        getString(m, "model"),
				MaxTurns:     toInt(m["max_turns"]),
				ContextMode:  getString(m, "context_mode"),
			})
		}
		return tasks
	}

	// Parallel mode without tasks array: build a single task from top-level
	// args so the caller can use parallel=true with a single prompt.
	prompt, ok := call.Args["prompt"].(string)
	if !ok || prompt == "" {
		return nil
	}
	id, ok := call.Args["id"].(string)
	if !ok || id == "" {
		id = nextRequestID("task")
	}
	return []SubagentTask{{
		ID:           id,
		Prompt:       prompt,
		SystemPrompt: getString(call.Args, "system_prompt"),
		Role:         getString(call.Args, "role"),
		Tools:        toStringSlice(call.Args["tools"]),
		Model:        getString(call.Args, "model"),
		MaxTurns:     toInt(call.Args["max_turns"]),
		ContextMode:  getString(call.Args, "context_mode"),
	}}
}

// getString safely extracts a string value from a map[string]any.
func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, ok := m[key].(string)
	if !ok {
		return ""
	}
	return s
}

// toAnySlice coerces a value into a []any, accepting []any and []map[string]any.
func toAnySlice(v any) ([]any, bool) {
	switch s := v.(type) {
	case []any:
		return s, true
	case []map[string]any:
		out := make([]any, len(s))
		for i, m := range s {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// nextRequestID generates a unique-ish identifier for requests that do not
// supply their own id. It combines a high-resolution timestamp with a process
// counter so concurrent callers do not collide.
func nextRequestID(prefix string) string {
	requestIDCounter.Add(1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), requestIDCounter.Load())
}

// toStringSlice coerces a tool-call argument into a []string. It accepts
// []string and []any (the shape JSON-decoded arrays take).
func toStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return append([]string(nil), s...)
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// toInt coerces a numeric tool-call argument into an int, accepting the common
// JSON-decoded numeric types.
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func subagentContextModeSchema() map[string]any {
	return map[string]any{"type": "string", "enum": []string{"independent", "inherit"}, "description": "Independent task context (default), or inherit parent conversation as reference data without its permissions."}
}

func validateSubagentContextMode(args map[string]any) error {
	value, exists := args["context_mode"]
	if !exists {
		return nil
	}
	mode, ok := value.(string)
	if !ok || (mode != "independent" && mode != "inherit") {
		return errors.New("dispatch_subagent: context_mode must be independent or inherit")
	}
	return nil
}

func subagentResultSessionIDs(results []SubagentResult) []string {
	ids := make([]string, len(results))
	for i, result := range results {
		ids[i] = result.PhysicalSessionID
	}
	return ids
}

func subagentSessionLine(id string) string {
	if id == "" {
		return ""
	}
	return "\nChild physical session ID (physical_session_id): " + id
}
func subagentContentWithSession(result SubagentResult) string {
	return result.Content + subagentSessionLine(result.PhysicalSessionID)
}
func parallelSubagentSessionLines(results []SubagentResult) string {
	var output strings.Builder
	for i, result := range results {
		if result.PhysicalSessionID != "" {
			fmt.Fprintf(&output, "\nTask %d child session ID: %s", i+1, result.PhysicalSessionID)
		}
	}
	return output.String()
}
