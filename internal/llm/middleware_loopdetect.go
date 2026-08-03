// Package llm middleware_loopdetect.go - LoopDetectionModelMiddleware detects
// when the wrapped model returns the same response repeatedly across
// consecutive calls and surfaces an error once a threshold is exceeded.
package llm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// LoopDetectOption configures a LoopDetectionModelMiddleware.
type LoopDetectOption func(*LoopDetectionModelMiddleware)

// WithLoopThreshold sets the number of consecutive identical responses that
// trigger a loop error.
func WithLoopThreshold(n int) LoopDetectOption {
	return func(l *LoopDetectionModelMiddleware) { l.threshold = n }
}

// WithLoopWindowSize sets how many recent responses are retained for
// comparison.
func WithLoopWindowSize(n int) LoopDetectOption {
	return func(l *LoopDetectionModelMiddleware) { l.windowSize = n }
}

// WithLoopDetectLogger sets the logger used for loop-detection tracing. If
// logger is nil the option is a no-op.
func WithLoopDetectLogger(logger *slog.Logger) LoopDetectOption {
	return func(l *LoopDetectionModelMiddleware) {
		if logger != nil {
			l.logger = logger
		}
	}
}

// LoopDetectionModelMiddleware tracks recent responses from the wrapped model
// and returns an error when the same response appears threshold times in a row.
// The recent-history window and mutex live on the middleware so that the
// wrapped model shares a single history.
type LoopDetectionModelMiddleware struct {
	threshold  int
	windowSize int
	logger     *slog.Logger
	mu         sync.Mutex
	recent     []string
}

var _ ModelMiddleware = (*LoopDetectionModelMiddleware)(nil)

// NewLoopDetectionModelMiddleware creates a LoopDetectionModelMiddleware with
// the given options. Defaults: threshold=3, windowSize=5.
func NewLoopDetectionModelMiddleware(opts ...LoopDetectOption) *LoopDetectionModelMiddleware {
	m := &LoopDetectionModelMiddleware{
		threshold:  3,
		windowSize: 5,
		logger:     slog.Default(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name returns "loopdetection".
func (l *LoopDetectionModelMiddleware) Name() string { return "loopdetection" }

// WrapModel returns a BaseChatModel that tracks responses from next for loops.
// The wrapped model shares the middleware's history and mutex.
func (l *LoopDetectionModelMiddleware) WrapModel(next BaseChatModel) BaseChatModel {
	return &loopDetectModel{
		next:       next,
		threshold:  l.threshold,
		windowSize: l.windowSize,
		logger:     l.logger,
		mu:         &l.mu,
		recent:     &l.recent,
	}
}

// loopDetectModel is the wrapped BaseChatModel produced by
// LoopDetectionModelMiddleware.
type loopDetectModel struct {
	next       BaseChatModel
	threshold  int
	windowSize int
	logger     *slog.Logger
	mu         *sync.Mutex
	recent     *[]string
}

var _ BaseChatModel = (*loopDetectModel)(nil)

// checkLoop records the response content and returns an error if it has now
// appeared threshold times in a row. It is safe for concurrent use.
func (m *loopDetectModel) checkLoop(content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	*m.recent = append(*m.recent, content)
	if len(*m.recent) > m.windowSize {
		*m.recent = (*m.recent)[len(*m.recent)-m.windowSize:]
	}

	// Count the trailing run of responses identical to the latest one.
	count := 1
	for i := len(*m.recent) - 2; i >= 0; i-- {
		if (*m.recent)[i] == content {
			count++
		} else {
			break
		}
	}

	if count >= m.threshold {
		m.logger.Warn("loopdetection.detected",
			"count", count, "threshold", m.threshold)
		return fmt.Errorf("loopdetection: detected repeated response (count=%d)", count)
	}
	return nil
}

// Generate forwards the call and then checks the response content for a loop.
// When a loop is detected the response is discarded and the error is returned.
func (m *loopDetectModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	resp, err := m.next.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	if err := m.checkLoop(resp.Content); err != nil {
		return nil, err
	}
	return resp, nil
}

// Stream forwards the stream while accumulating chunk content. Once the stream
// completes, the accumulated content is checked for a loop; if a loop is
// detected the buffered chunks are dropped and the error is returned.
func (m *loopDetectModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	ch, err := m.next.Stream(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	var chunks []MessageChunk
	var acc strings.Builder
	for chunk := range ch {
		chunks = append(chunks, chunk)
		acc.WriteString(chunk.Content)
	}
	if err := m.checkLoop(acc.String()); err != nil {
		return nil, err
	}
	out := make(chan MessageChunk, len(chunks))
	for _, c := range chunks {
		out <- c
	}
	close(out)
	return out, nil
}
