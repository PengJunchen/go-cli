package tui

import (
	"context"
	"log/slog"
	"sync"
)

// Renderer renders agent content of a given content type into a styled ANSI
// string. It is the functional equivalent of a Bubbletea content renderer; the
// returned string can be painted directly to a terminal or accumulated into the
// App's view buffer.
type Renderer interface {
	// Render converts content into a styled string using the given options.
	Render(ctx context.Context, content string, opts RenderOpts) string
	// Name returns the stable renderer identifier.
	Name() string
	// Supports reports whether this renderer can render the given content type.
	Supports(contentType string) bool
}

// RenderOpts carries contextual options into a Renderer call.
type RenderOpts struct {
	// Theme is the active theme whose styles the renderer may apply.
	Theme Theme
	// Width is the target render width in terminal columns (0 = unconstrained).
	Width int
	// ContentType names the type of content being rendered.
	ContentType string
	// Language is an optional language hint (used by code renderers).
	Language string
	// Stream identifies the output source for tool_output events: "stdout"
	// or "stderr".
	Stream string
}

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

// contentTypes lists every content type the TUI layer supports. The order is
// the canonical registry order.
var contentTypes = []string{
	ContentTypeMarkdown, ContentTypeCode, ContentTypeTable, ContentTypeDiff,
	ContentTypeError, ContentTypeToolCall, ContentTypeToolResult, ContentTypeThinking, ContentTypeProgress,
	ContentTypeFileTree, ContentTypeImage, ContentTypeLink, ContentTypeSystem,
	ContentTypeUser, ContentTypeAssistant, ContentTypeApproval, ContentTypePrompt,
	ContentTypeCompaction, ContentTypeStreaming, ContentTypeStreamingCode,
	ContentTypeStreamingThink, ContentTypeBlank, ContentTypeSeparator,
	ContentTypeStatus, ContentTypeBox, ContentTypeSpinner, ContentTypeToolOutput,
}

// DefaultContentType is the fallback content type used when an event carries an
// unknown or empty content type.
const DefaultContentType = ContentTypeStatus

// streamMarker is implemented by renderers that accumulate partial output and
// replace the previous rendered frame (streaming renderers). The App uses it to
// decide whether to append a new line or overwrite the last one.
type streamMarker interface {
	streaming() bool
}

// Compile-time assertions that the streaming renderers satisfy streamMarker
// (every interface must have a default-implementation guard).
var (
	_ streamMarker = StreamingRenderer{}
	_ streamMarker = StreamingCodeRenderer{}
	_ streamMarker = StreamingThinkingRenderer{}
)

// RendererRegistry is a thread-safe map from content type to renderer. Renderers
// register a payload of content types; the registry indexes each.
type RendererRegistry struct {
	mu     sync.RWMutex
	byType map[string]Renderer
}

// NewRendererRegistry returns an empty registry ready for registration.
func NewRendererRegistry() *RendererRegistry {
	return &RendererRegistry{byType: make(map[string]Renderer)}
}

// NewDefaultRegistry returns a registry pre-populated with all built-in
// renderers.
func NewDefaultRegistry() *RendererRegistry {
	reg := NewRendererRegistry()
	RegisterDefaultRenderers(reg)
	return reg
}

// Register indexes every content type the renderer supports.
func (r *RendererRegistry) Register(renderer Renderer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range contentTypes {
		if renderer.Supports(t) {
			r.byType[t] = renderer
		}
	}
	slog.Debug("tui.renderer.register", "renderer", renderer.Name())
}

// Get returns the renderer registered for contentType.
func (r *RendererRegistry) Get(contentType string) (Renderer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	renderer, ok := r.byType[contentType]
	return renderer, ok
}

// List returns a snapshot of the currently registered renderers keyed by
// content type.
func (r *RendererRegistry) List() map[string]Renderer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Renderer, len(r.byType))
	for t, renderer := range r.byType {
		out[t] = renderer
	}
	return out
}

// RegisterDefaultRenderers registers all built-in renderers into the given
// registry.
func RegisterDefaultRenderers(reg *RendererRegistry) {
	reg.Register(NewMarkdownRenderer())
	reg.Register(CodeRenderer{})
	reg.Register(TableRenderer{})
	reg.Register(DiffRenderer{})
	reg.Register(ErrorRenderer{})
	reg.Register(ToolCallRenderer{})
	reg.Register(ToolResultRenderer{})
	reg.Register(ToolOutputRenderer{})
	reg.Register(ThinkingRenderer{})
	reg.Register(ProgressRenderer{})
	reg.Register(FileTreeRenderer{})
	reg.Register(ImageRenderer{})
	reg.Register(LinkRenderer{})
	reg.Register(SystemRenderer{})
	reg.Register(UserRenderer{})
	reg.Register(AssistantRenderer{})
	reg.Register(ApprovalRenderer{})
	reg.Register(PromptRenderer{})
	reg.Register(CompactionRenderer{})
	reg.Register(StreamingRenderer{})
	reg.Register(StreamingCodeRenderer{})
	reg.Register(StreamingThinkingRenderer{})
	reg.Register(BlankRenderer{})
	reg.Register(SeparatorRenderer{})
	reg.Register(StatusRenderer{})
	reg.Register(BoxRenderer{})
}
