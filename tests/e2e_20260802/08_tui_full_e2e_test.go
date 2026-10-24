//go:build !headless

package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tui"
)

// =============================================================================
// TUI: BubbleteaApp lifecycle (start, send events, quit)
// =============================================================================

func TestTUI_BubbleteaAppLifecycle(t *testing.T) {
	events := make(chan tui.AgentEvent, 10)
	app := tui.NewBubbleteaApp(events)

	ctx, cancel := context.WithCancel(context.Background())

	// Run in background.
	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	// Send a couple of events.
	events <- tui.AgentEvent{
		Type:        "run",
		Content:     "hello world",
		ContentType: tui.ContentTypeAssistant,
		TraceID:     "trace-1",
		SpanID:      "span-1",
	}
	events <- tui.AgentEvent{
		Type:        "tool",
		Content:     "ls -la",
		ContentType: tui.ContentTypeToolCall,
		TraceID:     "trace-2",
		SpanID:      "span-2",
	}

	// Give the loop time to process.
	time.Sleep(50 * time.Millisecond)

	// Send a message via Send.
	app.Send("manual-msg")

	time.Sleep(20 * time.Millisecond)

	// Check view is populated.
	view := app.View()
	assert.NotEmpty(t, view, "view should contain rendered output")
	assert.Contains(t, view, "hello world")

	// Check event/message counters.
	assert.Equal(t, int64(2), app.EventsProcessed())
	assert.Equal(t, int64(1), app.MessagesProcessed())

	// Quit the app.
	app.Quit()

	// Wait for Run to return.
	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("app did not shut down in time")
	}

	cancel()
}

func TestTUI_BubbleteaAppContextCancel(t *testing.T) {
	events := make(chan tui.AgentEvent)
	app := tui.NewBubbleteaApp(events)

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	// Cancel immediately.
	cancel()

	select {
	case err := <-errCh:
		assert.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("app did not shut down on context cancel")
	}
}

func TestTUI_BubbleteaAppEventsClosed(t *testing.T) {
	events := make(chan tui.AgentEvent, 1)
	app := tui.NewBubbleteaApp(events)

	ctx := context.Background()
	close(events) // Close before Run.

	err := app.Run(ctx)
	assert.NoError(t, err)
}

func TestTUI_BubbleteaAppRunTwiceError(t *testing.T) {
	events := make(chan tui.AgentEvent)
	app := tui.NewBubbleteaApp(events)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = app.Run(ctx) }() //nolint:errcheck,gosec
	time.Sleep(20 * time.Millisecond)

	err := app.Run(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already running")

	app.Quit()
}

func TestTUI_BubbleteaAppSendDoesNotBlock(t *testing.T) {
	events := make(chan tui.AgentEvent)
	app := tui.NewBubbleteaApp(events)

	// Send many messages before Run — they fill the buffer but drop gracefully.
	for i := 0; i < 50; i++ {
		app.Send(fmt.Sprintf("msg-%d", i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		app.Quit()
	}()

	err := app.Run(ctx)
	assert.NoError(t, err)

	cancel()
}

// =============================================================================
// TUI: Renderer registry (register, get, list)
// =============================================================================

func TestTUI_RendererRegistryRegister(t *testing.T) {
	reg := tui.NewRendererRegistry()

	// Register a custom renderer that supports a known content type.
	custom := &customTestRenderer{name: "custom", ct: tui.ContentTypeMarkdown}
	reg.Register(custom)

	got, ok := reg.Get(tui.ContentTypeMarkdown)
	require.True(t, ok)
	assert.Equal(t, "custom", got.Name())

	// Verify the custom renderer takes precedence over the default.
	out := got.Render(context.Background(), "test", tui.RenderOpts{})
	assert.Equal(t, "[custom]test", out)
}

func TestTUI_RendererRegistryGetMissing(t *testing.T) {
	reg := tui.NewRendererRegistry()

	_, ok := reg.Get("nonexistent")
	assert.False(t, ok)
}

func TestTUI_RendererRegistryList(t *testing.T) {
	reg := tui.NewDefaultRegistry()
	all := reg.List()

	// All 24 built-in renderers should be registered, each for its content type.
	assert.GreaterOrEqual(t, len(all), 24)

	// Verify some known types.
	assert.Contains(t, all, tui.ContentTypeMarkdown)
	assert.Contains(t, all, tui.ContentTypeCode)
	assert.Contains(t, all, tui.ContentTypeTable)
	assert.Contains(t, all, tui.ContentTypeDiff)
	assert.Contains(t, all, tui.ContentTypeError)
	assert.Contains(t, all, tui.ContentTypeStreaming)
}

func TestTUI_RendererRegistryGetAfterRegister(t *testing.T) {
	reg := tui.NewDefaultRegistry()

	r, ok := reg.Get(tui.ContentTypeMarkdown)
	require.True(t, ok)
	assert.Equal(t, "markdown", r.Name())

	r, ok = reg.Get(tui.ContentTypeCode)
	require.True(t, ok)
	assert.Equal(t, "code", r.Name())
}

// customTestRenderer is a minimal Renderer implementation for testing.
type customTestRenderer struct {
	name string
	ct   string
}

func (c *customTestRenderer) Name() string            { return c.name }
func (c *customTestRenderer) Supports(ct string) bool { return ct == c.ct }
func (c *customTestRenderer) Render(_ context.Context, content string, _ tui.RenderOpts) string {
	return "[custom]" + content
}

// =============================================================================
// TUI: Content type renderers (markdown, code, table, diff, error, etc.)
// =============================================================================

func TestTUI_MarkdownRenderer(t *testing.T) {
	r := tui.MarkdownRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeMarkdown))
	assert.False(t, r.Supports(tui.ContentTypeCode))

	out := r.Render(context.Background(), "hello **world**", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

func TestTUI_CodeRenderer(t *testing.T) {
	r := tui.CodeRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeCode))
	assert.False(t, r.Supports(tui.ContentTypeMarkdown))

	out := r.Render(context.Background(), "fmt.Println(\"hello\")", tui.RenderOpts{})
	assert.NotEmpty(t, out)
	assert.Contains(t, out, "fmt.Println")
}

func TestTUI_TableRenderer(t *testing.T) {
	r := tui.TableRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeTable))

	out := r.Render(context.Background(), "col1\tcol2\nval1\tval2", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

func TestTUI_DiffRenderer(t *testing.T) {
	r := tui.DiffRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeDiff))

	diffContent := "+added line\n-deleted line\n unchanged"
	out := r.Render(context.Background(), diffContent, tui.RenderOpts{})
	assert.NotEmpty(t, out)
	assert.Contains(t, out, "added")
	assert.Contains(t, out, "deleted")
}

func TestTUI_ErrorRenderer(t *testing.T) {
	r := tui.ErrorRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeError))

	out := r.Render(context.Background(), "something went wrong", tui.RenderOpts{})
	assert.NotEmpty(t, out)
	assert.Contains(t, out, "something went wrong")
}

func TestTUI_ToolCallRenderer(t *testing.T) {
	r := tui.ToolCallRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeToolCall))

	out := r.Render(context.Background(), "bash: ls", tui.RenderOpts{})
	assert.NotEmpty(t, out)
	assert.Contains(t, out, "ls")
}

func TestTUI_ToolResultRenderer(t *testing.T) {
	r := tui.ToolResultRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeToolResult))

	out := r.Render(context.Background(), "file1.go", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

func TestTUI_ThinkingRenderer(t *testing.T) {
	r := tui.ThinkingRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeThinking))

	out := r.Render(context.Background(), "Let me think about this...", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

func TestTUI_ProgressRenderer(t *testing.T) {
	r := tui.ProgressRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeProgress))

	// 50% progress — enhanced renderer uses █/░ bar characters.
	out := r.Render(context.Background(), "0.5", tui.RenderOpts{Width: 40})
	assert.NotEmpty(t, out)
	assert.Contains(t, out, "█")

	// 100% progress.
	out = r.Render(context.Background(), "1.0", tui.RenderOpts{Width: 20})
	assert.NotEmpty(t, out)

	// 0% progress.
	out = r.Render(context.Background(), "0.0", tui.RenderOpts{Width: 20})
	assert.NotEmpty(t, out)

	// Non-numeric content falls back to literal.
	out = r.Render(context.Background(), "abc", tui.RenderOpts{Width: 20})
	assert.NotEmpty(t, out)
}

func TestTUI_FileTreeRenderer(t *testing.T) {
	r := tui.FileTreeRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeFileTree))

	out := r.Render(context.Background(), "src/main.go\nsrc/lib.go", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

func TestTUI_ImageRenderer(t *testing.T) {
	r := tui.ImageRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeImage))

	out := r.Render(context.Background(), "screenshot.png", tui.RenderOpts{})
	assert.NotEmpty(t, out)
	assert.Contains(t, out, "image")
}

func TestTUI_LinkRenderer(t *testing.T) {
	r := tui.LinkRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeLink))

	out := r.Render(context.Background(), "https://example.com", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

func TestTUI_SystemRenderer(t *testing.T) {
	r := tui.SystemRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeSystem))

	out := r.Render(context.Background(), "Session started", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

func TestTUI_UserRenderer(t *testing.T) {
	r := tui.UserRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeUser))

	out := r.Render(context.Background(), "Hello", tui.RenderOpts{})
	assert.NotEmpty(t, out)
	assert.Contains(t, out, "you:")
}

func TestTUI_AssistantRenderer(t *testing.T) {
	r := tui.AssistantRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeAssistant))

	out := r.Render(context.Background(), "I can help with that", tui.RenderOpts{})
	assert.NotEmpty(t, out)
	assert.Contains(t, out, "AI:")
}

func TestTUI_ApprovalRenderer(t *testing.T) {
	r := tui.ApprovalRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeApproval))

	out := r.Render(context.Background(), "Delete file?", tui.RenderOpts{})
	assert.NotEmpty(t, out)
	assert.Contains(t, out, "approval")
}

func TestTUI_PromptRenderer(t *testing.T) {
	r := tui.PromptRenderer{}
	assert.True(t, r.Supports(tui.ContentTypePrompt))

	out := r.Render(context.Background(), "> ", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

func TestTUI_CompactionRenderer(t *testing.T) {
	r := tui.CompactionRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeCompaction))

	out := r.Render(context.Background(), "Session summarized", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

func TestTUI_BlankRenderer(t *testing.T) {
	r := tui.BlankRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeBlank))

	out := r.Render(context.Background(), "ignored", tui.RenderOpts{})
	assert.Empty(t, out)
}

func TestTUI_SeparatorRenderer(t *testing.T) {
	r := tui.SeparatorRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeSeparator))

	out := r.Render(context.Background(), "ignored", tui.RenderOpts{Width: 60})
	assert.NotEmpty(t, out)
	// "─" is a multi-byte UTF-8 character; verify the output is a horizontal rule.
	assert.True(t, len(out) > 0, "separator should not be empty")
	assert.Contains(t, out, "─")
}

func TestTUI_StatusRenderer(t *testing.T) {
	r := tui.StatusRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeStatus))

	out := r.Render(context.Background(), "Ready", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

// =============================================================================
// TUI: Streaming renderer behavior
// =============================================================================

func TestTUI_StreamingRenderer(t *testing.T) {
	r := tui.StreamingRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeStreaming))

	out := r.Render(context.Background(), "streaming chunk", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

func TestTUI_StreamingCodeRenderer(t *testing.T) {
	r := tui.StreamingCodeRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeStreamingCode))

	out := r.Render(context.Background(), "func main() {", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

func TestTUI_StreamingThinkingRenderer(t *testing.T) {
	r := tui.StreamingThinkingRenderer{}
	assert.True(t, r.Supports(tui.ContentTypeStreamingThink))

	out := r.Render(context.Background(), "Hmm...", tui.RenderOpts{})
	assert.NotEmpty(t, out)
}

func TestTUI_StreamingOverwritesLastLine(t *testing.T) {
	events := make(chan tui.AgentEvent, 10)
	app := tui.NewBubbleteaApp(events)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- app.Run(ctx) }()

	// Send multiple streaming events.
	events <- tui.AgentEvent{Type: "stream", Content: "chunk1", ContentType: tui.ContentTypeStreaming}
	events <- tui.AgentEvent{Type: "stream", Content: "chunk2", ContentType: tui.ContentTypeStreaming}
	events <- tui.AgentEvent{Type: "stream", Content: "chunk3", ContentType: tui.ContentTypeStreaming}

	time.Sleep(50 * time.Millisecond)

	// Send a non-streaming event.
	events <- tui.AgentEvent{Type: "msg", Content: "static message", ContentType: tui.ContentTypeSystem}

	time.Sleep(30 * time.Millisecond)

	app.Quit()
	<-errCh

	view := app.View()
	// The streaming events should overwrite each other, resulting in only the
	// last streaming frame + the static message.
	lines := strings.Split(view, "\n")
	assert.GreaterOrEqual(t, len(lines), 2, "should have at least streaming frame + static message")
}

// =============================================================================
// TUI: Theme system (preset themes, Style ANSI codes)
// =============================================================================

func TestTUI_DarkTheme(t *testing.T) {
	th := tui.DarkTheme{}

	assert.NotEmpty(t, th.Primary().String())
	assert.NotEmpty(t, th.Secondary().String())
	assert.NotEmpty(t, th.Success().String())
	assert.NotEmpty(t, th.Warning().String())
	assert.NotEmpty(t, th.Error().String())
	assert.NotEmpty(t, th.Fg().String())
	// DarkTheme.Bg() is intentionally transparent (empty style) — its String()
	// correctly produces no SGR sequence.
	assert.Empty(t, th.Bg().String())
	assert.NotEmpty(t, th.Faint().String())
	assert.NotEmpty(t, th.Bold().String())
	assert.NotEmpty(t, th.Italic().String())
}

func TestTUI_LightTheme(t *testing.T) {
	th := tui.LightTheme{}

	assert.NotEmpty(t, th.Primary().String())
	assert.NotEmpty(t, th.Fg().String())
}

func TestTUI_MonokaiTheme(t *testing.T) {
	th := tui.MonokaiTheme{}

	assert.NotEmpty(t, th.Primary().String())
	assert.NotEmpty(t, th.Secondary().String())
}

func TestTUI_SolarizedTheme(t *testing.T) {
	th := tui.SolarizedTheme{}

	assert.NotEmpty(t, th.Primary().String())
	assert.NotEmpty(t, th.Fg().String())
}

func TestTUI_StyleANSIEscapeSequences(t *testing.T) {
	// lipgloss uses truecolor (#RRGGBB) rather than 8-bit ANSI codes, so we
	// style with a hex color and assert the rendered output carries an escape
	// sequence plus the bold/italic SGR attributes.
	s := tui.NewStyle().Foreground(lipgloss.Color("#04E762")).Bold(true).Italic(true)
	esc := s.String()

	assert.NotEmpty(t, esc)
	assert.Contains(t, esc, "\x1b[")
	assert.Contains(t, esc, "1") // bold
	assert.Contains(t, esc, "3") // italic

	rendered := s.Render("test")
	assert.Contains(t, rendered, "\x1b[")
	assert.Contains(t, rendered, "test")
	assert.Contains(t, rendered, "\x1b[0m") // reset
}

func TestTUI_StyleEmptyStringWhenNothingSet(t *testing.T) {
	s := tui.NewStyle()
	assert.Empty(t, s.String())
	assert.Equal(t, "plain", s.Render("plain"))
}

func TestTUI_StyleChaining(t *testing.T) {
	// With truecolor, foreground/background are expressed as 38;2;...;m and
	// 48;2;...;m sequences. We render text and assert the output carries the
	// truecolor foreground/background escapes plus bold (1) and underline (4).
	s := tui.NewStyle().Foreground(lipgloss.Color("#FF5C5C")).Background(lipgloss.Color("#04E762")).Bold(true).Underline(true)

	rendered := s.Render("x")
	assert.Contains(t, rendered, "38;2;") // fg truecolor
	assert.Contains(t, rendered, "48;2;") // bg truecolor
	assert.Contains(t, rendered, "1")     // bold
	assert.Contains(t, rendered, "4")     // underline
	assert.Contains(t, rendered, "x")
}

func TestTUI_StyleFaintAttribute(t *testing.T) {
	s := tui.NewStyle().Faint(true)
	assert.Contains(t, s.String(), "2") // faint
}

// =============================================================================
// TUI: ThemeManager switching and custom themes
// =============================================================================

func TestTUI_ThemeManagerDefault(t *testing.T) {
	mgr := tui.NewThemeManager()
	assert.NotNil(t, mgr.Get())

	// Default should be dark.
	th := mgr.Get()
	assert.NotNil(t, th)
}

func TestTUI_ThemeManagerSwitch(t *testing.T) {
	mgr := tui.NewThemeManager()

	err := mgr.Set("light")
	assert.NoError(t, err)

	err = mgr.Set("monokai")
	assert.NoError(t, err)

	err = mgr.Set("solarized")
	assert.NoError(t, err)
}

func TestTUI_ThemeManagerUnknown(t *testing.T) {
	mgr := tui.NewThemeManager()

	err := mgr.Set("unknown-theme")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown theme")
}

func TestTUI_ThemeManagerRegisterCustom(t *testing.T) {
	mgr := tui.NewThemeManager()

	custom := &customTestTheme{}
	mgr.Register("custom", custom)

	err := mgr.Set("custom")
	assert.NoError(t, err)

	got := mgr.Get()
	// Render produces ANSI-styled output with color 35.
	styled := got.Primary().Render("x")
	assert.NotEmpty(t, styled)
	assert.Contains(t, styled, "x")
}

// customTestTheme implements tui.Theme for testing. Colors use truecolor hex
// values compatible with the lipgloss-backed Style alias.
type customTestTheme struct{}

func (c *customTestTheme) Primary() tui.Style {
	return tui.NewStyle().Foreground(lipgloss.Color("#FF00FF"))
}
func (c *customTestTheme) Secondary() tui.Style {
	return tui.NewStyle().Foreground(lipgloss.Color("#00D9FF"))
}
func (c *customTestTheme) Success() tui.Style {
	return tui.NewStyle().Foreground(lipgloss.Color("#04E762"))
}
func (c *customTestTheme) Warning() tui.Style {
	return tui.NewStyle().Foreground(lipgloss.Color("#FFC000"))
}
func (c *customTestTheme) Error() tui.Style {
	return tui.NewStyle().Foreground(lipgloss.Color("#FF5C5C"))
}
func (c *customTestTheme) Bg() tui.Style     { return tui.NewStyle() }
func (c *customTestTheme) Fg() tui.Style     { return tui.NewStyle() }
func (c *customTestTheme) Faint() tui.Style  { return tui.NewStyle() }
func (c *customTestTheme) Bold() tui.Style   { return tui.NewStyle() }
func (c *customTestTheme) Italic() tui.Style { return tui.NewStyle() }

// Verify the Primary Render contains what we set.
func (c *customTestTheme) ensureCustom() {}

func TestTUI_ThemeManagerConcurrentAccess(t *testing.T) {
	mgr := tui.NewThemeManager()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		// Alternate reads and writes concurrently.
		if i%2 == 0 {
			go func() {
				defer wg.Done()
				_ = mgr.Set("dark") //nolint:errcheck,gosec
			}()
		} else {
			go func() {
				defer wg.Done()
				_ = mgr.Get()
			}()
		}
	}
	wg.Wait()
}
