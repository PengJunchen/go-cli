package tui

import "github.com/pengjunchen/go-cli/internal/core"

const assistantGenerationProgressLabel = "[Assistant response is being generated and checked]"

func (m *teaModel) updateAssistantGenerationProgress(event AgentEvent) {
	if event.RunID == "" || event.CanonicalTranscriptIndex == nil {
		return
	}
	occurrence := AssistantOccurrenceKey{
		RunID:                    event.RunID,
		CanonicalTranscriptIndex: *event.CanonicalTranscriptIndex,
	}

	changed := false
	var view string
	notify := m.shouldNotify()
	m.mu.Lock()
	switch event.Type {
	case core.EventKindAssistantGenerationStarted:
		if m.assistantProgress == nil || *m.assistantProgress != occurrence {
			active := occurrence
			m.assistantProgress = &active
			changed = true
		}
	case core.EventKindAssistantGenerationFinished:
		if m.assistantProgress != nil && *m.assistantProgress == occurrence {
			m.assistantProgress = nil
			changed = true
		}
	}
	if changed && notify {
		view = m.renderViewLocked()
	}
	m.mu.Unlock()
	m.notify(view)
}
