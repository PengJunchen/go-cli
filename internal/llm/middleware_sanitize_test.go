package llm

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sanitizeMockModel returns a fixed Generate response and a configurable
// sequence of Stream chunks. Used by sanitize middleware tests.
type sanitizeMockModel struct {
	genContent   string
	streamChunks []MessageChunk
}

func (m *sanitizeMockModel) Generate(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
	return &Message{Role: RoleAssistant, Content: m.genContent}, nil
}

func (m *sanitizeMockModel) Stream(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk, len(m.streamChunks))
	for _, c := range m.streamChunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

var _ BaseChatModel = (*sanitizeMockModel)(nil)

// TestSanitizeModelMiddleware_Name verifies the middleware identifier.
func TestSanitizeModelMiddleware_Name(t *testing.T) {
	mw := NewSanitizeModelMiddleware()
	assert.Equal(t, "sanitize", mw.Name())
}

// TestSanitizeModelMiddleware_ClaudeArtifactRemoved verifies that Claude
// <antThinking>...</antThinking> blocks are stripped from the response.
func TestSanitizeModelMiddleware_ClaudeArtifactRemoved(t *testing.T) {
	base := &sanitizeMockModel{genContent: "<antThinking>secret thoughts</antThinking>Hello world"}
	mw := NewSanitizeModelMiddleware()
	wrapped := mw.WrapModel(base)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello world", resp.Content)
}

// TestSanitizeModelMiddleware_PreservesBracketText verifies bracket-shaped
// prose is not treated as hidden reasoning without a typed provider fixture.
func TestSanitizeModelMiddleware_PreservesBracketText(t *testing.T) {
	cases := []string{
		"Hello【6bc68d3a】 world",
		"中文【重要内容尚未闭合",
		"日本語【未完了の見出し",
		"Code: `【6bc68d3a】` and quoted “【internal】” text.",
	}
	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			base := &sanitizeMockModel{genContent: text}
			wrapped := NewSanitizeModelMiddleware().WrapModel(base)

			resp, err := wrapped.Generate(context.Background(), nil)
			require.NoError(t, err)
			assert.Equal(t, text, resp.Content)
		})
	}
}

// TestSanitizeModelMiddleware_GeminiBlockRemoved verifies that Gemini
// ```thinking ... ``` blocks are stripped from the response.
func TestSanitizeModelMiddleware_GeminiBlockRemoved(t *testing.T) {
	base := &sanitizeMockModel{genContent: "Final answer```thinking\ninternal reasoning\n```"}
	mw := NewSanitizeModelMiddleware()
	wrapped := mw.WrapModel(base)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "Final answer", resp.Content)
}

// TestSanitizeModelMiddleware_CleanContentUnchanged verifies that content
// without any artifacts is returned verbatim.
func TestSanitizeModelMiddleware_CleanContentUnchanged(t *testing.T) {
	base := &sanitizeMockModel{genContent: "The quick brown fox jumps over the lazy dog."}
	mw := NewSanitizeModelMiddleware()
	wrapped := mw.WrapModel(base)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "The quick brown fox jumps over the lazy dog.", resp.Content)
}

func TestSanitizeModelMiddleware_GenerateFailsClosedForUnclosedSensitiveBlocks(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{name: "claude", text: "answer<antThinking>CLAUDE-GENERATE-SECRET"},
		{name: "gemini", text: "answer```thinking\nGEMINI-GENERATE-SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := &sanitizeMockModel{genContent: tc.text}
			wrapped := NewSanitizeModelMiddleware().WrapModel(base)
			resp, err := wrapped.Generate(context.Background(), nil)
			require.NoError(t, err)
			assert.Equal(t, "answer", resp.Content)
			assert.NotContains(t, resp.Content, "SECRET")
		})
	}
}

func TestSanitizeModelMiddleware_GeneratePreservesOrdinaryTextNearMarkerPrefixes(t *testing.T) {
	const text = "Documentation: <antThinkingX>, ```thinking example, 【section, and literal braces remain visible."
	base := &sanitizeMockModel{genContent: text}
	wrapped := NewSanitizeModelMiddleware().WrapModel(base)
	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, text, resp.Content)
}

// TestSanitizeModelMiddleware_StreamSanitized verifies that artifacts are
// stripped from each streamed chunk while inter-chunk spacing is preserved.
func TestSanitizeModelMiddleware_StreamSanitized(t *testing.T) {
	base := &sanitizeMockModel{streamChunks: []MessageChunk{
		{Role: RoleAssistant, Content: "<antThinking>x</antThinking>Hello ", Final: false},
		{Role: RoleAssistant, Content: "World", Final: true},
	}}
	mw := NewSanitizeModelMiddleware()
	wrapped := mw.WrapModel(base)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	var got string
	for chunk := range ch {
		got += chunk.Content
	}
	assert.Equal(t, "Hello World", got)
}

func TestSanitizeModelMiddleware_StreamRedactsMarkersAcrossEverySplit(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		text   string
		want   string
	}{
		{
			name:   "claude",
			secret: "CLAUDE-SECRET",
			text:   "before <antThinking>CLAUDE-SECRET\nprivate</antThinking> after",
			want:   "before  after",
		},
		{
			name:   "gemini",
			secret: "GEMINI-SECRET",
			text:   "before```thinking\nGEMINI-SECRET\nprivate\n``` after",
			want:   "before after",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for split := 0; split <= len(tc.text); split++ {
				t.Run(fmt.Sprintf("split-%d", split), func(t *testing.T) {
					base := &sanitizeMockModel{streamChunks: []MessageChunk{
						{Role: RoleAssistant, Content: tc.text[:split]},
						{Role: RoleAssistant, Content: tc.text[split:], Final: true},
					}}
					wrapped := NewSanitizeModelMiddleware().WrapModel(base)
					ch, err := wrapped.Stream(context.Background(), nil)
					require.NoError(t, err)

					var got strings.Builder
					for chunk := range ch {
						got.WriteString(chunk.Content)
					}
					assert.NotContains(t, got.String(), tc.secret)
					assert.Equal(t, tc.want, got.String())
				})
			}
		})
	}
}

func TestSanitizeModelMiddleware_StreamFailsClosedForUnclosedSensitiveBlocks(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{name: "claude", text: "answer<antThinking>CLAUDE-EOF-SECRET"},
		{name: "gemini", text: "answer```thinking\nGEMINI-EOF-SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := &sanitizeMockModel{streamChunks: []MessageChunk{{Content: tc.text}}}
			wrapped := NewSanitizeModelMiddleware().WrapModel(base)
			ch, err := wrapped.Stream(context.Background(), nil)
			require.NoError(t, err)

			var got strings.Builder
			for chunk := range ch {
				got.WriteString(chunk.Content)
			}
			assert.Equal(t, "answer", got.String())
			assert.NotContains(t, got.String(), "SECRET")
		})
	}
}

func TestSanitizeModelMiddleware_StreamDropsIncompleteOpeningMarkerAtEOF(t *testing.T) {
	base := &sanitizeMockModel{streamChunks: []MessageChunk{{Content: "answer <antThink"}}}
	wrapped := NewSanitizeModelMiddleware().WrapModel(base)
	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var got strings.Builder
	for chunk := range ch {
		got.WriteString(chunk.Content)
	}
	assert.Equal(t, "answer ", got.String())
}

func TestSanitizeModelMiddleware_StreamCollapsesNewlinesAcrossChunksAndKeepsWhitespace(t *testing.T) {
	base := &sanitizeMockModel{streamChunks: []MessageChunk{
		{Role: RoleAssistant, Content: "  leading\n"},
		{Role: RoleAssistant, Content: "\n\ntrailing  ", Final: true},
	}}
	wrapped := NewSanitizeModelMiddleware().WrapModel(base)
	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var got strings.Builder
	for chunk := range ch {
		got.WriteString(chunk.Content)
	}
	assert.Equal(t, "  leading\n\ntrailing  ", got.String())
}

func TestSanitizeModelMiddleware_StreamPreservesChunkMetadataAndBackpressure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	secondAccepted := make(chan struct{})
	thirdStarted := make(chan struct{})
	thirdAccepted := make(chan struct{})
	upstream := make(chan MessageChunk)
	base := &blockingSanitizeModel{stream: func() <-chan MessageChunk { return upstream }}
	wrapped := NewSanitizeModelMiddleware().WrapModel(base)
	out, err := wrapped.Stream(ctx, nil)
	require.NoError(t, err)

	go func() {
		upstream <- MessageChunk{Role: RoleAssistant, Content: "visible", Final: false}
		upstream <- MessageChunk{Role: RoleAssistant, Content: "later<antThinking>private</antThinking>", Final: false}
		close(secondAccepted)
		close(thirdStarted)
		upstream <- MessageChunk{Role: RoleAssistant, Content: "last", Final: true, FinishReason: "stop", Usage: &Usage{TotalTokens: 2}}
		close(thirdAccepted)
		close(upstream)
	}()
	waitForSanitizeSignal(t, secondAccepted, "second source chunk was consumed")
	waitForSanitizeSignal(t, thirdStarted, "third source send started")
	assert.Equal(t, 1, cap(out), "the sanitizer output queue must remain bounded to one chunk")
	select {
	case <-thirdAccepted:
		t.Fatal("sanitizer consumed the third source chunk before downstream drained its bounded output queue")
	case <-time.After(time.Second):
	}

	first := <-out
	assert.Equal(t, "visible", first.Content)
	assert.False(t, first.Final)
	second := <-out
	assert.Equal(t, "later", second.Content)
	assert.False(t, second.Final)
	third := <-out
	assert.Equal(t, "last", third.Content)
	assert.True(t, third.Final)
	assert.Equal(t, "stop", third.FinishReason)
	require.NotNil(t, third.Usage)
	assert.Equal(t, 2, third.Usage.TotalTokens)
	waitForSanitizeSignal(t, thirdAccepted, "third source chunk was consumed after downstream progress")
	_, ok := <-out
	assert.False(t, ok)
}

func TestSanitizeModelMiddleware_StreamCancellationReachesUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	upstreamCanceled := make(chan struct{})
	base := &cancelAwareSanitizeModel{canceled: upstreamCanceled}
	wrapped := NewSanitizeModelMiddleware().WrapModel(base)
	out, err := wrapped.Stream(ctx, nil)
	require.NoError(t, err)
	cancel()

	for range out {
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("sanitizer did not cancel the upstream stream")
	}
}

func TestSanitizeModelMiddleware_StreamPreservesBracketTextAcrossChunks(t *testing.T) {
	text := []string{
		"中文【重要内容",
		"尚未闭合、日本語【未完了の見出し",
		" and code `【6bc68d3a】` with quote “【internal】”.",
	}
	base := &sanitizeMockModel{streamChunks: []MessageChunk{
		{Role: RoleAssistant, Content: text[0]},
		{Role: RoleAssistant, Content: text[1]},
		{Role: RoleAssistant, Content: text[2], Final: true},
	}}
	wrapped := NewSanitizeModelMiddleware().WrapModel(base)
	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var got strings.Builder
	for chunk := range ch {
		got.WriteString(chunk.Content)
	}
	assert.Equal(t, strings.Join(text, ""), got.String())
}

func TestSanitizeModelMiddleware_StreamPreservesFinalErrorCauseWithoutLeakingContent(t *testing.T) {
	stopErr := errors.New("provider stream stopped")
	base := &sanitizeMockModel{streamChunks: []MessageChunk{{
		Role:    RoleAssistant,
		Content: "visible<antThinking>CLAUDE-ERROR-SECRET",
		Error:   stopErr,
	}}}
	wrapped := NewSanitizeModelMiddleware().WrapModel(base)
	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var got strings.Builder
	var gotErr error
	for chunk := range ch {
		got.WriteString(chunk.Content)
		gotErr = chunk.Error
	}
	assert.Equal(t, "visible", got.String())
	require.ErrorIs(t, gotErr, stopErr)
	assert.NotContains(t, got.String(), "SECRET")
}

func TestStreamContentSanitizerRetainsOnlyBoundedMarkerCarry(t *testing.T) {
	const secretSize = 1 << 20
	secret := strings.Repeat("private", secretSize/len("private"))
	var sanitizer streamContentSanitizer
	got := sanitizer.sanitize("visible<antThinking>" + secret)
	assert.Equal(t, "visible", got)
	assert.Same(t, &streamSanitizeMarkers[0], sanitizer.block)
	assert.LessOrEqual(t, len(sanitizer.pending), len(streamSanitizeMarkers[1].open))
	assert.LessOrEqual(t, len(sanitizer.closing), len(streamSanitizeMarkers[0].close)-1)

	got = sanitizer.sanitize("</antThinking>after")
	assert.Equal(t, "after", got)
	assert.Nil(t, sanitizer.block)
}

func TestStreamContentSanitizerDoesNotAllocateForLargeRedactedBody(t *testing.T) {
	const bodySize = 4 << 20
	content := "<antThinking>" + strings.Repeat("x", bodySize) + "</antThinking>"
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var sanitizer streamContentSanitizer
	got := sanitizer.sanitize(content)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	assert.Empty(t, got)
	allocated := after.TotalAlloc - before.TotalAlloc
	assert.Less(t, allocated, uint64(bodySize/8), "fully hidden content must not allocate an output buffer proportional to its input")
}

type blockingSanitizeModel struct {
	stream func() <-chan MessageChunk
}

func waitForSanitizeSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func (*blockingSanitizeModel) Generate(context.Context, []Message, ...Option) (*Message, error) {
	return nil, fmt.Errorf("unexpected Generate call")
}

func (m *blockingSanitizeModel) Stream(context.Context, []Message, ...Option) (<-chan MessageChunk, error) {
	return m.stream(), nil
}

type cancelAwareSanitizeModel struct {
	canceled chan struct{}
}

func (*cancelAwareSanitizeModel) Generate(context.Context, []Message, ...Option) (*Message, error) {
	return nil, fmt.Errorf("unexpected Generate call")
}

func (m *cancelAwareSanitizeModel) Stream(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk)
	go func() {
		defer close(ch)
		<-ctx.Done()
		close(m.canceled)
	}()
	return ch, nil
}
