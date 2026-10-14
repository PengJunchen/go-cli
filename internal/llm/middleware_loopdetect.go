// Package llm middleware_loopdetect.go - LoopDetectionModelMiddleware detects
// when the wrapped model returns the same response repeatedly across
// consecutive calls and surfaces an error once a threshold is exceeded.
package llm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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

// responseFingerprint returns a bounded fingerprint of the assistant's
// user-visible text and ordered tool actions. Provider-generated tool IDs are
// excluded because they vary between otherwise identical responses.
func responseFingerprint(content string, toolCalls []ToolCall) (string, error) {
	if len(toolCalls) > GuardedStreamMaxToolCalls {
		return "", fmt.Errorf("loopdetection: response exceeds %d tool calls", GuardedStreamMaxToolCalls)
	}

	h := sha256.New()
	totalBytes := 0
	writeField := func(value []byte) error {
		next, ok := addBoundedStreamBytes(totalBytes, len(value))
		if !ok {
			return fmt.Errorf("loopdetection: response exceeds %d-byte candidate limit", GuardedStreamMaxBytes)
		}
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = h.Write(length[:])
		_, _ = h.Write(value)
		totalBytes = next
		return nil
	}

	if err := writeField([]byte(content)); err != nil {
		return "", err
	}
	var callCount [8]byte
	binary.BigEndian.PutUint64(callCount[:], uint64(len(toolCalls)))
	var countLength [8]byte
	binary.BigEndian.PutUint64(countLength[:], uint64(len(callCount)))
	_, _ = h.Write(countLength[:])
	_, _ = h.Write(callCount[:])
	for _, call := range toolCalls {
		if err := writeField([]byte(call.Name)); err != nil {
			return "", err
		}
		args, err := json.Marshal(call.Args)
		if err != nil {
			return "", fmt.Errorf("loopdetection: encode tool arguments for fingerprint: %w", err)
		}
		if err := writeField(args); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkLoop records the response fingerprint and returns an error if it has
// now appeared threshold times in a row. It is safe for concurrent use.
func (m *loopDetectModel) checkLoop(fingerprint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	*m.recent = append(*m.recent, fingerprint)
	if len(*m.recent) > m.windowSize {
		*m.recent = (*m.recent)[len(*m.recent)-m.windowSize:]
	}

	// Count the trailing run of responses identical to the latest one.
	count := 1
	for i := len(*m.recent) - 2; i >= 0; i-- {
		if (*m.recent)[i] == fingerprint {
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
	fingerprint, err := responseFingerprint(resp.Content, resp.ToolCalls)
	if err != nil {
		return nil, err
	}
	if err := m.checkLoop(fingerprint); err != nil {
		return nil, err
	}
	return resp, nil
}

// Stream forwards chunks from the inner model in real-time via a goroutine.
// After the stream completes, the accumulated content is checked for a loop
// (post-hoc detection). If a loop is detected, a warning is logged and a
// final MessageChunk carrying the loop error is sent so the caller can
// terminate the turn. The loop history is also recorded so subsequent calls
// will detect the loop earlier.
func (m *loopDetectModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	streamCtx, cancel := context.WithCancelCause(ctx)
	ch, err := m.next.Stream(streamCtx, msgs, opts...)
	if err != nil {
		cancelStreamAttempt(ctx, cancel, err)
		drainMessageChunks(ch)
		return nil, err
	}
	if ch == nil {
		err = fmt.Errorf("loopdetection: model returned a nil stream")
		cancelStreamAttempt(ctx, cancel, err)
		return nil, err
	}

	out := make(chan MessageChunk, 16)
	go func() {
		defer close(out)
		var acc strings.Builder
		var totalBytes int
		var totalChunks int
		var finalChunk *MessageChunk
		fail := func(failure error, sourceOpen bool) {
			sendStreamError(streamCtx, out, failure)
			cancelStreamAttempt(ctx, cancel, failure)
			if sourceOpen {
				drainMessageChunks(ch)
			}
		}
		for {
			var chunk MessageChunk
			var ok bool
			select {
			case <-streamCtx.Done():
				fail(context.Cause(streamCtx), true)
				return
			case chunk, ok = <-ch:
			}
			if !ok {
				break
			}
			if chunk.Error != nil {
				fail(chunk.Error, true)
				return
			}
			if finalChunk != nil {
				fail(fmt.Errorf("loopdetection: stream emitted data after its final chunk"), true)
				return
			}
			totalChunks++
			if totalChunks > GuardedStreamMaxChunks {
				fail(fmt.Errorf("loopdetection: stream exceeds %d chunks", GuardedStreamMaxChunks), true)
				return
			}
			textBytes := RetainedStreamChunkTextBytes(chunk)
			if textBytes > GuardedStreamMaxBytes-totalBytes {
				fail(fmt.Errorf("loopdetection: stream exceeds %d-byte candidate limit", GuardedStreamMaxBytes), true)
				return
			}
			totalBytes += textBytes
			acc.WriteString(chunk.Content)
			if chunk.Final {
				final := chunk
				final.Content = ""
				finalChunk = &final
				if chunk.Content != "" {
					contentOnly := chunk
					contentOnly.Final = false
					contentOnly.ToolCalls = nil
					contentOnly.FinishReason = ""
					contentOnly.Usage = nil
					select {
					case out <- contentOnly:
					case <-streamCtx.Done():
						fail(context.Cause(streamCtx), true)
						return
					}
				}
				continue
			}
			select {
			case out <- chunk:
			case <-streamCtx.Done():
				fail(context.Cause(streamCtx), true)
				return
			}
		}
		if finalChunk == nil {
			fail(fmt.Errorf("loopdetection: stream ended without a final chunk"), false)
			return
		}
		// Post-hoc loop detection after all chunks have been forwarded.
		fingerprint, fingerprintErr := responseFingerprint(acc.String(), finalChunk.ToolCalls)
		if fingerprintErr != nil {
			fail(fingerprintErr, false)
			return
		}
		if loopErr := m.checkLoop(fingerprint); loopErr != nil {
			m.logger.Warn("loopdetection.stream.post_hoc",
				"op", "loopdetection.stream.post_hoc",
				"error", loopErr.Error(),
			)
			fail(loopErr, false)
			return
		}
		select {
		case out <- *finalChunk:
			cancel(nil)
		case <-ctx.Done():
			cause := context.Cause(ctx)
			if cause == nil {
				cause = ctx.Err()
			}
			fail(cause, false)
		}
	}()
	return out, nil
}
