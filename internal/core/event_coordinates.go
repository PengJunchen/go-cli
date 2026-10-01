package core

import "context"

type agentEventCoordinatesKey struct{}

type agentEventCoordinates struct {
	runID                      string
	canonicalTranscriptIndex   *uint64
	assistantTranscriptOrdinal *uint64
}

func withAgentEventCoordinates(ctx context.Context, coordinates agentEventCoordinates) context.Context {
	if ctx == nil {
		return nil
	}
	return context.WithValue(ctx, agentEventCoordinatesKey{}, cloneAgentEventCoordinates(coordinates))
}

func agentEventCoordinatesFromContext(ctx context.Context) agentEventCoordinates {
	if ctx == nil {
		return agentEventCoordinates{}
	}
	coordinates, _ := ctx.Value(agentEventCoordinatesKey{}).(agentEventCoordinates)
	if coordinates.runID == "" {
		coordinates.runID = RunIDFromContext(ctx)
	}
	return cloneAgentEventCoordinates(coordinates)
}

func cloneAgentEventCoordinates(coordinates agentEventCoordinates) agentEventCoordinates {
	if coordinates.canonicalTranscriptIndex != nil {
		index := *coordinates.canonicalTranscriptIndex
		coordinates.canonicalTranscriptIndex = &index
	}
	if coordinates.assistantTranscriptOrdinal != nil {
		ordinal := *coordinates.assistantTranscriptOrdinal
		coordinates.assistantTranscriptOrdinal = &ordinal
	}
	return coordinates
}

func stampAgentEvent(coordinates agentEventCoordinates, event AgentEvent) AgentEvent {
	if coordinates.runID != "" {
		event.RunID = coordinates.runID
	}
	switch event.Kind {
	case "message":
		event.CanonicalTranscriptIndex = cloneOptionalUint64(coordinates.canonicalTranscriptIndex)
		event.AssistantTranscriptOrdinal = cloneOptionalUint64(coordinates.assistantTranscriptOrdinal)
	case EventKindPreToolCall,
		EventKindAssistantGenerationStarted,
		EventKindAssistantGenerationFinished,
		"tool_call", "tool_canceled", "tool_output", "tool_result":
		event.CanonicalTranscriptIndex = cloneOptionalUint64(coordinates.canonicalTranscriptIndex)
		event.AssistantTranscriptOrdinal = cloneOptionalUint64(coordinates.assistantTranscriptOrdinal)
	}
	return event
}

func cloneOptionalUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
