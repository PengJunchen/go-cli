// Package llm middleware_sanitize.go - SanitizeModelMiddleware strips
// cross-model artifacts (Claude thinking tags, GPT watermark markers, Gemini
// thinking blocks) from model responses and normalizes excessive whitespace.
package llm

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

// SanitizeOption configures a SanitizeModelMiddleware.
type SanitizeOption func(*SanitizeModelMiddleware)

// WithSanitizeLogger sets the logger used for sanitize tracing. If logger is
// nil the option is a no-op.
func WithSanitizeLogger(logger *slog.Logger) SanitizeOption {
	return func(s *SanitizeModelMiddleware) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// SanitizeModelMiddleware cleans cross-model artifacts from the response
// content of the wrapped model.
type SanitizeModelMiddleware struct {
	logger *slog.Logger
}

var _ ModelMiddleware = (*SanitizeModelMiddleware)(nil)

// NewSanitizeModelMiddleware creates a SanitizeModelMiddleware with the given
// options. The default logger is slog.Default().
func NewSanitizeModelMiddleware(opts ...SanitizeOption) *SanitizeModelMiddleware {
	m := &SanitizeModelMiddleware{logger: slog.Default()}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name returns "sanitize".
func (s *SanitizeModelMiddleware) Name() string { return "sanitize" }

// WrapModel returns a BaseChatModel that sanitizes responses from next.
func (s *SanitizeModelMiddleware) WrapModel(next BaseChatModel) BaseChatModel {
	return &sanitizeModel{next: next, logger: s.logger}
}

// sanitizeModel is the wrapped BaseChatModel produced by
// SanitizeModelMiddleware.
type sanitizeModel struct {
	next   BaseChatModel
	logger *slog.Logger
}

var _ BaseChatModel = (*sanitizeModel)(nil)

// Precompiled patterns for the supported cross-model artifacts. The Claude and
// Gemini patterns use DotAll mode ((?s)) so their content may span newlines.
var (
	// sanitizeClaudeRe matches Claude <antThinking>...</antThinking> blocks.
	sanitizeClaudeRe = regexp.MustCompile(`(?s)<antThinking>.*?</antThinking>`)
	// sanitizeGPTRe matches GPT watermark markers such as 【6bc68d3a】.
	sanitizeGPTRe = regexp.MustCompile(`【.*?】`)
	// sanitizeGeminiRe matches Gemini ```thinking\n...\n``` code blocks. Built
	// from a double-quoted string because the pattern contains backticks.
	sanitizeGeminiRe = regexp.MustCompile("(?s)```thinking\n.*?```")
	// sanitizeMultiNewlineRe collapses three or more consecutive newlines into
	// a single blank line.
	sanitizeMultiNewlineRe = regexp.MustCompile(`\n{3,}`)
)

// sanitizeContent removes cross-model artifacts and collapses excessive blank
// lines from content. It does not trim leading/trailing whitespace so that
// per-chunk streaming content (where edge spaces may be significant) is
// preserved; callers that have a complete response may trim afterwards.
func sanitizeContent(content string) string {
	content = sanitizeClaudeRe.ReplaceAllString(content, "")
	content = sanitizeGPTRe.ReplaceAllString(content, "")
	content = sanitizeGeminiRe.ReplaceAllString(content, "")
	content = sanitizeMultiNewlineRe.ReplaceAllString(content, "\n\n")
	return content
}

// Generate sanitizes the response content returned by the next model. The
// complete response is also trimmed of surrounding whitespace.
func (m *sanitizeModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	resp, err := m.next.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	original := resp.Content
	resp.Content = strings.TrimSpace(sanitizeContent(original))
	if resp.Content != original {
		m.logger.Debug("sanitize.generate.modified",
			"before_len", len(original), "after_len", len(resp.Content))
	}
	return resp, nil
}

// Stream sanitizes each chunk's content as it is forwarded from the next
// model. Edge whitespace is intentionally preserved so that content split
// across chunks is not corrupted.
func (m *sanitizeModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	ch, err := m.next.Stream(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	out := make(chan MessageChunk)
	go func() {
		defer close(out)
		for chunk := range ch {
			original := chunk.Content
			chunk.Content = sanitizeContent(original)
			if chunk.Content != original {
				m.logger.Debug("sanitize.stream.modified",
					"before_len", len(original), "after_len", len(chunk.Content))
			}
			out <- chunk
		}
	}()
	return out, nil
}
