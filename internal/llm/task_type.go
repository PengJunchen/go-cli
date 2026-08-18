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

// taskTypeKey is the context key used to carry a TaskType.
type taskTypeKey struct{}

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
