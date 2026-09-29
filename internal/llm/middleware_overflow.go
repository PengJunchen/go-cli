package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

const (
	finishReasonLength = "length"
	maxContinuations   = 3
)

// OverflowRecoveryMiddleware detects context overflow errors from the model
// provider and retries the call with a reduced message history. When the
// underlying model returns an error containing overflow indicators (e.g.
// "context_length_exceeded", "maximum context length"), the middleware
// trims older messages from the conversation and retries.
type OverflowRecoveryMiddleware struct {
	maxRetries    int
	trimRatio     float64 // fraction of messages to remove on each retry
	logger        *slog.Logger
	mu            sync.Mutex
	overflowCount int
}

var _ ModelMiddleware = (*OverflowRecoveryMiddleware)(nil)

// OverflowOption configures an OverflowRecoveryMiddleware.
type OverflowOption func(*OverflowRecoveryMiddleware)

// WithOverflowMaxRetries sets the maximum number of retry attempts after
// overflow detection.
func WithOverflowMaxRetries(n int) OverflowOption {
	return func(m *OverflowRecoveryMiddleware) { m.maxRetries = n }
}

// WithOverflowTrimRatio sets the fraction of messages to remove on each
// retry (0.0-1.0, default 0.3 = remove oldest 30%).
func WithOverflowTrimRatio(r float64) OverflowOption {
	return func(m *OverflowRecoveryMiddleware) { m.trimRatio = r }
}

// WithOverflowLogger sets a custom logger.
func WithOverflowLogger(l *slog.Logger) OverflowOption {
	return func(m *OverflowRecoveryMiddleware) { m.logger = l }
}

// NewOverflowRecoveryMiddleware creates a middleware that recovers from
// context overflow errors by trimming message history and retrying.
func NewOverflowRecoveryMiddleware(opts ...OverflowOption) *OverflowRecoveryMiddleware {
	m := &OverflowRecoveryMiddleware{
		maxRetries: 2,
		trimRatio:  0.3,
		logger:     slog.Default(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name implements ModelMiddleware.
func (m *OverflowRecoveryMiddleware) Name() string { return "overflow" }

// WrapModel implements ModelMiddleware.
func (m *OverflowRecoveryMiddleware) WrapModel(next BaseChatModel) BaseChatModel {
	return &overflowRecoveryModel{
		mw:    m,
		inner: next,
	}
}

// overflowRecoveryModel is the wrapped BaseChatModel that performs
// overflow detection and recovery.
type overflowRecoveryModel struct {
	mw    *OverflowRecoveryMiddleware
	inner BaseChatModel
}

var _ BaseChatModel = (*overflowRecoveryModel)(nil)

// Generate implements BaseChatModel.
func (m *overflowRecoveryModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	currentMsgs := msgs

	for attempt := 0; attempt <= m.mw.maxRetries; attempt++ {
		result, err := m.inner.Generate(ctx, currentMsgs, opts...)
		if err == nil {
			// Successful response: if the output was truncated by
			// max_tokens (FinishReason == "length"), automatically
			// request continuation(s) to retrieve the remaining content.
			if result != nil && result.FinishReason == finishReasonLength && !LoopOwnsContinuation(ctx) && !ContinuationNoReplay(ctx) {
				contResult, contErr := m.continueGeneration(ctx, currentMsgs, result, opts)
				if contErr != nil {
					m.mw.logger.Warn("middleware.overflow.continue_error",
						"op", "middleware.overflow.continue_error",
						"err", contErr,
					)
					return nil, contErr
				}
				result = contResult
			}
			return result, nil
		}

		if !isOverflowError(err) {
			return nil, err
		}
		if ContinuationNoReplay(ctx) {
			return nil, err
		}

		m.mw.mu.Lock()
		m.mw.overflowCount++
		m.mw.mu.Unlock()

		if attempt >= m.mw.maxRetries {
			m.mw.logger.Warn("middleware.overflow.exhausted",
				"op", "middleware.overflow.exhausted",
				"attempts", attempt+1,
				"remaining_msgs", len(currentMsgs),
			)
			return nil, fmt.Errorf("overflow: max retries (%d) exhausted: %w", m.mw.maxRetries, err)
		}

		// Trim oldest messages (keep system message if present).
		trimmed := trimMessages(currentMsgs, m.mw.trimRatio)
		m.mw.logger.Debug("middleware.overflow.retry",
			"op", "middleware.overflow.retry",
			"attempt", attempt+1,
			"msgs_before", len(currentMsgs),
			"msgs_after", len(trimmed),
		)
		currentMsgs = trimmed

		if len(currentMsgs) == 0 {
			return nil, fmt.Errorf("overflow: all messages trimmed: %w", err)
		}
	}

	return nil, fmt.Errorf("overflow: unexpected state")
}

// Stream implements BaseChatModel. It forwards chunks from the inner model
// in real-time via a goroutine, avoiding the latency of buffering the entire
// stream before returning. Overflow detection still occurs at the
// m.inner.Stream() call level: when the inner model returns an overflow
// error, the middleware trims messages and retries. After a successful
// stream, if FinishReason is "length", continuation is requested via Generate
// and the extra content is sent as additional chunks.
func (m *overflowRecoveryModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	currentMsgs := msgs

	for attempt := 0; attempt <= m.mw.maxRetries; attempt++ {
		streamCtx, cancel := context.WithCancelCause(ctx)
		ch, err := m.inner.Stream(streamCtx, currentMsgs, opts...)
		if err == nil && ch == nil {
			err = errors.New("overflow: inner model returned a nil stream")
			cancelStreamAttempt(ctx, cancel, err)
			return nil, err
		}
		if err == nil {
			// Success: forward bounded chunks in real-time, but hold the final
			// marker until any middleware-owned continuation has completed.
			outCh := make(chan MessageChunk, 64)
			go func() {
				defer close(outCh)
				var contentBuf strings.Builder
				completeContent := ""
				var contentBytes int
				var chunkCount int
				var finalChunk *MessageChunk
				sourceClosed := false
				fail := func(failure error) {
					sendStreamError(streamCtx, outCh, failure)
					cancelStreamAttempt(ctx, cancel, failure)
					if !sourceClosed {
						drainMessageChunks(ch)
						sourceClosed = true
					}
				}

				for {
					var chunk MessageChunk
					var ok bool
					select {
					case <-streamCtx.Done():
						cause := context.Cause(streamCtx)
						if cause == nil {
							cause = streamCtx.Err()
						}
						fail(cause)
						return
					case chunk, ok = <-ch:
					}
					if !ok {
						sourceClosed = true
						break
					}
					if chunk.Error != nil {
						fail(chunk.Error)
						return
					}
					if streamCtx.Err() != nil {
						cause := context.Cause(streamCtx)
						if cause == nil {
							cause = streamCtx.Err()
						}
						fail(cause)
						return
					}
					if finalChunk != nil {
						fail(errors.New("overflow: stream emitted data after its final chunk"))
						return
					}
					chunkCount++
					if chunkCount > GuardedStreamMaxChunks {
						fail(fmt.Errorf("overflow: stream exceeds %d chunks", GuardedStreamMaxChunks))
						return
					}
					textBytes := RetainedStreamChunkTextBytes(chunk)
					if textBytes > GuardedStreamMaxBytes-contentBytes {
						fail(fmt.Errorf("overflow: stream exceeds %d-byte candidate limit", GuardedStreamMaxBytes))
						return
					}
					contentBytes += textBytes
					if chunk.Content != "" {
						contentBuf.WriteString(chunk.Content)
						contentOnly := chunk
						contentOnly.Final = false
						contentOnly.ToolCalls = nil
						contentOnly.FinishReason = ""
						contentOnly.Usage = nil
						select {
						case outCh <- contentOnly:
						case <-streamCtx.Done():
							cause := context.Cause(streamCtx)
							if cause == nil {
								cause = streamCtx.Err()
							}
							fail(cause)
							return
						}
					}
					if chunk.Final {
						final := chunk
						final.Content = ""
						finalChunk = &final
					}
				}

				if streamCtx.Err() != nil {
					cause := context.Cause(streamCtx)
					if cause == nil {
						cause = streamCtx.Err()
					}
					fail(cause)
					return
				}
				if finalChunk == nil {
					fail(errors.New("overflow: stream ended without a final chunk"))
					return
				}
				completeContent = contentBuf.String()
				// The core loop is the single continuation authority for guarded
				// candidates. Other callers retain middleware-owned continuation.
				if !LoopOwnsContinuation(streamCtx) && !ContinuationNoReplay(streamCtx) && finalChunk.FinishReason == finishReasonLength {
					result := &Message{
						Role:         RoleAssistant,
						Content:      contentBuf.String(),
						ToolCalls:    finalChunk.ToolCalls,
						FinishReason: finalChunk.FinishReason,
						Usage:        finalChunk.Usage,
					}
					origLen := len(result.Content)
					contResult, contErr := m.continueGeneration(streamCtx, currentMsgs, result, opts)
					if contErr != nil {
						m.mw.logger.Warn("middleware.overflow.stream_continue_error",
							"op", "middleware.overflow.stream_continue_error",
							"err", contErr,
						)
						fail(contErr)
						return
					}
					if contResult == nil || len(contResult.Content) < origLen {
						fail(errors.New("overflow: continuation returned an incomplete response"))
						return
					}
					if len(contResult.Content) > origLen {
						extra := contResult.Content[origLen:]
						if extra != "" {
							select {
							case outCh <- MessageChunk{Role: RoleAssistant, Content: extra}:
							case <-streamCtx.Done():
								cause := context.Cause(streamCtx)
								if cause == nil {
									cause = streamCtx.Err()
								}
								fail(cause)
								return
							}
						}
					}
					finalChunk.FinishReason = contResult.FinishReason
					finalChunk.ToolCalls = contResult.ToolCalls
					finalChunk.Usage = contResult.Usage
					completeContent = contResult.Content
				}
				retainedCalls := finalChunk.ToolCalls
				if finalChunk.FinishReason == finishReasonLength {
					// Partial arguments from a truncated response are not part of
					// the retained logical candidate; the core drops them before
					// issuing its next continuation request.
					retainedCalls = nil
				}
				complete := &Message{
					Role:         RoleAssistant,
					Content:      completeContent,
					ToolCalls:    retainedCalls,
					FinishReason: finalChunk.FinishReason,
					Usage:        finalChunk.Usage,
				}
				if !LoopOwnsContinuation(streamCtx) || finalChunk.FinishReason != finishReasonLength {
					retainedBytes, sizeErr := RetainedMessagePayloadBytes(complete)
					if sizeErr != nil {
						fail(fmt.Errorf("overflow: encode complete stream response: %w", sizeErr))
						return
					}
					if retainedBytes > GuardedStreamMaxBytes {
						fail(fmt.Errorf("overflow: complete stream response exceeds %d-byte candidate limit", GuardedStreamMaxBytes))
						return
					}
				}
				select {
				case outCh <- *finalChunk:
					cancel(nil)
				case <-streamCtx.Done():
					cause := context.Cause(streamCtx)
					if cause == nil {
						cause = streamCtx.Err()
					}
					fail(cause)
					return
				}
			}()
			return outCh, nil
		}
		cancelStreamAttempt(ctx, cancel, err)
		drainMessageChunks(ch)

		if !isOverflowError(err) {
			return nil, err
		}
		if ContinuationNoReplay(ctx) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		m.mw.mu.Lock()
		m.mw.overflowCount++
		m.mw.mu.Unlock()

		if attempt >= m.mw.maxRetries {
			m.mw.logger.Warn("middleware.overflow.stream_exhausted",
				"op", "middleware.overflow.stream_exhausted",
				"attempts", attempt+1,
			)
			return nil, fmt.Errorf("overflow: max retries (%d) exhausted: %w", m.mw.maxRetries, err)
		}

		trimmed := trimMessages(currentMsgs, m.mw.trimRatio)
		m.mw.logger.Debug("middleware.overflow.stream_retry",
			"op", "middleware.overflow.stream_retry",
			"attempt", attempt+1,
			"msgs_before", len(currentMsgs),
			"msgs_after", len(trimmed),
		)
		currentMsgs = trimmed

		if len(currentMsgs) == 0 {
			return nil, fmt.Errorf("overflow: all messages trimmed: %w", err)
		}
	}

	return nil, fmt.Errorf("overflow: unexpected state")
}

// continueGeneration appends the partial assistant response to the
// conversation and re-requests generation to obtain the remaining content.
// Up to maxContinuations attempts are made. Results are merged into the
// original response.
func (m *overflowRecoveryModel) continueGeneration(ctx context.Context, msgs []Message, result *Message, opts []Option) (*Message, error) {
	if result == nil {
		return nil, errors.New("overflow: cannot continue a nil response")
	}
	for attempt := 0; attempt < maxContinuations && result.FinishReason == finishReasonLength; attempt++ {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("overflow: continuation canceled: %w", ctx.Err())
		}
		// Partial tool-call arguments are never re-submitted or dispatched.
		result.ToolCalls = nil
		// Build continuation messages: original + partial assistant response.
		contMsgs := make([]Message, len(msgs), len(msgs)+1)
		copy(contMsgs, msgs)
		contMsgs = append(contMsgs, Message{
			Role:         RoleAssistant,
			Content:      result.Content,
			ToolCalls:    result.ToolCalls,
			FinishReason: result.FinishReason,
		})

		m.mw.logger.Debug("middleware.overflow.continue",
			"op", "middleware.overflow.continue",
			"attempt", attempt+1,
		)

		contCtx := WithContinuationNoReplay(ctx)
		contResp, err := m.inner.Generate(contCtx, contMsgs, opts...)
		if err != nil {
			m.mw.logger.Warn("middleware.overflow.continue_failed",
				"op", "middleware.overflow.continue_failed",
				"attempt", attempt+1,
				"err", err,
			)
			return nil, fmt.Errorf("overflow: continuation %d failed: %w", attempt+1, err)
		}
		if contResp == nil {
			return nil, fmt.Errorf("overflow: continuation %d returned a nil response", attempt+1)
		}

		// Merge continuation content.
		if len(contResp.Content) > GuardedStreamMaxBytes-len(result.Content) {
			return nil, fmt.Errorf("overflow: continuation exceeds %d-byte content limit", GuardedStreamMaxBytes)
		}
		result.Content += contResp.Content
		result.FinishReason = contResp.FinishReason
		result.ToolCalls = contResp.ToolCalls
		if contResp.Usage != nil {
			if result.Usage == nil {
				usage := *contResp.Usage
				result.Usage = &usage
			} else {
				result.Usage.InputTokens += contResp.Usage.InputTokens
				result.Usage.OutputTokens += contResp.Usage.OutputTokens
				result.Usage.TotalTokens += contResp.Usage.TotalTokens
			}
		}
		encoded, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			return nil, fmt.Errorf("overflow: encode complete continuation: %w", marshalErr)
		}
		if len(encoded) > GuardedStreamMaxBytes {
			return nil, fmt.Errorf("overflow: complete continuation exceeds %d-byte limit", GuardedStreamMaxBytes)
		}
	}

	if result.FinishReason == finishReasonLength {
		m.mw.logger.Warn("middleware.overflow.continue_exhausted",
			"op", "middleware.overflow.continue_exhausted",
			"max_continuations", maxContinuations,
		)
		return nil, fmt.Errorf("overflow: response remains truncated after %d continuations", maxContinuations)
	}
	return result, nil
}

// isOverflowError checks whether an error indicates a context length
// overflow from the model provider. It first tries a structured ProviderError
// type assertion (errors.As traverses the Unwrap chain). For non-ProviderError
// errors it falls back to keyword matching for backward compatibility.
func isOverflowError(err error) bool {
	if err == nil {
		return false
	}
	// Prefer structured ProviderError (errors.As traverses the Unwrap chain).
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.ErrorType == ErrTypeOverflow
	}
	// Fallback: keyword matching for non-ProviderError errors (backward compat).
	return containsOverflowIndicator(err.Error())
}

// trimMessages removes the oldest fraction of messages from the slice.
// It preserves the first message (typically the system prompt) if present.
func trimMessages(msgs []Message, ratio float64) []Message {
	if len(msgs) <= 1 {
		return msgs
	}

	// Preserve system message at index 0 if present.
	startIdx := 0
	if msgs[0].Role == "system" {
		startIdx = 1
	}

	// Calculate how many messages to remove from the non-system portion.
	removable := len(msgs) - startIdx
	if removable <= 0 {
		return msgs
	}

	trimCount := int(float64(removable) * ratio)
	if trimCount < 1 {
		trimCount = 1
	}

	// Build the trimmed slice: system + remaining messages after trim.
	result := make([]Message, 0, len(msgs)-trimCount)
	result = append(result, msgs[:startIdx]...)
	result = append(result, msgs[startIdx+trimCount:]...)
	return result
}
