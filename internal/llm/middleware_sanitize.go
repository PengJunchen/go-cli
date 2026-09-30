// Package llm middleware_sanitize.go - SanitizeModelMiddleware strips
// cross-model artifacts (Claude thinking tags, GPT watermark markers, Gemini
// thinking blocks) from model responses and normalizes excessive whitespace.
package llm

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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

// sanitizeContent removes supported cross-model blocks. An exact opening
// marker without a closing marker suppresses the remainder of the content;
// an incomplete marker prefix at EOF is discarded while ordinary text is
// preserved. It does not trim surrounding whitespace.
func sanitizeContent(content string) string {
	var sanitizer streamContentSanitizer
	return sanitizer.sanitize(content)
}

type streamSanitizeMarker struct {
	open  []byte
	close []byte
}

var streamSanitizeMarkers = [...]streamSanitizeMarker{
	{open: []byte("<antThinking>"), close: []byte("</antThinking>")},
	{open: []byte("【"), close: []byte("】")},
	{open: []byte("```thinking\n"), close: []byte("```")},
}

// streamContentSanitizer keeps only a possible marker prefix between chunks.
// Sensitive block bodies are discarded as they arrive, so carry memory is
// bounded by the longest marker rather than by block or response size.
type streamContentSanitizer struct {
	block      *streamSanitizeMarker
	pending    []byte
	closing    []byte
	newlineRun int
}

func (s *streamContentSanitizer) sanitize(content string) string {
	var output strings.Builder
	for i := 0; i < len(content); i++ {
		value := content[i]
		if s.block != nil {
			s.consumeSensitive(value)
			continue
		}

		s.pending = append(s.pending, value)
		for len(s.pending) > 0 {
			var matched *streamSanitizeMarker
			possiblePrefix := false
			for i := range streamSanitizeMarkers {
				marker := &streamSanitizeMarkers[i]
				if bytes.Equal(s.pending, marker.open) {
					matched = marker
					break
				}
				if len(s.pending) < len(marker.open) && bytes.Equal(s.pending, marker.open[:len(s.pending)]) {
					possiblePrefix = true
				}
			}
			if matched != nil {
				s.block = matched
				s.pending = s.pending[:0]
				s.closing = s.closing[:0]
				break
			}
			if possiblePrefix {
				break
			}

			s.writeVisible(s.pending[0], &output)
			copy(s.pending, s.pending[1:])
			s.pending = s.pending[:len(s.pending)-1]
		}
	}
	return output.String()
}

func (s *streamContentSanitizer) consumeSensitive(value byte) {
	s.closing = append(s.closing, value)
	if bytes.Equal(s.closing, s.block.close) {
		s.block = nil
		s.closing = s.closing[:0]
		return
	}

	maxPrefix := len(s.block.close) - 1
	if len(s.closing) < maxPrefix {
		maxPrefix = len(s.closing)
	}
	keep := 0
	for size := maxPrefix; size > 0; size-- {
		if bytes.Equal(s.closing[len(s.closing)-size:], s.block.close[:size]) {
			keep = size
			break
		}
	}
	if keep > 0 {
		copy(s.closing[:keep], s.closing[len(s.closing)-keep:])
	}
	s.closing = s.closing[:keep]
}

func (s *streamContentSanitizer) writeVisible(value byte, output *strings.Builder) {
	if value == '\n' {
		if s.newlineRun < 2 {
			output.WriteByte(value)
			s.newlineRun++
		}
		return
	}
	s.newlineRun = 0
	output.WriteByte(value)
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

// Stream sanitizes content across chunk boundaries. Each source chunk still
// produces one output chunk with its metadata intact; only Content is rewritten.
func (m *sanitizeModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	streamCtx, cancel := context.WithCancelCause(ctx)
	ch, err := m.next.Stream(streamCtx, msgs, opts...)
	if err != nil {
		cancelStreamAttempt(ctx, cancel, err)
		drainMessageChunks(ch)
		return nil, err
	}
	if ch == nil {
		err = errors.New("sanitize: model returned a nil stream")
		cancelStreamAttempt(ctx, cancel, err)
		return nil, err
	}
	out := make(chan MessageChunk, 1)
	go func() {
		defer close(out)
		var sanitizer streamContentSanitizer
		abort := func(cause error) {
			cancelStreamAttempt(ctx, cancel, cause)
			drainMessageChunks(ch)
		}
		for {
			select {
			case <-streamCtx.Done():
				abort(context.Cause(streamCtx))
				return
			case chunk, ok := <-ch:
				if !ok {
					cancel(nil)
					return
				}
				original := chunk.Content
				chunk.Content = sanitizer.sanitize(original)
				if chunk.Content != original {
					m.logger.Debug("sanitize.stream.modified",
						"before_len", len(original), "after_len", len(chunk.Content))
				}
				if chunk.Error != nil {
					select {
					case out <- chunk:
						abort(chunk.Error)
					case <-streamCtx.Done():
						abort(context.Cause(streamCtx))
					}
					return
				}
				select {
				case out <- chunk:
				case <-streamCtx.Done():
					abort(context.Cause(streamCtx))
					return
				}
			}
		}
	}()
	return out, nil
}
