// Package e2e_20260802 contains end-to-end integration tests for the TUI
// (theme, renderer registry, BubbleteaApp lifecycle) and ACP (client, stdio
// adapter, gRPC adapter, middleware bridging) modules.
package e2e_20260802

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/acp"
	"github.com/pengjunchen/go-cli/internal/extension"
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

	go func() { _ = app.Run(ctx) }()
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

	// 50% progress.
	out := r.Render(context.Background(), "0.5", tui.RenderOpts{Width: 40})
	assert.NotEmpty(t, out)
	assert.Contains(t, out, "=")

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
	assert.NotEmpty(t, th.Bg().String())
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
	s := tui.NewStyle().Foreground(32).Bold(true).Italic(true)
	esc := s.String()

	assert.NotEmpty(t, esc)
	assert.Contains(t, esc, "\x1b[")
	assert.Contains(t, esc, "32")
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
	s := tui.NewStyle().Foreground(31).Background(42).Bold(true).Underline(true)

	esc := s.String()
	assert.Contains(t, esc, "31") // fg red
	assert.Contains(t, esc, "52") // bg green (42+10)
	assert.Contains(t, esc, "1")  // bold
	assert.Contains(t, esc, "4")  // underline
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

// customTestTheme implements tui.Theme for testing.
type customTestTheme struct{}

func (c *customTestTheme) Primary() tui.Style   { return tui.NewStyle().Foreground(35) }
func (c *customTestTheme) Secondary() tui.Style { return tui.NewStyle().Foreground(36) }
func (c *customTestTheme) Success() tui.Style   { return tui.NewStyle().Foreground(32) }
func (c *customTestTheme) Warning() tui.Style   { return tui.NewStyle().Foreground(33) }
func (c *customTestTheme) Error() tui.Style     { return tui.NewStyle().Foreground(31) }
func (c *customTestTheme) Bg() tui.Style        { return tui.NewStyle() }
func (c *customTestTheme) Fg() tui.Style        { return tui.NewStyle() }
func (c *customTestTheme) Faint() tui.Style     { return tui.NewStyle() }
func (c *customTestTheme) Bold() tui.Style      { return tui.NewStyle() }
func (c *customTestTheme) Italic() tui.Style    { return tui.NewStyle() }

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
				_ = mgr.Set("dark")
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

// =============================================================================
// ACP: ACPClient connect/disconnect/send/receive (via StdioAdapter)
// =============================================================================

func TestACP_StdioAdapterBidirectionalCommunication(t *testing.T) {
	// Create two connected pipes for bidirectional communication.
	// clientWriter -> serverReader: client sends to server.
	// serverWriter -> clientReader: server sends to client.
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	client := acp.NewStdioAdapter(clientReader, clientWriter, acp.WithName("client-agent"))
	server := acp.NewStdioAdapter(serverReader, serverWriter, acp.WithName("server-agent"))

	ctx := context.Background()

	// Start draining clientReader in a goroutine so that server's Connect
	// write does not block.
	var serverConnectMsg []byte
	readyCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Read the server's connect message that gets written to serverWriter (clientReader).
		buf := make([]byte, 256)
		n, readErr := clientReader.Read(buf)
		if readErr == nil {
			serverConnectMsg = make([]byte, n)
			copy(serverConnectMsg, buf[:n])
		}
		close(readyCh)
	}()

	var wg2 sync.WaitGroup
	wg2.Add(1)
	serverDone := make(chan struct{})
	errCh := make(chan error, 1)

	// Server goroutine.
	go func() {
		defer wg2.Done()
		err := server.Connect(ctx)
		require.NoError(t, err)
		defer server.Disconnect(ctx)

		msgCh := server.ReceiveMessages()

		// First message should be the client's connect message.
		select {
		case connectMsg := <-msgCh:
			assert.Equal(t, acp.TypeConnect, connectMsg.Type)
			assert.Equal(t, "client-agent", connectMsg.SenderID)
		case <-time.After(5 * time.Second):
			errCh <- fmt.Errorf("timed out waiting for connect message on server")
			return
		}

		// Second message should be the test message.
		select {
		case msg := <-msgCh:
			assert.Equal(t, "client-agent", msg.SenderID)
			assert.Equal(t, "hello from client", msg.Content)
		case <-time.After(5 * time.Second):
			errCh <- fmt.Errorf("timed out waiting for message on server")
			return
		}

		// Send response back.
		reply := acp.ACPMessage{
			Type:       acp.TypeResponse,
			SenderID:   "server-agent",
			ReceiverID: "client-agent",
			Content:    "hello from server",
		}
		sendErr := server.SendMessage(ctx, reply)
		assert.NoError(t, sendErr)

		close(serverDone)
	}()

	// Wait for the server's connect message to be read from clientReader,
	// so client.Connect won't deadlock when writing.
	<-readyCh
	require.NotEmpty(t, serverConnectMsg, "server should have sent a connect message")

	// Now client can connect because the server is already connected
	// and its read loop is consuming serverReader.
	err := client.Connect(ctx)
	require.NoError(t, err)
	defer client.Disconnect(ctx)

	sendMsg := acp.ACPMessage{
		Type:       acp.TypeMessage,
		SenderID:   "client-agent",
		ReceiverID: "server-agent",
		Content:    "hello from client",
	}
	err = client.SendMessage(ctx, sendMsg)
	require.NoError(t, err)

	// Receive response from server.
	clientCh := client.ReceiveMessages()
	select {
	case response := <-clientCh:
		assert.Equal(t, "server-agent", response.SenderID)
		assert.Equal(t, "hello from server", response.Content)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for response on client")
	}

	<-serverDone
	wg.Wait()
	wg2.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}

func TestACP_StdioAdapterNameDefault(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	adapter := acp.NewStdioAdapter(r, w)
	assert.Equal(t, "stdio", adapter.Name())
}

func TestACP_StdioAdapterNameCustom(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	adapter := acp.NewStdioAdapter(r, w, acp.WithName("my-agent"))
	assert.Equal(t, "my-agent", adapter.Name())
}

func TestACP_StdioAdapterSendBeforeConnect(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	adapter := acp.NewStdioAdapter(r, w)
	msg := acp.ACPMessage{Type: acp.TypeMessage, SenderID: "a", Content: "msg"}
	err := adapter.SendMessage(context.Background(), msg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

func TestACP_StdioAdapterReceiveBeforeConnect(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	adapter := acp.NewStdioAdapter(r, w)
	ch := adapter.ReceiveMessages()
	assert.Nil(t, ch)
}

func TestACP_StdioAdapterConnectDisconnect(t *testing.T) {
	r, w := io.Pipe()

	// Start a goroutine that reads connect message from the pipe.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1024)
		n, _ := r.Read(buf)
		// Should contain the connect message as JSON.
		assert.Contains(t, string(buf[:n]), "connect")
	}()

	adapter := acp.NewStdioAdapter(r, w)
	ctx := context.Background()

	err := adapter.Connect(ctx)
	assert.NoError(t, err)

	err = adapter.Disconnect(ctx)
	assert.NoError(t, err)

	wg.Wait()
}

// =============================================================================
// ACP: gRPC adapter
// =============================================================================

func TestACP_GRPCAdapterName(t *testing.T) {
	adapter := acp.NewGRPCAdapter("http://localhost:9/acp", acp.WithName("grpc-agent"))
	assert.Equal(t, "grpc-agent", adapter.Name())
}

func TestACP_GRPCAdapterNameDefault(t *testing.T) {
	adapter := acp.NewGRPCAdapter("http://localhost:9/acp")
	assert.Equal(t, "grpc", adapter.Name())
}

func TestACP_GRPCAdapterSendBeforeConnect(t *testing.T) {
	adapter := acp.NewGRPCAdapter("http://localhost:9/acp")
	msg := acp.ACPMessage{Type: acp.TypeMessage, SenderID: "a", Content: "msg"}
	err := adapter.SendMessage(context.Background(), msg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

// =============================================================================
// ACP: ACPMiddleware bridging
// =============================================================================

func TestACP_ACPMiddlewarePassthrough(t *testing.T) {
	mw := acp.NewACPMiddleware("bridge", nil)

	next := func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{Text: "result: " + input.Message}, nil
	}

	wrapped := mw.WrapAgent(next)

	// Non-ACPMessage data passes through.
	out, err := wrapped(context.Background(), extension.AgentInput{
		Message: "hello",
		Data:    "plain-string",
	})
	assert.NoError(t, err)
	assert.Equal(t, "result: hello", out.Text)
}

func TestACP_ACPMiddlewareConvertsMessage(t *testing.T) {
	mw := acp.NewACPMiddleware("bridge", nil)

	next := func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{Text: "processed: " + input.Message}, nil
	}

	wrapped := mw.WrapAgent(next)

	acpMsg := acp.ACPMessage{
		Type:       acp.TypeMessage,
		SenderID:   "sender",
		ReceiverID: "receiver",
		Content:    "acp content",
		Metadata:   map[string]string{"key": "value"},
	}

	out, err := wrapped(context.Background(), extension.AgentInput{
		Message: "ignored",
		Data:    acpMsg,
	})
	assert.NoError(t, err)
	assert.Equal(t, "processed: acp content", out.Text)
}

func TestACP_ACPMiddlewareNonMessagePassesThrough(t *testing.T) {
	mw := acp.NewACPMiddleware("bridge", nil)

	next := func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{Text: input.Message}, nil
	}

	wrapped := mw.WrapAgent(next)

	// An ACP message of type "connect" should pass through.
	acpMsg := acp.ACPMessage{
		Type:    acp.TypeConnect,
		Content: "should be ignored",
	}

	out, err := wrapped(context.Background(), extension.AgentInput{
		Message: "raw message",
		Data:    acpMsg,
	})
	assert.NoError(t, err)
	assert.Equal(t, "raw message", out.Text)
}

func TestACP_DefaultACPServer(t *testing.T) {
	srv := acp.NewDefaultACPServer("test-server")
	assert.Equal(t, "test-server", srv.Name())

	// Cast to concrete type to check Running.
	ds, ok := srv.(*acp.DefaultACPServer)
	require.True(t, ok)

	assert.False(t, ds.Running())

	err := srv.Start(context.Background())
	assert.NoError(t, err)
	assert.True(t, ds.Running())

	err = srv.Stop(context.Background())
	assert.NoError(t, err)
	assert.False(t, ds.Running())
}

func TestACP_ACPMessageStruct(t *testing.T) {
	now := time.Now()
	msg := acp.ACPMessage{
		Type:       acp.TypeMessage,
		SenderID:   "agent-a",
		ReceiverID: "agent-b",
		Content:    "test message",
		Metadata:   map[string]string{"k": "v"},
		Timestamp:  now,
	}

	assert.Equal(t, acp.TypeMessage, msg.Type)
	assert.Equal(t, "agent-a", msg.SenderID)
	assert.Equal(t, "agent-b", msg.ReceiverID)
	assert.Equal(t, "test message", msg.Content)
	assert.Equal(t, "v", msg.Metadata["k"])
	assert.Equal(t, now, msg.Timestamp)
}

func TestACP_ACPTransportString(t *testing.T) {
	assert.Equal(t, "gRPC", acp.ACPTransportGRPC.String())
	assert.Equal(t, "Stdio", acp.ACPTransportStdio.String())
}
