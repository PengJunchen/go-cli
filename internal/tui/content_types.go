package tui

// Content type constants are the canonical identifiers used to route agent
// events to renderers. They are declared as constants so that renderers can
// compare against them by name.
const (
	ContentTypeMarkdown       = "markdown"
	ContentTypeCode           = "code"
	ContentTypeTable          = "table"
	ContentTypeDiff           = "diff"
	ContentTypeError          = "error"
	ContentTypeToolCall       = "tool_call"
	ContentTypeToolResult     = "tool_result"
	ContentTypeThinking       = "thinking"
	ContentTypeProgress       = "progress"
	ContentTypeFileTree       = "file_tree"
	ContentTypeImage          = "image"
	ContentTypeLink           = "link"
	ContentTypeSystem         = "system"
	ContentTypeUser           = "user"
	ContentTypeAssistant      = "assistant"
	ContentTypeApproval       = "approval"
	ContentTypePrompt         = "prompt"
	ContentTypeCompaction     = "compaction"
	ContentTypeStreaming      = "streaming"
	ContentTypeStreamingCode  = "streaming_code"
	ContentTypeStreamingThink = "streaming_thinking"
	ContentTypeBlank          = "blank"
	ContentTypeSeparator      = "separator"
	ContentTypeToolOutput     = "tool_output"
	ContentTypeStatus         = "status"
	ContentTypeBox            = "box"
	ContentTypeSpinner        = "spinner"
	ContentTypeTodo           = "todo"
)
