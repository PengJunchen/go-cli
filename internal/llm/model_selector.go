package llm

// DefaultModelSelector is a ModelSelector that holds a primary model for full
// chat turns and an optional small model for lightweight tasks (summary, title,
// extraction). When the small model is nil, all task types route to the
// primary model.
type DefaultModelSelector struct {
	primary BaseChatModel
	small   BaseChatModel
}

// Compile-time assertion that DefaultModelSelector satisfies ModelSelector.
var _ ModelSelector = (*DefaultModelSelector)(nil)

// NewDefaultModelSelector creates a ModelSelector with the given primary and
// small models. When small is nil, every task type uses the primary model.
func NewDefaultModelSelector(primary, small BaseChatModel) *DefaultModelSelector {
	return &DefaultModelSelector{primary: primary, small: small}
}

// SelectModel returns the model appropriate for the given task type. Lightweight
// tasks (summary, title, extraction) use the small model when available; chat
// and any unrecognized type use the primary model.
func (s *DefaultModelSelector) SelectModel(taskType TaskType) BaseChatModel {
	if s.small != nil {
		switch taskType {
		case TaskTypeSummary, TaskTypeTitle, TaskTypeExtraction:
			return s.small
		}
	}
	return s.primary
}

// PrimaryModel returns the primary (chat) model.
func (s *DefaultModelSelector) PrimaryModel() BaseChatModel { return s.primary }

// SmallModel returns the small model, or nil when not configured.
func (s *DefaultModelSelector) SmallModel() BaseChatModel { return s.small }

// HasSmallModel reports whether a small model is configured.
func (s *DefaultModelSelector) HasSmallModel() bool { return s.small != nil }
