package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type extraToolSchemaExpectation struct {
	propertyTypes map[string]string
	required      []string
	enums         map[string][]string
	items         map[string]string
}

func TestFixedExtraToolsExposeAccurateParameterSchemas(t *testing.T) {
	goalStore, err := NewDefaultGoalStore("")
	require.NoError(t, err)
	taskStore := NewTaskStore()
	planController := &extraSchemaPlanController{}
	searchTargets := NewDefaultToolRegistry()
	searchTool := NewToolSearchTool(searchTargets)

	definitions := []ToolDefinition{
		NewTodoWriteTool(NewTodoStore()),
		NewTaskCreateTool(taskStore),
		NewTaskUpdateTool(taskStore),
		NewGoalCreateTool(goalStore),
		NewGoalUpdateTool(goalStore),
		NewGitPRCreateTool(""),
		NewTaskGetTool(taskStore),
		NewTaskListTool(taskStore),
		NewGoalListTool(goalStore, taskStore),
		NewGoalGetTool(goalStore, taskStore),
		NewAskUserQuestionTool(&extraSchemaQuestionEmitter{}, 0),
		NewEnterPlanModeTool(planController),
		NewExitPlanModeTool(planController),
		searchTool,
		NewWebFetchTool(),
		NewWebSearchTool(),
	}

	wants := map[string]extraToolSchemaExpectation{
		"todo_write": {
			propertyTypes: map[string]string{"action": "string", "id": "string", "content": "string", "status": "string", "priority": "string"},
			required:      []string{"action"},
			enums:         map[string][]string{"action": {"add", "update", "list", "remove"}},
		},
		"task_create": {
			propertyTypes: map[string]string{"title": "string", "description": "string"},
			required:      []string{"title"},
		},
		"task_update": {
			propertyTypes: map[string]string{"id": "string", "status": "string", "title": "string", "description": "string"},
			required:      []string{"id"},
		},
		"task_get": {
			propertyTypes: map[string]string{"id": "string"},
			required:      []string{"id"},
		},
		"task_list": {propertyTypes: map[string]string{}},
		"goal_create": {
			propertyTypes: map[string]string{"title": "string", "description": "string", "success_criteria": "string"},
			required:      []string{"title"},
		},
		"goal_update": {
			propertyTypes: map[string]string{"id": "string", "status": "string", "title": "string", "description": "string", "priority": "string", "success_criteria": "string"},
			required:      []string{"id"},
			enums:         map[string][]string{"status": {"draft", "active", "in_progress", "completed", "blocked", "abandoned"}},
		},
		"goal_get": {
			propertyTypes: map[string]string{"id": "string"},
			required:      []string{"id"},
		},
		"goal_list": {propertyTypes: map[string]string{}},
		"enter_plan_mode": {
			propertyTypes: map[string]string{"reason": "string"},
		},
		"exit_plan_mode": {
			propertyTypes: map[string]string{"summary": "string"},
		},
		"ask_user": {
			propertyTypes: map[string]string{"question": "string", "options": "array"},
			required:      []string{"question"},
			items:         map[string]string{"options": "string"},
		},
		"tool_search": {
			propertyTypes: map[string]string{"query": "string", "category": "string"},
			enums:         map[string][]string{"category": {"", "file", "network", "shell", "other"}},
		},
		"web_fetch": {
			propertyTypes: map[string]string{"url": "string"},
			required:      []string{"url"},
		},
		"web_search": {
			propertyTypes: map[string]string{"query": "string"},
			required:      []string{"query"},
		},
		"git_pr_create": {
			propertyTypes: map[string]string{"title": "string", "body": "string", "base": "string", "head": "string"},
			required:      []string{"title", "base", "head"},
		},
	}

	seen := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		name := definition.Name()
		want, ok := wants[name]
		require.Truef(t, ok, "unexpected extra tool %q", name)
		seen[name] = true

		parameterized, ok := definition.(Parameterized)
		require.Truef(t, ok, "%s must implement Parameterized", name)
		schema := requireSchemaObject(t, parameterized.Parameters())
		assert.Equal(t, "object", schema["type"], "%s root schema", name)
		assert.Equal(t, false, schema["additionalProperties"], "%s must close its root object", name)
		properties := requireSchemaObject(t, schema["properties"])
		assert.Len(t, properties, len(want.propertyTypes), "%s properties", name)
		assert.ElementsMatch(t, want.required, schemaStringArray(t, schema["required"]), "%s required", name)

		for propertyName, expectedType := range want.propertyTypes {
			property := requireSchemaObject(t, properties[propertyName])
			assert.Equalf(t, expectedType, property["type"], "%s.%s type", name, propertyName)
			if expectedEnum, hasEnum := want.enums[propertyName]; hasEnum {
				assert.Equalf(t, expectedEnum, schemaStringArray(t, property["enum"]), "%s.%s enum", name, propertyName)
			}
			if expectedItemType, hasItems := want.items[propertyName]; hasItems {
				items := requireSchemaObject(t, property["items"])
				assert.Equalf(t, expectedItemType, items["type"], "%s.%s item type", name, propertyName)
			}
		}

		// The concrete schema methods use only the validator's currently supported
		// object/property subset. git_pr_create retains its existing annotations.
		if name != "git_pr_create" {
			assert.Equal(t, map[string]any{"type": "object", "properties": properties, "required": schema["required"], "additionalProperties": false}, schema, "%s schema shape", name)
		}
	}
	for name := range wants {
		assert.Truef(t, seen[name], "missing fixed extra tool %q", name)
	}
	assert.Len(t, definitions, 16)

	// Each call returns a fresh schema so a model adapter cannot mutate a later
	// definition or the prepared registry's captured snapshot.
	for _, definition := range definitions {
		parameterized := definition.(Parameterized)
		first := requireSchemaObject(t, parameterized.Parameters())
		firstProperties := requireSchemaObject(t, first["properties"])
		firstProperties["caller_mutation"] = map[string]any{"type": "string"}
		second := requireSchemaObject(t, parameterized.Parameters())
		secondProperties := requireSchemaObject(t, second["properties"])
		assert.NotContainsf(t, secondProperties, "caller_mutation", "%s must return independent schemas", definition.Name())
	}
}

type extraSchemaPlanController struct {
	active    bool
	reasons   []string
	summaries []string
}

func (c *extraSchemaPlanController) Enter(_ context.Context, reason string) error {
	c.active = true
	c.reasons = append(c.reasons, reason)
	return nil
}

func (c *extraSchemaPlanController) Exit(_ context.Context, summary string) error {
	c.active = false
	c.summaries = append(c.summaries, summary)
	return nil
}

func (c *extraSchemaPlanController) IsActive() bool { return c.active }

type extraSchemaQuestionEmitter struct {
	questions []HITLQuestionEvent
}

func (e *extraSchemaQuestionEmitter) Emit(_ context.Context, event HITLQuestionEvent) error {
	e.questions = append(e.questions, HITLQuestionEvent{
		QuestionID: event.QuestionID,
		Question:   event.Question,
		Options:    append([]string(nil), event.Options...),
	})
	event.ResponseCh <- HITLAnswer{QuestionID: event.QuestionID, Answer: "answered"}
	return nil
}

type extraSchemaSearchProvider struct {
	queries []string
}

func (*extraSchemaSearchProvider) Name() string { return "schema-test" }

func (p *extraSchemaSearchProvider) Search(_ context.Context, query string, _ SearchOptions) ([]SearchResult, error) {
	p.queries = append(p.queries, query)
	return []SearchResult{{Title: "fixture", URL: "https://example.test", Snippet: "local result"}}, nil
}

type extraSchemaTargetTool struct{ name string }

func (t *extraSchemaTargetTool) Name() string        { return t.name }
func (t *extraSchemaTargetTool) Description() string { return "read a local file" }
func (t *extraSchemaTargetTool) Execute(_ context.Context, call ToolCall) (*ToolResult, error) {
	return &ToolResult{Output: call.Name}, nil
}

func TestFixedExtraPreparedSchemasReachExistingExecutors(t *testing.T) {
	ctx := context.Background()
	var fetchCalls atomic.Int32
	webServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetchCalls.Add(1)
		_, _ = w.Write([]byte("local web body"))
	}))
	t.Cleanup(webServer.Close)

	todoStore := NewTodoStore()
	taskStore := NewTaskStore()
	seedTask := taskStore.Create("seed task", "seed description")
	goalStore, err := NewDefaultGoalStore("")
	require.NoError(t, err)
	seedGoal, err := goalStore.Create(ctx, "seed goal", "seed description", "seed criteria")
	require.NoError(t, err)
	planController := &extraSchemaPlanController{}
	questionEmitter := &extraSchemaQuestionEmitter{}
	searchProvider := &extraSchemaSearchProvider{}
	searchTargets := NewDefaultToolRegistry()
	require.NoError(t, searchTargets.Register(ctx, &extraSchemaTargetTool{name: "read_file"}))

	definitions := []ToolDefinition{
		NewTodoWriteTool(todoStore),
		NewTaskCreateTool(taskStore),
		NewTaskUpdateTool(taskStore),
		NewGoalCreateTool(goalStore),
		NewGoalUpdateTool(goalStore),
		NewTaskGetTool(taskStore),
		NewTaskListTool(taskStore),
		NewGoalListTool(goalStore, taskStore),
		NewGoalGetTool(goalStore, taskStore),
		NewAskUserQuestionTool(questionEmitter, 0),
		NewEnterPlanModeTool(planController),
		NewExitPlanModeTool(planController),
		NewToolSearchTool(searchTargets),
		NewWebFetchTool(WithWebFetchClient(&http.Client{})),
		NewWebSearchTool(WithSearchProvider(searchProvider)),
	}
	validArgs := map[string]map[string]any{
		"todo_write":      {"action": "add", "content": "ship schemas"},
		"task_create":     {"title": "schema task", "description": "created through prepared dispatch"},
		"task_update":     {"id": seedTask.ID, "status": "reviewing", "title": "updated seed", "description": "updated"},
		"goal_create":     {"title": "schema goal", "description": "created through prepared dispatch", "success_criteria": "visible"},
		"goal_update":     {"id": seedGoal.ID, "status": "active", "title": "active seed", "priority": "urgent"},
		"task_get":        {"id": seedTask.ID},
		"task_list":       {},
		"goal_list":       {},
		"goal_get":        {"id": seedGoal.ID},
		"ask_user":        {"question": "Choose", "options": []string{"yes", "no"}},
		"enter_plan_mode": {"reason": "inspect the change"},
		"exit_plan_mode":  {"summary": "inspection complete"},
		"tool_search":     {"query": "read_file", "category": ""},
		"web_fetch":       {"url": webServer.URL},
		"web_search":      {"query": "schema parity"},
	}

	base := NewDefaultToolRegistry()
	definitionByName := make(map[string]ToolDefinition, len(definitions))
	for _, definition := range definitions {
		definitionByName[definition.Name()] = definition
		require.NoError(t, base.Register(ctx, definition))
	}
	registry := NewMiddlewareToolRegistry(base)
	var policyCalls atomic.Int32
	configurePreparedRegistry(t, registry, func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		policyCalls.Add(1)
		return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}, PreparedToolStages{})

	proxyByName := make(map[string]ToolDefinition, len(definitions))
	for name := range validArgs {
		proxy, getErr := registry.Get(ctx, name)
		require.NoError(t, getErr)
		proxyByName[name] = proxy
	}

	for _, definition := range definitions {
		name := definition.Name()
		args := decodeExtraToolArgs(t, validArgs[name])
		call := ToolCall{ID: "direct-" + name, Name: name, Args: args}

		// The exact same JSON-decoded fixture is first accepted by the existing
		// concrete executor and then by its protected prepared proxy.
		directResult, directErr := definition.Execute(ctx, call)
		require.NoErrorf(t, directErr, "%s direct executor", name)
		require.NotNilf(t, directResult, "%s direct result", name)
		preparedResult, preparedErr := proxyByName[name].Execute(ctx, call)
		require.NoErrorf(t, preparedErr, "%s prepared executor", name)
		require.NotNilf(t, preparedResult, "%s prepared result", name)
		assert.NotEmptyf(t, preparedResult.Output, "%s result proves executor was reached", name)
		if name == "web_fetch" {
			assert.Contains(t, preparedResult.Output, "local web body")
		}
		if name == "tool_search" {
			assert.Contains(t, preparedResult.Output, "read_file")
		}
	}
	assert.Equal(t, int32(len(definitions)), policyCalls.Load(), "only prepared valid calls ask policy")
	assert.Equal(t, []string{"schema parity", "schema parity"}, searchProvider.queries)
	assert.Equal(t, 2, len(questionEmitter.questions))
	assert.Equal(t, []string{"yes", "no"}, questionEmitter.questions[1].Options)
	assert.Equal(t, []string{"inspect the change", "inspect the change"}, planController.reasons)
	assert.Equal(t, []string{"inspection complete", "inspection complete"}, planController.summaries)
	assert.False(t, planController.active)
	assert.Len(t, todoStore.List(), 2, "direct and prepared todo additions reach the store")
	for _, item := range todoStore.List() {
		assert.Equal(t, "pending", item.Status)
		assert.Equal(t, "medium", item.Priority)
	}
	updatedTask, found := taskStore.Get(seedTask.ID)
	require.True(t, found)
	assert.Equal(t, TaskStatus("reviewing"), updatedTask.Status, "task_update reaches the store")
	assert.Equal(t, "updated seed", updatedTask.Title)
	assert.Equal(t, "updated", updatedTask.Description)
	assert.Len(t, taskStore.List(), 3, "task_create reaches the store")
	for _, task := range taskStore.List() {
		if task.Title == "schema task" {
			assert.Equal(t, "created through prepared dispatch", task.Description)
			assert.Equal(t, StatusPending, task.Status)
		}
	}
	updatedGoal, err := goalStore.Get(ctx, seedGoal.ID)
	require.NoError(t, err)
	assert.Equal(t, GoalStatusActive, updatedGoal.Status)
	assert.Equal(t, "urgent", updatedGoal.Priority)
	goals, err := goalStore.List(ctx)
	require.NoError(t, err)
	createdGoals := 0
	for _, goal := range goals {
		if goal.Title == "schema goal" {
			createdGoals++
			assert.Equal(t, "created through prepared dispatch", goal.Description)
			assert.Equal(t, "visible", goal.SuccessCriteria)
			assert.Equal(t, GoalStatusDraft, goal.Status)
			assert.Equal(t, "medium", goal.Priority)
		}
	}
	assert.Equal(t, 2, createdGoals, "both direct and prepared goal_create fixtures reach the store")
	assert.Equal(t, int32(2), fetchCalls.Load(), "web_fetch reaches its executor twice")

	sideEffectsBeforeInvalid := map[string]int{
		"todos":       len(todoStore.List()),
		"tasks":       len(taskStore.List()),
		"goals":       len(goals),
		"searches":    len(searchProvider.queries),
		"fetches":     int(fetchCalls.Load()),
		"questions":   len(questionEmitter.questions),
		"plan_enters": len(planController.reasons),
		"plan_exits":  len(planController.summaries),
	}
	for _, definition := range definitions {
		name := definition.Name()
		proxy := proxyByName[name]
		parameterized, ok := proxy.(Parameterized)
		require.Truef(t, ok, "%s prepared proxy must preserve Parameterized", name)
		schema := requireSchemaObject(t, parameterized.Parameters())
		properties := requireSchemaObject(t, schema["properties"])

		invalidCases := []map[string]any{}
		unknownField := decodeExtraToolArgs(t, validArgs[name])
		unknownField["unexpected"] = true
		invalidCases = append(invalidCases, unknownField)
		for propertyName, propertyValue := range properties {
			propertySchema := requireSchemaObject(t, propertyValue)
			if propertySchema["type"] == "array" {
				continue
			}
			wrongType := decodeExtraToolArgs(t, validArgs[name])
			wrongType[propertyName] = int64(7)
			invalidCases = append(invalidCases, wrongType)
		}
		for _, invalidArgs := range invalidCases {
			before := policyCalls.Load()
			_, callErr := proxy.Execute(ctx, ToolCall{Name: name, Args: invalidArgs})
			require.Error(t, callErr, "%s must reject malformed/unknown arguments", name)
			assert.Equal(t, before, policyCalls.Load(), "%s schema errors precede policy", name)
		}
		for _, required := range schemaStringArray(t, schema["required"]) {
			missing := decodeExtraToolArgs(t, validArgs[name])
			delete(missing, required)
			before := policyCalls.Load()
			_, callErr := proxy.Execute(ctx, ToolCall{Name: name, Args: missing})
			require.Error(t, callErr, "%s must require %s", name, required)
			assert.Equal(t, before, policyCalls.Load(), "%s missing required field precedes policy", name)
		}
	}
	for _, tt := range []struct {
		name string
		args map[string]any
	}{
		{"todo_write", map[string]any{"action": "ADD"}},
		{"goal_update", map[string]any{"id": seedGoal.ID, "status": "ACTIVE"}},
		{"tool_search", map[string]any{"category": "unknown"}},
	} {
		before := policyCalls.Load()
		_, callErr := proxyByName[tt.name].Execute(ctx, ToolCall{Name: tt.name, Args: tt.args})
		require.Error(t, callErr, "%s enum rejects non-canonical value", tt.name)
		assert.Equal(t, before, policyCalls.Load(), "%s enum errors precede policy", tt.name)
	}
	goals, err = goalStore.List(ctx)
	require.NoError(t, err)
	sideEffectsAfterInvalid := map[string]int{
		"todos":       len(todoStore.List()),
		"tasks":       len(taskStore.List()),
		"goals":       len(goals),
		"searches":    len(searchProvider.queries),
		"fetches":     int(fetchCalls.Load()),
		"questions":   len(questionEmitter.questions),
		"plan_enters": len(planController.reasons),
		"plan_exits":  len(planController.summaries),
	}
	assert.Equal(t, sideEffectsBeforeInvalid, sideEffectsAfterInvalid, "invalid prepared calls do not reach concrete executors")

	// Blank strings satisfy JSON Schema presence/type but remain rejected by
	// each concrete executor after the prepared policy, as before this change.
	for _, tt := range []struct {
		name string
		args map[string]any
	}{
		{"web_search", map[string]any{"query": " "}},
		{"web_fetch", map[string]any{"url": " "}},
		{"task_create", map[string]any{"title": " "}},
		{"task_update", map[string]any{"id": " "}},
		{"task_get", map[string]any{"id": " "}},
		{"goal_create", map[string]any{"title": " "}},
		{"goal_update", map[string]any{"id": " "}},
		{"goal_get", map[string]any{"id": " "}},
		{"ask_user", map[string]any{"question": ""}},
	} {
		before := policyCalls.Load()
		_, callErr := proxyByName[tt.name].Execute(ctx, ToolCall{Name: tt.name, Args: tt.args})
		require.Error(t, callErr, "%s executor rejects blank required strings", tt.name)
		assert.Equal(t, before+1, policyCalls.Load(), "%s blank value passes schema but fails after policy", tt.name)
	}

	// todo_write has conditional action requirements that the current prepared
	// schema vocabulary cannot express. Keep the existing executor responsible.
	initialTodoCount := len(todoStore.List())
	for _, args := range []map[string]any{
		{"action": "add"},
		{"action": "update", "id": todoStore.List()[0].ID},
		{"action": "remove"},
	} {
		before := policyCalls.Load()
		_, callErr := proxyByName["todo_write"].Execute(ctx, ToolCall{Name: "todo_write", Args: args})
		require.Error(t, callErr)
		assert.Equal(t, before+1, policyCalls.Load(), "conditional executor error follows schema and policy")
	}
	assert.Len(t, todoStore.List(), initialTodoCount, "failed conditional actions have no todo side effect")

	// Preserve existing direct-executor aliases while restricting the model
	// schema to canonical enum spellings.
	_, err = definitionByName["goal_update"].Execute(ctx, ToolCall{Name: "goal_update", Args: map[string]any{"id": seedGoal.ID, "status": " ACTIVE "}})
	require.NoError(t, err, "raw goal executor continues to accept trimmed/case-insensitive status aliases")
	before := policyCalls.Load()
	_, err = proxyByName["goal_update"].Execute(ctx, ToolCall{Name: "goal_update", Args: map[string]any{"id": seedGoal.ID, "status": " ACTIVE "}})
	require.Error(t, err, "prepared enum publishes canonical goal status spelling")
	assert.Equal(t, before, policyCalls.Load())

	_, err = definitionByName["tool_search"].Execute(ctx, ToolCall{Name: "tool_search", Args: map[string]any{"query": "read_file", "category": " FILE "}})
	require.NoError(t, err, "raw tool search continues to normalize category aliases")
	before = policyCalls.Load()
	_, err = proxyByName["tool_search"].Execute(ctx, ToolCall{Name: "tool_search", Args: map[string]any{"query": "read_file", "category": " FILE "}})
	require.Error(t, err, "prepared category enum publishes the canonical spelling")
	assert.Equal(t, before, policyCalls.Load())

	_, err = definitionByName["ask_user"].Execute(ctx, ToolCall{Name: "ask_user", Args: map[string]any{"question": "raw options", "options": []any{"yes", 3}}})
	require.NoError(t, err, "raw executor continues to drop non-string option values")
	assert.Equal(t, []string{"yes"}, questionEmitter.questions[len(questionEmitter.questions)-1].Options)
	before = policyCalls.Load()
	_, err = proxyByName["ask_user"].Execute(ctx, ToolCall{Name: "ask_user", Args: map[string]any{"question": "invalid options", "options": []any{"yes", 3}}})
	require.Error(t, err, "prepared array<string> rejects non-string option values")
	assert.Equal(t, before, policyCalls.Load())

	// Unknown fields stay ignored by the raw executor, while the model-facing
	// prepared contract remains closed.
	_, err = definitionByName["task_create"].Execute(ctx, ToolCall{Name: "task_create", Args: map[string]any{"title": "raw unknown", "future_field": true}})
	require.NoError(t, err)
}

func decodeExtraToolArgs(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(args)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	return decoded
}
