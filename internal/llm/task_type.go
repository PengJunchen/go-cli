package llm

import "context"

// TaskType classifies the purpose of an LLM call so the model selector can
// route lightweight tasks (summaries, title generation, extraction) to a
// cheaper model while keeping full chat turns on the primary model.
type TaskType string

const (
	// TaskTypeChat is the default task type for interactive conversation turns.
	TaskTypeChat TaskType = "chat"
	// TaskTypeSummary is used for compaction summarization.
	TaskTypeSummary TaskType = "summary"
	// TaskTypeTitle is used for conversation title generation.
	TaskTypeTitle TaskType = "title"
	// TaskTypeExtraction is used for memory/fact extraction.
	TaskTypeExtraction TaskType = "extraction"
)

// RequestPurpose classifies why one provider request was made. It is carried
// separately from TaskType so sub-agent calls can remain ordinary chat for
// model selection while retaining their purpose in traces.
type RequestPurpose string

const (
	// RequestPurposeMain identifies the primary interactive agent request.
	RequestPurposeMain RequestPurpose = "main"
	// RequestPurposeSubagent identifies a request made by a sub-agent.
	RequestPurposeSubagent RequestPurpose = "subagent"
	// RequestPurposeMemory identifies workspace-memory extraction requests.
	RequestPurposeMemory RequestPurpose = "memory"
	// RequestPurposeCompaction identifies conversation compaction requests.
	RequestPurposeCompaction RequestPurpose = "compaction"
	// RequestPurposeTitle identifies conversation title generation requests.
	RequestPurposeTitle RequestPurpose = "title"
)

// taskTypeKey is the context key used to carry a TaskType.
type taskTypeKey struct{}
type requestPurposeKey struct{}

// WithTaskType returns a copy of ctx that carries the given taskType.
// Downstream model selectors and cyclers read it to route the call to the
// appropriate model.
func WithTaskType(ctx context.Context, taskType TaskType) context.Context {
	return context.WithValue(ctx, taskTypeKey{}, taskType)
}

// TaskTypeFromContext extracts the TaskType from ctx. It returns TaskTypeChat
// when no task type is present, so callers that do not set a task type
// transparently use the primary model.
func TaskTypeFromContext(ctx context.Context) TaskType {
	if v, ok := ctx.Value(taskTypeKey{}).(TaskType); ok && v != "" {
		return v
	}
	return TaskTypeChat
}

// WithRequestPurpose returns a copy of ctx carrying an explicit request
// purpose. TaskType remains the selector-routing signal.
func WithRequestPurpose(ctx context.Context, purpose RequestPurpose) context.Context {
	return context.WithValue(ctx, requestPurposeKey{}, purpose)
}

// RequestPurposeFromContext returns an explicit purpose when present, or a
// stable purpose derived from the existing task type.
func RequestPurposeFromContext(ctx context.Context) RequestPurpose {
	taskType := TaskTypeFromContext(ctx)
	if purpose, ok := ctx.Value(requestPurposeKey{}).(RequestPurpose); ok && purpose != "" {
		// Subagent is a chat-only purpose override. Nested compaction, memory,
		// and title work keeps its task-specific routing and trace attribution.
		if purpose != RequestPurposeSubagent || taskType == TaskTypeChat {
			return purpose
		}
	}
	switch taskType {
	case TaskTypeSummary:
		return RequestPurposeCompaction
	case TaskTypeExtraction:
		return RequestPurposeMemory
	case TaskTypeTitle:
		return RequestPurposeTitle
	default:
		return RequestPurposeMain
	}
}
