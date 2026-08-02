package tui

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allRenderers enumerates exactly the 24 required renderer types.
var allRenderers = func() []Renderer {
	return []Renderer{
		MarkdownRenderer{}, CodeRenderer{}, TableRenderer{}, DiffRenderer{},
		ErrorRenderer{}, ToolCallRenderer{}, ToolResultRenderer{}, ThinkingRenderer{},
		ProgressRenderer{}, FileTreeRenderer{}, ImageRenderer{}, LinkRenderer{},
		SystemRenderer{}, UserRenderer{}, AssistantRenderer{}, ApprovalRenderer{},
		PromptRenderer{}, CompactionRenderer{}, StreamingRenderer{}, StreamingCodeRenderer{},
		StreamingThinkingRenderer{}, BlankRenderer{}, SeparatorRenderer{}, StatusRenderer{},
	}
}()

func TestTwentyFourRenderers(t *testing.T) {
	require.Len(t, allRenderers, 24)
	assert.Len(t, contentTypes, 24)
}

func TestDefaultRegistryRegistersAll(t *testing.T) {
	reg := NewDefaultRegistry()
	require.Len(t, reg.List(), 24)
	for _, ct := range contentTypes {
		r, ok := reg.Get(ct)
		require.True(t, ok, "content type %q should be registered", ct)
		require.NotNil(t, r)
	}
}

func TestRegistryLookupByContentType(t *testing.T) {
	reg := NewRendererRegistry()
	mr := NewMockRenderer("mock", ContentTypeCode, "fixed")
	reg.Register(mr)

	r, ok := reg.Get(ContentTypeCode)
	require.True(t, ok)
	assert.Equal(t, mr, r)

	_, ok = reg.Get(ContentTypeMarkdown)
	assert.False(t, ok)
}

func TestRegistryGetMissing(t *testing.T) {
	reg := NewDefaultRegistry()
	_, ok := reg.Get("definitely_missing")
	assert.False(t, ok)
}

func TestRendererSupportsOwnType(t *testing.T) {
	for i, ct := range contentTypes {
		r, ok := NewDefaultRegistry().Get(ct)
		require.True(t, ok, "missing type %q at index %d", ct, i)
		assert.True(t, r.Supports(ct), "%s should support %s", r.Name(), ct)
	}
}

func TestEveryRendererProducesOutput(t *testing.T) {
	reg := NewDefaultRegistry()
	ctx := context.Background()
	opts := RenderOpts{Theme: DarkTheme{}, Width: 60}

	for _, ct := range contentTypes {
		r, _ := reg.Get(ct)
		out := r.Render(ctx, "sample", opts)
		if ct == ContentTypeBlank {
			// Blank renderers intentionally produce a blank line.
			assert.Empty(t, out, "blank renderer must emit empty output")
			continue
		}
		assert.NotEmpty(t, out, "renderer for %q must produce output", ct)
	}
}

func TestCodeRendererHonorsLanguage(t *testing.T) {
	r := CodeRenderer{}
	ctx := context.Background()
	out := r.Render(ctx, "package main", RenderOpts{Theme: DarkTheme{}, Language: "go"})
	assert.Contains(t, out, "package main")
}

func TestDiffRendererColorizes(t *testing.T) {
	d := DiffRenderer{}
	out := d.Render(context.Background(), "+added\n-removed\nctx", RenderOpts{Theme: DarkTheme{}})
	assert.Contains(t, out, "\x1b[32m") // green for additions
	assert.Contains(t, out, "\x1b[31m") // red for removals
}

func TestProgressRendererWidth(t *testing.T) {
	p := ProgressRenderer{}
	// 0.5 on width 10 yields 5 filled cells.
	out := p.Render(context.Background(), "0.5", RenderOpts{Theme: DarkTheme{}, Width: 10})
	assert.Contains(t, out, strings.Repeat("=", 5))
	assert.Contains(t, out, strings.Repeat("-", 5))
}

func TestSeparatorRendererSpan(t *testing.T) {
	s := SeparatorRenderer{}
	out := s.Render(context.Background(), "", RenderOpts{Width: 12})
	assert.Equal(t, 12, utf8.RuneCountInString(out))
}

func TestStatusFallbackIsRegistered(t *testing.T) {
	reg := NewDefaultRegistry()
	r, ok := reg.Get(DefaultContentType)
	require.True(t, ok)
	assert.Equal(t, ContentTypeStatus, DefaultContentType)
	assert.True(t, r.Supports(ContentTypeStatus))
}

func TestStreamingRenderersImplementMarker(t *testing.T) {
	for _, r := range []Renderer{StreamingRenderer{}, StreamingCodeRenderer{}, StreamingThinkingRenderer{}} {
		sm, ok := r.(streamMarker)
		require.True(t, ok, "%T should be a stream marker", r)
		assert.True(t, sm.streaming())
	}
	// Non-streaming renderers are not markers.
	_, ok := Renderer(MarkdownRenderer{}).(streamMarker)
	assert.False(t, ok)
}
