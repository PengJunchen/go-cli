//go:build !headless

package tui

import "github.com/pengjunchen/go-cli/internal/core"

func (event AgentEvent) clone() AgentEvent {
	if event.CanonicalTranscriptIndex != nil {
		index := *event.CanonicalTranscriptIndex
		event.CanonicalTranscriptIndex = &index
	}
	if event.AssistantTranscriptOrdinal != nil {
		ordinal := *event.AssistantTranscriptOrdinal
		event.AssistantTranscriptOrdinal = &ordinal
	}
	if event.SourceTruncated != nil {
		truncated := *event.SourceTruncated
		event.SourceTruncated = &truncated
	}
	if event.ToolCalls != nil {
		event.ToolCalls = append([]ToolCallData(nil), event.ToolCalls...)
	}
	if event.TokenUsage != nil {
		usage := *event.TokenUsage
		usage.CostSources = append([]core.TokenUsageCostSource(nil), event.TokenUsage.CostSources...)
		usage.PriceSources = append([]core.TokenUsagePriceSource(nil), event.TokenUsage.PriceSources...)
		usage.ProviderModels = append([]core.TokenUsageProviderModel(nil), event.TokenUsage.ProviderModels...)
		event.TokenUsage = &usage
	}
	return event
}
