package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
	"unsafe"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestStreamingMarkdownAppendChunkHandlesSplitUTF8AndANSI(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	ctx := context.Background()
	opts := RenderOpts{ContentType: ContentTypeStreaming, Width: 40}

	utf8Chunk := []byte("你")
	first := renderer.AppendChunk(ctx, string(utf8Chunk[:1]), opts)
	require.NotContains(t, first.Content, "你", "a partial UTF-8 rune must stay buffered")
	second := renderer.AppendChunk(ctx, string(utf8Chunk[1:]), opts)
	require.Contains(t, second.Content, "你")
	require.True(t, utf8.ValidString(second.Content))

	third := renderer.AppendChunk(ctx, "\x1b[31", opts)
	require.NotContains(t, third.Content, "\x1b", "an incomplete CSI must not reach the terminal")
	fourth := renderer.AppendChunk(ctx, "mred\x1b[0m", opts)
	require.Contains(t, ansi.Strip(fourth.Content), "red")
	require.NotContains(t, ansi.Strip(fourth.Content), "31", "CSI parameters are not visible text")
	require.True(t, utf8.ValidString(fourth.Content))
}

func TestStreamingMarkdownAppendChunkLinesMatchWindowContent(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	opts := RenderOpts{ContentType: ContentTypeStreaming, Width: 12}
	for _, chunk := range []string{"first line\n", "second line with a long tail", "\x1b[31m red\x1b[0m", "\nlast"} {
		want := renderer.AppendChunk(context.Background(), chunk, opts)
		got := renderer.WindowLines()
		lines := append([]string(nil), got.Lines...)
		if got.HasCurrentLine {
			lines = append(lines, got.CurrentLine)
		}
		if got.HasNextLine {
			lines = append(lines, got.NextLine)
		}
		require.Equal(t, want.Content, strings.Join(lines, "\n"))
		require.Equal(t, want.HasHiddenPrefix, got.HasHiddenPrefix)
	}
}

func TestStreamingMarkdownAppendChunkBoundsUnterminatedControls(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	opts := RenderOpts{ContentType: ContentTypeStreaming, Width: 80}

	w := renderer.AppendChunk(context.Background(), "before\x1b]52;clipboard;"+strings.Repeat("x", maxPendingTerminalControlBytes+128), opts)
	require.NotContains(t, w.Content, "\x1b", "incomplete OSC bytes must not be exposed")
	require.LessOrEqual(t, renderer.stats().pendingControlBytes, maxPendingTerminalControlBytes)
	require.True(t, renderer.stats().discardingControl, "oversized unterminated controls must enter discard-until-terminator mode")

	w = renderer.AppendChunk(context.Background(), "secret\aafter", opts)
	require.NotContains(t, w.Content, "secret", "discarded OSC payload must not leak as printable text")
	require.Contains(t, w.Content, "after")
	require.False(t, renderer.stats().discardingControl)
}

func TestStreamingMarkdownAppendChunkTracksFenceAcrossBoundaries(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	opts := RenderOpts{ContentType: ContentTypeStreaming, Width: 80}
	ctx := context.Background()

	renderer.AppendChunk(ctx, "before\n  `", opts)
	require.False(t, renderer.inCodeBlock)
	renderer.AppendChunk(ctx, "``go", opts)
	require.False(t, renderer.inCodeBlock, "an unfinished fence line must not change block state")
	renderer.AppendChunk(ctx, "\nfmt.Println(1)\n", opts)
	require.True(t, renderer.inCodeBlock)
	renderer.AppendChunk(ctx, "`", opts)
	require.True(t, renderer.inCodeBlock)
	renderer.AppendChunk(ctx, "``", opts)
	require.True(t, renderer.inCodeBlock)
	renderer.AppendChunk(ctx, "\n", opts)
	require.False(t, renderer.inCodeBlock)

	renderer.Finish(ctx, "before\n  ```go\nfmt.Println(1)\n```", opts)
	require.False(t, renderer.inCodeBlock)
}

func TestStreamingMarkdownAppendChunkKeepsGraphemesTogether(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	ctx := context.Background()
	opts := RenderOpts{ContentType: ContentTypeStreaming, Width: 2}

	renderer.AppendChunk(ctx, "e", opts)
	w := renderer.AppendChunk(ctx, "\u0301界", opts)
	w = renderer.AppendChunk(ctx, "👩‍💻", opts)
	require.True(t, utf8.ValidString(w.Content))
	require.Contains(t, w.Content, "e\u0301")
	require.Contains(t, w.Content, "界")
	require.Contains(t, w.Content, "👩‍💻")
	for _, line := range strings.Split(w.Content, "\n") {
		require.LessOrEqual(t, cellWidth(line), 2, "each fitting grapheme must wrap within the two-cell viewport")
	}
}

func TestStreamingMarkdownAppendChunkWrapsTwoWideGraphemes(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	w := renderer.AppendChunk(context.Background(), "界界", RenderOpts{Width: 2})

	require.Equal(t, "界\n界", w.Content)
	for _, line := range strings.Split(w.Content, "\n") {
		require.LessOrEqual(t, cellWidth(line), 2)
	}
}

func TestStreamingMarkdownAppendChunkBoundsPendingGraphemeBytes(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	w := renderer.AppendChunk(context.Background(), strings.Repeat("x", maxStreamingRenderBytes+1), RenderOpts{Width: maxStreamingRenderBytes * 2})

	require.LessOrEqual(t, len(w.Content), maxStreamingRenderBytes)
	require.True(t, w.HasHiddenPrefix, "text omitted to preserve the byte budget must be reported")
	require.True(t, utf8.ValidString(w.Content))
}

func TestStreamingMarkdownAppendChunkBoundsPendingWrappedRow(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	w := renderer.AppendChunk(context.Background(), strings.Repeat("x", maxStreamingRenderLines+10), RenderOpts{Width: 1})
	visibleRows := strings.Count(w.Content, "\n") + 1

	require.LessOrEqual(t, visibleRows, maxStreamingRenderLines)
	require.Equal(t, visibleRows, renderer.stats().renderedLines)
	require.True(t, w.HasHiddenPrefix, "evicting a row to retain the pending wrapped grapheme must be reported")
}

func TestStreamingMarkdownAppendChunkDoesNotRetainChunkBacking(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	chunk := strings.Repeat("x", 1<<20)
	renderer.AppendChunk(context.Background(), chunk, RenderOpts{Width: 80})

	for _, line := range renderer.windowLines {
		require.False(t, sharesStringBacking(line, chunk), "committed lines must not retain a large caller chunk")
	}
	for _, part := range renderer.windowLineParts {
		require.False(t, sharesStringBacking(part, chunk), "open line parts must not retain a large caller chunk")
	}
	require.False(t, sharesStringBacking(renderer.graphemePending, chunk), "pending graphemes must not retain a large caller chunk")
	require.LessOrEqual(t, len(renderer.Window().Content), maxStreamingRenderBytes)
}

func TestStreamingMarkdownReplaceWindowClonesRetainedRenderedLines(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	line := strings.Repeat("x", 512)
	rendered := strings.Repeat(line+"\n", 1024)
	renderer.replaceWindowWithText(rendered, false)

	for _, retained := range renderer.windowLines {
		require.False(t, sharesStringBacking(retained, rendered), "retained output lines must not pin the complete render allocation")
	}
	w := renderer.Window()
	require.LessOrEqual(t, len(w.Content), maxStreamingRenderBytes)
	require.LessOrEqual(t, renderer.stats().renderedLines, maxStreamingRenderLines)
	require.LessOrEqual(t, cap(renderer.windowLines), maxStreamingRenderLines*2, "line storage capacity must stay bounded by the window limit")
}

func TestStreamingMarkdownFinishSourceTailStartsAtGraphemeBoundary(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	ctx := context.Background()
	opts := RenderOpts{ContentType: ContentTypeMarkdown, Width: 80}
	source := "head" + "e\u0301" + strings.Repeat("x", maxStreamingRenderBytes-2)

	renderer.Finish(ctx, source, opts)
	tail := renderer.sourceTailString()
	require.False(t, strings.HasPrefix(tail, "\u0301"), "a retained source tail must not start with a detached combining mark")
	require.True(t, renderer.sourceTailHidden)
}

func TestStreamingMarkdownFinishIsIdempotent(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	ctx := context.Background()
	opts := RenderOpts{ContentType: ContentTypeMarkdown, Width: 80}
	source := "# once\n\ncanonical **text**"

	first := renderer.Finish(ctx, source, opts)
	second := renderer.Finish(ctx, source, opts)

	require.Equal(t, first, second)
	require.EqualValues(t, 1, renderer.stats().markdownRenderCalls)
	require.EqualValues(t, len(source), renderer.stats().markdownRenderBytes)
}

func TestStreamingMarkdownAppendChunkBoundsAnOversizedPendingGrapheme(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	w := renderer.AppendChunk(context.Background(), strings.Repeat("\u0301", maxPendingGraphemeBytes), RenderOpts{Width: 80})
	require.LessOrEqual(t, len(renderer.graphemePending), maxPendingGraphemeBytes)
	require.True(t, w.HasHiddenPrefix, "the presentation must report dropped display-cluster content")
	require.True(t, utf8.ValidString(w.Content))
	require.LessOrEqual(t, len(w.Content), maxStreamingRenderBytes)
}

func TestStreamingMarkdownAppendChunkKeepsBoundedWindowAndReset(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	opts := RenderOpts{ContentType: ContentTypeStreaming, Width: 24}

	for i := 0; i < maxStreamingRenderLines+40; i++ {
		renderer.AppendChunk(context.Background(), "line\n", opts)
	}
	w := renderer.Window()
	require.True(t, w.HasHiddenPrefix)
	require.LessOrEqual(t, renderer.stats().renderedLines, maxStreamingRenderLines)
	require.LessOrEqual(t, renderer.stats().renderedBytes, maxStreamingRenderBytes)
	require.Contains(t, w.Content, "line")
	resized := renderer.Resize(context.Background(), RenderOpts{ContentType: ContentTypeStreaming, Width: 12})
	require.True(t, resized.HasHiddenPrefix, "resize must not clear prefix-eviction metadata")

	renderer.Reset()
	w = renderer.Window()
	require.Empty(t, w.Content)
	require.False(t, w.HasHiddenPrefix)
	require.Zero(t, renderer.stats().renderedLines)
	require.Zero(t, renderer.stats().retainedSourceBytes)
}

func TestStreamingMarkdownResizeReflowsOnlyRetainedTail(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	ctx := context.Background()
	wide := RenderOpts{ContentType: ContentTypeStreaming, Width: 80}
	text := strings.Repeat("界e\u0301 👩‍💻 ", 40)
	renderer.AppendChunk(ctx, text, wide)
	before := renderer.Window()

	narrow := renderer.Resize(ctx, RenderOpts{ContentType: ContentTypeStreaming, Width: 12})
	require.NotEmpty(t, narrow.Content)
	require.Equal(t, before.HasHiddenPrefix, narrow.HasHiddenPrefix, "resize cannot claim a previously retained prefix disappeared")
	require.True(t, utf8.ValidString(narrow.Content))
	require.LessOrEqual(t, renderer.stats().renderedLines, maxStreamingRenderLines)
	require.LessOrEqual(t, renderer.stats().renderedBytes, maxStreamingRenderBytes)
	require.Contains(t, narrow.Content, "👩‍💻")

	wideAgain := renderer.Resize(ctx, wide)
	require.True(t, utf8.ValidString(wideAgain.Content))
	require.Contains(t, wideAgain.Content, "👩‍💻")
	continued := renderer.AppendChunk(ctx, "tail", wide)
	require.Contains(t, continued.Content, "tail")
}

func TestStreamingMarkdownFinishRendersCanonicalMarkdownOnce(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	ctx := context.Background()
	opts := RenderOpts{Theme: DarkTheme{}, ContentType: ContentTypeMarkdown, Width: 80}

	source := "# Heading\n\n> quoted **text**\n\n- first\n- second\n\n[reference][target]\n\n[target]: https://example.test/path\n\n```go\nfmt.Println(\"done\")\n```"
	for start := 0; start < len(source); {
		end := start + 7
		if end > len(source) {
			end = len(source)
		}
		renderer.AppendChunk(ctx, source[start:end], opts)
		start = end
	}

	w := renderer.Finish(ctx, source, opts)
	want := renderer.md.Render(ctx, source, opts)
	require.Equal(t, want, w.Content, "finish must preserve full-document Markdown semantics")
	require.False(t, w.HasHiddenPrefix)
	require.EqualValues(t, 1, renderer.stats().markdownRenderCalls)
	require.EqualValues(t, len(source), renderer.stats().markdownRenderBytes)
	require.False(t, renderer.inCodeBlock)
}

func TestStreamingMarkdownFinishBoundsRenderedCacheAndReportsHiddenPrefix(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	ctx := context.Background()
	opts := RenderOpts{ContentType: ContentTypeMarkdown, Width: 80}
	source := strings.Repeat("rendered row ", maxStreamingRenderBytes/len("rendered row ")+100)

	w := renderer.Finish(ctx, source, opts)
	require.True(t, w.HasHiddenPrefix)
	require.LessOrEqual(t, renderer.stats().renderedLines, maxStreamingRenderLines)
	require.LessOrEqual(t, renderer.stats().renderedBytes, maxStreamingRenderBytes)
	require.Contains(t, w.Content, "rendered row")
	require.EqualValues(t, 1, renderer.stats().markdownRenderCalls)
}

func TestStreamingMarkdownAppendCountersDoNotScaleWithAccumulatedPrefix(t *testing.T) {
	renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
	opts := RenderOpts{ContentType: ContentTypeStreaming, Width: 80}
	chunk := strings.Repeat("x", 1024)
	for i := 0; i < 1000; i++ {
		renderer.AppendChunk(context.Background(), chunk, opts)
	}

	stats := renderer.stats()
	require.EqualValues(t, 1024*1000, stats.inputBytes)
	require.EqualValues(t, 1024*1000, stats.parserBytes, "append parser work must count only newly supplied bytes")
	require.Zero(t, stats.markdownRenderCalls, "append preview must not repeatedly invoke whole-message Markdown render")
	require.LessOrEqual(t, stats.renderedLines, maxStreamingRenderLines)
	require.LessOrEqual(t, stats.renderedBytes, maxStreamingRenderBytes)
	require.LessOrEqual(t, stats.retainedSourceBytes, maxStreamingRenderBytes)
}

func TestStreamingMarkdownAppendChunkScalingCounters(t *testing.T) {
	const totalBytes = 1 << 20
	for _, chunks := range []int{1, 16, 256, 1000} {
		t.Run(fmt.Sprintf("chunks_%d", chunks), func(t *testing.T) {
			renderer := NewStreamingMarkdownRenderer(NewMarkdownRenderer())
			chunkSize := totalBytes / chunks
			remainder := totalBytes % chunks
			for i := range chunks {
				size := chunkSize
				if i < remainder {
					size++
				}
				renderer.AppendChunk(context.Background(), strings.Repeat("x", size), RenderOpts{ContentType: ContentTypeStreaming, Width: 120})
			}
			stats := renderer.stats()
			require.EqualValues(t, totalBytes, stats.inputBytes)
			require.EqualValues(t, totalBytes, stats.parserBytes)
			require.Zero(t, stats.markdownRenderCalls)
			require.LessOrEqual(t, stats.renderedBytes, maxStreamingRenderBytes)
			require.LessOrEqual(t, stats.retainedSourceBytes, maxStreamingRenderBytes)
		})
	}
}

func sharesStringBacking(text, backing string) bool {
	if text == "" || backing == "" {
		return false
	}
	textStart := uintptr(unsafe.Pointer(unsafe.StringData(text)))
	backingStart := uintptr(unsafe.Pointer(unsafe.StringData(backing)))
	return textStart >= backingStart && textStart < backingStart+uintptr(len(backing))
}
