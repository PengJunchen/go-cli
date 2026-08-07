package tui

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/pengjunchen/go-cli/internal/tui/markdown"
)

// renderTheme resolves the theme to apply, falling back to a light-safe dark
// preset when the options carry none.
func renderTheme(opts RenderOpts) Theme {
	if opts.Theme != nil {
		return opts.Theme
	}
	return DarkTheme{}
}

// logRender records a render event at debug level together with the rendered
// byte length. The TUI layer uses slog (not tracing spans) for this metric.
func logRender(ctx context.Context, renderer, contentType string, outLen int) {
	slog.DebugContext(ctx, "tui.renderer.render",
		"renderer", renderer,
		"content_type", contentType,
		"output_bytes", outLen,
	)
}

// wrapWidth wraps text to the given width (0 or negative means no wrapping).
func wrapWidth(text string, width int) string {
	if width <= 0 || len(text) <= width {
		return text
	}
	return stripANSI(text, width)
}

// stripANSI returns a best-effort wrapped copy of text limited to maxCols
// printable characters per line, ignoring ANSI escape sequences.
func stripANSI(text string, maxCols int) string {
	var sb strings.Builder
	col := 0
	for _, r := range text {
		if r == '\n' {
			sb.WriteRune('\n')
			col = 0
			continue
		}
		if col >= maxCols {
			sb.WriteRune('\n')
			col = 0
		}
		sb.WriteRune(r)
		col++
	}
	return sb.String()
}

// ---------- markdown ----------

// themeAdapter wraps a Theme to satisfy the markdown.ThemeAdapter interface.
// It bridges the tui.Theme accessors (which return Style) to the simpler
// string-returning methods that the markdown renderer expects.
type themeAdapter struct{ theme Theme }

func (a themeAdapter) Bold(text string) string          { return a.theme.Bold().Render(text) }
func (a themeAdapter) Italic(text string) string        { return a.theme.Italic().Render(text) }
func (a themeAdapter) Faint(text string) string         { return a.theme.Faint().Render(text) }
func (a themeAdapter) Primary(text string) string       { return a.theme.Primary().Render(text) }
func (a themeAdapter) Secondary(text string) string     { return a.theme.Secondary().Render(text) }
func (a themeAdapter) Error(text string) string         { return a.theme.Error().Render(text) }
func (a themeAdapter) Underline(text string) string     { return NewStyle().Underline(true).Render(text) }
func (a themeAdapter) Strikethrough(text string) string { return NewStyle().Strikethrough(true).Render(text) }

// MarkdownRenderer parses markdown content into an AST and renders it with
// ANSI styling via the markdown.MarkdownASTRenderer. An optional CodeHighlighter
// provides syntax highlighting for fenced code blocks.
type MarkdownRenderer struct {
	highlighter CodeHighlighter
}

var _ Renderer = (*MarkdownRenderer)(nil)

// NewMarkdownRenderer returns a MarkdownRenderer wired to the given highlighter.
// If hl is nil the renderer falls back to a DefaultCodeHighlighter at render time.
func NewMarkdownRenderer(hl CodeHighlighter) *MarkdownRenderer {
	return &MarkdownRenderer{highlighter: hl}
}

// Render parses content as markdown, walks the AST, and returns ANSI-styled text.
func (m MarkdownRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	hl := m.highlighter
	if hl == nil {
		hl = NewDefaultCodeHighlighter()
	}
	parser := markdown.NewParser()
	ast := parser.Parse(content)
	adapter := themeAdapter{theme: renderTheme(opts)}
	r := markdown.NewMarkdownASTRenderer(adapter, opts.Width, hl)
	out := r.Render(ast)
	logRender(ctx, "markdown", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (MarkdownRenderer) Name() string { return "markdown" }

// Supports reports whether the renderer handles the content type.
func (MarkdownRenderer) Supports(ct string) bool { return ct == ContentTypeMarkdown }

// ---------- code ----------

// CodeRenderer styles code content with the theme foreground color.
type CodeRenderer struct{}

var _ Renderer = (*CodeRenderer)(nil)

// Render styles code content.
func (CodeRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	out := renderTheme(opts).Fg().Render(wrapWidth(content, opts.Width))
	logRender(ctx, "code", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (CodeRenderer) Name() string { return "code" }

// Supports reports whether the renderer handles the content type.
func (CodeRenderer) Supports(ct string) bool { return ct == ContentTypeCode }

// ---------- table ----------

// TableRenderer renders tabular content with the primary style applied to
// header lines (those ending with a tab column separator).
type TableRenderer struct{}

var _ Renderer = (*TableRenderer)(nil)

// Render styles table rows.
func (TableRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	var sb strings.Builder
	for i, line := range strings.Split(content, "\n") {
		text := wrapWidth(line, opts.Width)
		if i == 0 {
			text = theme.Primary().Render(text)
		}
		sb.WriteString(text)
		sb.WriteString("\n")
	}
	out := strings.TrimRight(sb.String(), "\n")
	logRender(ctx, "table", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (TableRenderer) Name() string { return "table" }

// Supports reports whether the renderer handles the content type.
func (TableRenderer) Supports(ct string) bool { return ct == ContentTypeTable }

// ---------- diff ----------

// DiffRenderer styles unified diff hunks: additions in green, deletions in red,
// unchanged lines in the foreground style.
type DiffRenderer struct{}

var _ Renderer = (*DiffRenderer)(nil)

// Render styles diff lines by their leading marker.
func (DiffRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	var sb strings.Builder
	for _, line := range strings.Split(content, "\n") {
		var styled string
		switch {
		case strings.HasPrefix(line, "+"):
			styled = theme.Success().Render(line)
		case strings.HasPrefix(line, "-"):
			styled = theme.Error().Render(line)
		default:
			styled = theme.Fg().Render(line)
		}
		sb.WriteString(wrapWidth(styled, opts.Width))
		sb.WriteString("\n")
	}
	out := strings.TrimRight(sb.String(), "\n")
	logRender(ctx, "diff", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (DiffRenderer) Name() string { return "diff" }

// Supports reports whether the renderer handles the content type.
func (DiffRenderer) Supports(ct string) bool { return ct == ContentTypeDiff }

// ---------- error ----------

// ErrorRenderer renders error content in the theme error style.
type ErrorRenderer struct{}

var _ Renderer = (*ErrorRenderer)(nil)

// Render styles error content.
func (ErrorRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	out := renderTheme(opts).Error().Render(wrapWidth(content, opts.Width))
	logRender(ctx, "error", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (ErrorRenderer) Name() string { return "error" }

// Supports reports whether the renderer handles the content type.
func (ErrorRenderer) Supports(ct string) bool { return ct == ContentTypeError }

// ---------- tool_call ----------

// ToolCallRenderer renders a tool invocation in the secondary/bold style.
// The collapsed field tracks display state for the collapsible render methods
// (RenderCollapsed / RenderExpanded) defined in accordion.go.
type ToolCallRenderer struct {
	collapsed bool
}

var _ Renderer = (*ToolCallRenderer)(nil)

// NewToolCallRenderer returns a ToolCallRenderer with collapsed set to true,
// suitable for interactive accordion display.
func NewToolCallRenderer() *ToolCallRenderer {
	return &ToolCallRenderer{collapsed: true}
}

// Render styles tool call content.
func (ToolCallRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	label := theme.Primary().Bold(true).Render("[tool]")
	out := label + " " + theme.Fg().Render(wrapWidth(content, opts.Width))
	logRender(ctx, "tool_call", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (ToolCallRenderer) Name() string { return "tool_call" }

// Supports reports whether the renderer handles the content type.
func (ToolCallRenderer) Supports(ct string) bool { return ct == ContentTypeToolCall }

// ---------- tool_result ----------

// ToolResultRenderer renders a tool result in the theme success style.
type ToolResultRenderer struct{}

var _ Renderer = (*ToolResultRenderer)(nil)

// Render styles tool result content.
func (ToolResultRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	label := theme.Success().Bold(true).Render("[result]")
	out := label + " " + theme.Fg().Render(wrapWidth(content, opts.Width))
	logRender(ctx, "tool_result", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (ToolResultRenderer) Name() string { return "tool_result" }

// Supports reports whether the renderer handles the content type.
func (ToolResultRenderer) Supports(ct string) bool { return ct == ContentTypeToolResult }

// ---------- tool_output ----------

// ToolOutputRenderer renders a line of streaming tool output. It is similar
// to ToolResultRenderer but uses a distinct label so the user can distinguish
// real-time output from a final result.
type ToolOutputRenderer struct{}

var _ Renderer = (*ToolOutputRenderer)(nil)

// Render styles streaming tool output content. When opts.Stream is "stderr"
// the output is rendered in the error style with an [err] label; otherwise
// (stdout or empty) it uses the foreground style with an [output] label.
func (ToolOutputRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	if opts.Stream == "stderr" {
		label := theme.Error().Bold(true).Render("[err]")
		out := label + " " + theme.Error().Render(wrapWidth(content, opts.Width))
		logRender(ctx, "tool_output", opts.ContentType, len(out))
		return out
	}
	label := theme.Fg().Bold(true).Render("[output]")
	out := label + " " + theme.Fg().Render(wrapWidth(content, opts.Width))
	logRender(ctx, "tool_output", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (ToolOutputRenderer) Name() string { return ContentTypeToolOutput }

// Supports reports whether the renderer handles the content type.
func (ToolOutputRenderer) Supports(ct string) bool { return ct == ContentTypeToolOutput }

// ---------- thinking ----------

// ThinkingRenderer renders internal reasoning in the faint/italic style.
type ThinkingRenderer struct{}

var _ Renderer = (*ThinkingRenderer)(nil)

// Render styles reasoning content.
func (ThinkingRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	out := theme.Faint().Italic(true).Render(wrapWidth(content, opts.Width))
	logRender(ctx, "thinking", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (ThinkingRenderer) Name() string { return "thinking" }

// Supports reports whether the renderer handles the content type.
func (ThinkingRenderer) Supports(ct string) bool { return ct == ContentTypeThinking }

// ---------- progress ----------

// ProgressRenderer renders a progress bar bounded by the render width. The bar
// uses filled (█) and empty (░) cells, appends a percentage readout, and
// selects the bar color by tier: green below 50 %, yellow from 50 % to 79 %,
// red at 80 % and above.
type ProgressRenderer struct{}

var _ Renderer = (*ProgressRenderer)(nil)

// Render draws a progress bar; content is expected to be a float value in
// [0,1] as text. Non-numeric content is rendered as 0 %.
func (ProgressRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	width := opts.Width
	if width <= 0 {
		width = 40
	}
	filled := progressFrac(content, width)
	pct := progressPct(content)
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	style := progressStyle(theme, pct)
	out := style.Bold(true).Render("["+bar+"]") + " " + strconv.Itoa(pct) + "%"
	logRender(ctx, "progress", opts.ContentType, len(out))
	return out
}

// progressFrac converts textual progress into a filled-cell count.
func progressFrac(content string, width int) int {
	return int(progressValue(content) * float64(width))
}

// progressPct converts textual progress into an integer percentage [0,100].
func progressPct(content string) int {
	return int(progressValue(content) * 100)
}

// progressValue parses a float in [0,1] from content, clamping out-of-range
// values and returning 0 for non-numeric input.
func progressValue(content string) float64 {
	val, err := strconv.ParseFloat(content, 64)
	if err != nil {
		return 0
	}
	if val > 1.0 {
		val = 1.0
	}
	if val < 0 {
		val = 0
	}
	return val
}

// progressStyle selects the bar color by percentage tier.
func progressStyle(theme Theme, pct int) Style {
	switch {
	case pct < 50:
		return theme.Success()
	case pct < 80:
		return theme.Warning()
	default:
		return theme.Error()
	}
}

// Name identifies the renderer.
func (ProgressRenderer) Name() string { return "progress" }

// Supports reports whether the renderer handles the content type.
func (ProgressRenderer) Supports(ct string) bool { return ct == ContentTypeProgress }

// ---------- file_tree ----------

// FileTreeRenderer renders directory listings using the theme foreground and
// secondary styles for indent markers.
type FileTreeRenderer struct{}

var _ Renderer = (*FileTreeRenderer)(nil)

// Render styles file-tree content, decorating lines with a leading separator.
func (FileTreeRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	var sb strings.Builder
	for _, line := range strings.Split(content, "\n") {
		sb.WriteString(theme.Secondary().Render("│ "))
		sb.WriteString(theme.Fg().Render(wrapWidth(line, opts.Width)))
		sb.WriteString("\n")
	}
	out := strings.TrimRight(sb.String(), "\n")
	logRender(ctx, "file_tree", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (FileTreeRenderer) Name() string { return "file_tree" }

// Supports reports whether the renderer handles the content type.
func (FileTreeRenderer) Supports(ct string) bool { return ct == ContentTypeFileTree }

// ---------- image ----------

// ImageRenderer renders a placeholder for binary image content in the secondary
// style.
type ImageRenderer struct{}

var _ Renderer = (*ImageRenderer)(nil)

// Render emits a compact image placeholder.
func (ImageRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	out := renderTheme(opts).Secondary().Render("[image: " + wrapWidth(content, opts.Width) + "]")
	logRender(ctx, "image", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (ImageRenderer) Name() string { return "image" }

// Supports reports whether the renderer handles the content type.
func (ImageRenderer) Supports(ct string) bool { return ct == ContentTypeImage }

// ---------- link ----------

// LinkRenderer renders a hyperlink using OSC-8 terminal escape sequences when
// it detects a URL in the content.
type LinkRenderer struct{}

var _ Renderer = (*LinkRenderer)(nil)

// Render styles link content.
func (LinkRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	display := wrapWidth(content, opts.Width)
	out := theme.Primary().Underline(true).Render(display)
	logRender(ctx, "link", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (LinkRenderer) Name() string { return "link" }

// Supports reports whether the renderer handles the content type.
func (LinkRenderer) Supports(ct string) bool { return ct == ContentTypeLink }

// ---------- system ----------

// SystemRenderer renders system messages with the secondary style.
type SystemRenderer struct{}

var _ Renderer = (*SystemRenderer)(nil)

// Render styles system messages.
func (SystemRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	out := renderTheme(opts).Secondary().Render(wrapWidth(content, opts.Width))
	logRender(ctx, "system", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (SystemRenderer) Name() string { return "system" }

// Supports reports whether the renderer handles the content type.
func (SystemRenderer) Supports(ct string) bool { return ct == ContentTypeSystem }

// ---------- user ----------

// UserRenderer renders user messages with the theme fg style and a bold prefix.
type UserRenderer struct{}

var _ Renderer = (*UserRenderer)(nil)

// Render styles user messages.
func (UserRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	out := theme.Bold().Render("you: ") + theme.Fg().Render(wrapWidth(content, opts.Width))
	logRender(ctx, "user", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (UserRenderer) Name() string { return "user" }

// Supports reports whether the renderer handles the content type.
func (UserRenderer) Supports(ct string) bool { return ct == ContentTypeUser }

// ---------- assistant ----------

// AssistantRenderer renders assistant messages with the primary style.
type AssistantRenderer struct{}

var _ Renderer = (*AssistantRenderer)(nil)

// Render styles assistant messages.
func (AssistantRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	out := theme.Bold().Render("AI: ") + theme.Primary().Render(wrapWidth(content, opts.Width))
	logRender(ctx, "assistant", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (AssistantRenderer) Name() string { return "assistant" }

// Supports reports whether the renderer handles the content type.
func (AssistantRenderer) Supports(ct string) bool { return ct == ContentTypeAssistant }

// ---------- approval ----------

// ApprovalRenderer renders an approval prompt in the warning style.
type ApprovalRenderer struct{}

var _ Renderer = (*ApprovalRenderer)(nil)

// Render styles approval prompts.
func (ApprovalRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	label := theme.Warning().Bold(true).Render("[approval]")
	out := label + " " + theme.Fg().Render(wrapWidth(content, opts.Width))
	logRender(ctx, "approval", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (ApprovalRenderer) Name() string { return "approval" }

// Supports reports whether the renderer handles the content type.
func (ApprovalRenderer) Supports(ct string) bool { return ct == ContentTypeApproval }

// ---------- prompt ----------

// PromptRenderer renders a user prompt in the bold fg style.
type PromptRenderer struct{}

var _ Renderer = (*PromptRenderer)(nil)

// Render styles prompt content.
func (PromptRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	out := theme.Bold().Foreground(lipgloss.Color("#FFFFFF")).Render(wrapWidth(content, opts.Width))
	logRender(ctx, "prompt", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (PromptRenderer) Name() string { return "prompt" }

// Supports reports whether the renderer handles the content type.
func (PromptRenderer) Supports(ct string) bool { return ct == ContentTypePrompt }

// ---------- compaction ----------

// CompactionRenderer renders a session-compaction summary in the secondary
// italic style.
type CompactionRenderer struct{}

var _ Renderer = (*CompactionRenderer)(nil)

// Render styles compaction summaries.
func (CompactionRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	out := theme.Secondary().Italic(true).Render(wrapWidth(content, opts.Width))
	logRender(ctx, "compaction", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (CompactionRenderer) Name() string { return "compaction" }

// Supports reports whether the renderer handles the content type.
func (CompactionRenderer) Supports(ct string) bool { return ct == ContentTypeCompaction }

// ---------- streaming ----------

// StreamingRenderer renders a chunk of streaming text in the primary style.
// Streaming accumulation is owned by the App, which feeds the running buffer to
// this renderer on each update.
type StreamingRenderer struct{}

var _ Renderer = (*StreamingRenderer)(nil)

// Render styles a streaming chunk.
func (StreamingRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	out := renderTheme(opts).Primary().Render(wrapWidth(content, opts.Width))
	logRender(ctx, "streaming", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (StreamingRenderer) Name() string { return "streaming" }

// Supports reports whether the renderer handles the content type.
func (StreamingRenderer) Supports(ct string) bool { return ct == ContentTypeStreaming }

// streaming marks this as a streaming-capable renderer.
func (StreamingRenderer) streaming() bool { return true }

// ---------- streaming_code ----------

// StreamingCodeRenderer renders streaming code chunks in the code style.
type StreamingCodeRenderer struct{}

var _ Renderer = (*StreamingCodeRenderer)(nil)

// Render styles a streaming code chunk.
func (StreamingCodeRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	out := renderTheme(opts).Fg().Render(wrapWidth(content, opts.Width))
	logRender(ctx, "streaming_code", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (StreamingCodeRenderer) Name() string { return "streaming_code" }

// Supports reports whether the renderer handles the content type.
func (StreamingCodeRenderer) Supports(ct string) bool { return ct == ContentTypeStreamingCode }

// streaming marks this as a streaming-capable renderer.
func (StreamingCodeRenderer) streaming() bool { return true }

// ---------- streaming_thinking ----------

// StreamingThinkingRenderer renders streaming reasoning chunks in the faint
// italic style.
type StreamingThinkingRenderer struct{}

var _ Renderer = (*StreamingThinkingRenderer)(nil)

// Render styles a streaming reasoning chunk.
func (StreamingThinkingRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	out := theme.Faint().Italic(true).Render(wrapWidth(content, opts.Width))
	logRender(ctx, "streaming_thinking", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (StreamingThinkingRenderer) Name() string { return "streaming_thinking" }

// Supports reports whether the renderer handles the content type.
func (StreamingThinkingRenderer) Supports(ct string) bool {
	return ct == ContentTypeStreamingThink
}

// streaming marks this as a streaming-capable renderer.
func (StreamingThinkingRenderer) streaming() bool { return true }

// ---------- blank ----------

// BlankRenderer renders empty space, useful for vertical rhythm.
type BlankRenderer struct{}

var _ Renderer = (*BlankRenderer)(nil)

// Render emits a blank line.
func (BlankRenderer) Render(context.Context, string, RenderOpts) string { return "" }

// Name identifies the renderer.
func (BlankRenderer) Name() string { return "blank" }

// Supports reports whether the renderer handles the content type.
func (BlankRenderer) Supports(ct string) bool { return ct == ContentTypeBlank }

// ---------- separator ----------

// SeparatorRenderer renders a horizontal rule spanning the render width.
type SeparatorRenderer struct{}

var _ Renderer = (*SeparatorRenderer)(nil)

// Render emits a horizontal separator line.
func (SeparatorRenderer) Render(_ context.Context, _ string, opts RenderOpts) string {
	w := opts.Width
	if w <= 0 {
		w = 60
	}
	return strings.Repeat("─", w)
}

// Name identifies the renderer.
func (SeparatorRenderer) Name() string { return "separator" }

// Supports reports whether the renderer handles the content type.
func (SeparatorRenderer) Supports(ct string) bool { return ct == ContentTypeSeparator }

// ---------- status ----------

// StatusRenderer renders a status line with the theme secondary style.
type StatusRenderer struct{}

var _ Renderer = (*StatusRenderer)(nil)

// Render styles status content.
func (StatusRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	out := theme.Secondary().Render(wrapWidth(content, opts.Width))
	logRender(ctx, "status", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (StatusRenderer) Name() string { return "status" }

// Supports reports whether the renderer handles the content type.
func (StatusRenderer) Supports(ct string) bool { return ct == ContentTypeStatus }
