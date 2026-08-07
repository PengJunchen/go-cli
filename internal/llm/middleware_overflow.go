package llm

import (
	"context"
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
			if result != nil && result.FinishReason == finishReasonLength {
				result, _ = m.continueGeneration(ctx, currentMsgs, result, opts)
			}
			return result, nil
		}

		if !isOverflowError(err) {
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

// Stream implements BaseChatModel.
func (m *overflowRecoveryModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	currentMsgs := msgs

	// Collect the stream so we can inspect the FinishReason and retry on
	// overflow errors (the final chunk carrying FinishReason is only known
	// after the stream is fully drained).
	var result *Message
	for attempt := 0; attempt <= m.mw.maxRetries; attempt++ {
		collected, err := m.collectStream(ctx, currentMsgs, opts)
		if err == nil {
			result = collected
			break
		}

		if !isOverflowError(err) {
			return nil, err
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

	// Guard against an empty stream response.
	if result == nil {
		result = &Message{Role: RoleAssistant}
	}

	// If the output was truncated by max_tokens, request continuation(s)
	// via Generate (simpler than re-streaming).
	if result.FinishReason == finishReasonLength {
		result, _ = m.continueGeneration(ctx, currentMsgs, result, opts)
	}

	// Re-emit the (possibly continued) result as a single final chunk.
	ch := make(chan MessageChunk, 1)
	ch <- MessageChunk{
		Role:         RoleAssistant,
		Content:      result.Content,
		Final:        true,
		ToolCalls:    result.ToolCalls,
		FinishReason: result.FinishReason,
	}
	close(ch)
	return ch, nil
}

// collectStream calls inner.Stream, drains the channel, and returns the
// accumulated result as a *Message. It mirrors the chunk-accumulation logic
// in core/loop.go's streamGenerate.
func (m *overflowRecoveryModel) collectStream(ctx context.Context, msgs []Message, opts []Option) (*Message, error) {
	ch, err := m.inner.Stream(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}

	var contentBuf strings.Builder
	var toolCalls []ToolCall
	var finishReason string
	gotChunk := false

	for chunk := range ch {
		gotChunk = true
		if chunk.Content != "" {
			contentBuf.WriteString(chunk.Content)
		}
		if chunk.Final {
			if len(chunk.ToolCalls) > 0 {
				toolCalls = chunk.ToolCalls
			}
			if chunk.FinishReason != "" {
				finishReason = chunk.FinishReason
			}
		}
	}

	if !gotChunk {
		return nil, nil
	}

	return &Message{
		Role:         RoleAssistant,
		Content:      contentBuf.String(),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
	}, nil
}

// continueGeneration appends the partial assistant response to the
// conversation and re-requests generation to obtain the remaining content.
// Up to maxContinuations attempts are made. Results are merged into the
// original response.
func (m *overflowRecoveryModel) continueGeneration(ctx context.Context, msgs []Message, result *Message, opts []Option) (*Message, error) {
	for attempt := 0; attempt < maxContinuations && result.FinishReason == finishReasonLength; attempt++ {
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

		contResp, err := m.inner.Generate(ctx, contMsgs, opts...)
		if err != nil || contResp == nil {
			break
		}

		// Merge continuation content.
		result.Content += contResp.Content
		result.FinishReason = contResp.FinishReason
		if len(contResp.ToolCalls) > 0 {
			result.ToolCalls = append(result.ToolCalls, contResp.ToolCalls...)
		}
	}

	if result.FinishReason == finishReasonLength {
		m.mw.logger.Warn("middleware.overflow.continue_exhausted",
			"op", "middleware.overflow.continue_exhausted",
			"max_continuations", maxContinuations,
		)
	}

	return result, nil
}

// isOverflowError checks whether an error indicates a context length
// overflow from the model provider.
func isOverflowError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	indicators := []string{
		"context_length_exceeded",
		"maximum context length",
		"context window",
		"token limit exceeded",
		"context_length",
	}
	for _, ind := range indicators {
		if strings.Contains(msg, ind) {
			return true
		}
	}
	return false
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
