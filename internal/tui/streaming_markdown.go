package tui

import (
	"context"
	"strings"
)

// StreamingMarkdownRenderer incrementally renders streaming markdown by caching
// the already-rendered stable lines and re-rendering only the trailing
// "unstable" lines on each call. This eliminates the O(n²) full re-parse and
// re-render that would otherwise occur when every streaming token chunk triggers
// a fresh glamour.Render of the entire accumulated buffer.
//
// The accumulated content is split into lines. The last unstableCount lines are
// treated as unstable (the streaming cursor may still be mutating them) and
// re-rendered every call. Everything above them is stable: it is rendered once
// and cached until the stable prefix grows (a new line crosses into stable
// territory) or the buffer is reset.
type StreamingMarkdownRenderer struct {
	md              *MarkdownRenderer
	stableRendered  string // cached rendered output for the stable prefix
	stableCount     int    // number of leading lines considered stable (cached)
	unstableCount   int    // trailing lines re-rendered on every call
	lastAccumulated string // previous accumulated input (for pure-append detection)

	codeBlockBuffer strings.Builder // buffers content inside an incomplete code block
	inCodeBlock     bool            // true when the accumulated content has an unclosed ```
}

// NewStreamingMarkdownRenderer returns a StreamingMarkdownRenderer that wraps
// md and treats the last 2 lines as unstable.
func NewStreamingMarkdownRenderer(md *MarkdownRenderer) *StreamingMarkdownRenderer {
	return &StreamingMarkdownRenderer{md: md, unstableCount: 2}
}

// RenderIncremental renders accumulated markdown, caching the stable prefix.
// It returns the combined rendered output (stable prefix + unstable tail).
//
// Cache rules:
//   - If accumulated has at most unstableCount lines, the whole buffer is
//     rendered directly (nothing is worth caching yet) and the cache resets.
//   - If the accumulated buffer did not grow by pure append since the last call
//     (e.g. a new message replaced the buffer), the cache is invalidated and the
//     new stable prefix is re-rendered.
//   - If the stable line count grew (a previously-unstable line became stable),
//     the enlarged stable prefix is re-rendered and re-cached.
//   - Otherwise (pure append within the existing stable region) the cached
//     stable output is reused and only the unstable tail is re-rendered.
//
// The KEY invariant: the whole accumulated buffer is never re-rendered on every
// token; only the unstable tail is, plus an occasional re-render of the stable
// prefix when it actually grows.
func (s *StreamingMarkdownRenderer) RenderIncremental(ctx context.Context, accumulated string, opts RenderOpts) string {
	lines := strings.Split(accumulated, "\n")

	// Too few lines to partition: render everything and reset the cache so the
	// next call starts fresh once stable lines appear.
	if len(lines) <= s.unstableCount {
		s.invalidate(accumulated)
		return s.md.Render(ctx, accumulated, opts)
	}

	stableCount := len(lines) - s.unstableCount

	// When an incomplete code block is detected (odd number of ``` markers),
	// ensure the opening fence and everything after it stays in the unstable
	// region. This prevents the renderer from caching a partial code block in
	// the stable prefix, which would produce broken output until the closing
	// fence arrives. The entire code block is re-rendered on each call until it
	// completes.
	if s.hasIncompleteCodeBlock(accumulated) {
		s.inCodeBlock = true
		// Find the last ``` fence — this is the opening of the incomplete block.
		lastFenceIdx := -1
		for i, line := range lines {
			if strings.Contains(line, "```") {
				lastFenceIdx = i
			}
		}
		// Move the stable boundary back to before the fence so the entire
		// incomplete code block stays in the unstable region.
		if lastFenceIdx >= 0 && lastFenceIdx < stableCount {
			stableCount = lastFenceIdx
		}
		// Track the buffered code-block content for diagnostics / future use.
		s.codeBlockBuffer.Reset()
		s.codeBlockBuffer.WriteString(strings.Join(lines[stableCount:], "\n"))
	} else {
		s.inCodeBlock = false
		s.codeBlockBuffer.Reset()
	}

	// Invalidate when the stable region grew (more lines became stable) or when
	// the buffer is not a pure append of the previous one (replacement/reset).
	if stableCount != s.stableCount || !strings.HasPrefix(accumulated, s.lastAccumulated) {
		stablePart := strings.Join(lines[:stableCount], "\n")
		s.stableRendered = s.md.Render(ctx, stablePart, opts)
		s.stableCount = stableCount
	}

	s.lastAccumulated = accumulated

	// Always re-render the unstable tail.
	unstablePart := strings.Join(lines[stableCount:], "\n")
	unstableRendered := s.md.Render(ctx, unstablePart, opts)

	if s.stableRendered == "" {
		return unstableRendered
	}
	return s.stableRendered + "\n" + unstableRendered
}

// hasIncompleteCodeBlock reports whether content contains an odd number of ```
// fence markers, indicating that a code block has been opened but not yet
// closed.
func (s *StreamingMarkdownRenderer) hasIncompleteCodeBlock(content string) bool {
	return strings.Count(content, "```")%2 != 0
}

// invalidate clears the stable cache and records the current accumulated input
// as the new append-detection baseline.
func (s *StreamingMarkdownRenderer) invalidate(accumulated string) {
	s.stableRendered = ""
	s.stableCount = 0
	s.lastAccumulated = accumulated
}

// Reset clears all cached state. Call it when a new streaming message starts so
// stale rendered lines from the previous message are not reused.
func (s *StreamingMarkdownRenderer) Reset() {
	s.stableRendered = ""
	s.stableCount = 0
	s.lastAccumulated = ""
	s.inCodeBlock = false
	s.codeBlockBuffer.Reset()
}
