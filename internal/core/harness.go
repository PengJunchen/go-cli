package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// harnessConfig holds the configurable knobs of a HarnessImpl.
type harnessConfig struct {
	startSpan  string
	bufferSize int
	discard    DiscardPolicy
}

// HarnessOption configures a HarnessImpl at construction time.
type HarnessOption func(*harnessConfig)

// WithEventBuffer sets the capacity of each event stream the harness creates.
func WithEventBuffer(n int) HarnessOption {
	return func(c *harnessConfig) {
		if n > 0 {
			c.bufferSize = n
		}
	}
}

// WithDiscardPolicy sets the discard policy applied when a bounded stream
// buffer is full. The underlying EventStreamImpl enforces its own backpressure;
// this option is retained for API completeness and forward compatibility.
func WithDiscardPolicy(p DiscardPolicy) HarnessOption {
	return func(c *harnessConfig) { c.discard = p }
}

// WithStartSpanName overrides the name of the root span emitted by Submit.
func WithStartSpanName(name string) HarnessOption {
	return func(c *harnessConfig) {
		if name != "" {
			c.startSpan = name
		}
	}
}

// HarnessImpl is the full runtime facade. It accepts a user message and
// returns an EventStream that streams agent events until the run completes.
type HarnessImpl struct {
	agent         Agent
	startSpanName string
	bufferSize    int
}

var _ Harness = (*HarnessImpl)(nil)

// NewHarnessImpl builds a HarnessImpl bound to the given Agent. A nil agent
// panics because a harness with no agent can process no submissions.
func NewHarnessImpl(agent Agent, opts ...HarnessOption) *HarnessImpl {
	if agent == nil {
		panic("core: harness requires a non-nil Agent")
	}
	cfg := harnessConfig{startSpan: "harness.submit"}
	for _, o := range opts {
		o(&cfg)
	}
	h := &HarnessImpl{
		agent:         agent,
		startSpanName: cfg.startSpan,
		bufferSize:    cfg.bufferSize,
	}
	slog.Info("core.harness.new",
		"agent", agent.Name(),
		"start_span", cfg.startSpan,
		"buffer", cfg.bufferSize,
		"discard", cfg.discard.String(),
	)
	return h
}

// Submit runs the agent asynchronously and returns an EventStream immediately.
// Non-blocking: the caller owns the stream and must drain it until it closes.
func (h *HarnessImpl) Submit(ctx context.Context, msg string) (EventStream, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, h.startSpanName, tracing.SpanKindInternal)
	span.SetAttributes(tracing.Attribute{Key: "user_message", Value: msg})
	defer span.End()
	logger := tracing.NewTraceLogger(span, nil)
	logger.Info("core.harness.submit", "msg", msg)

	stream := NewEventStream(h.bufferSize)
	if stream == nil {
		return nil, context.Canceled
	}

	submission := Submission{Type: SubmissionUserMessage, Content: msg}

	go h.run(spanCtx, stream, submission)

	return stream, nil
}

// run executes the agent and fans its events out to the stream. It always
// records a terminal result, sends a closing event, and closes the stream so
// consumers can observe completion without a goroutine leak.
func (h *HarnessImpl) run(ctx context.Context, stream *EventStreamImpl, submission Submission) {
	result, err := h.agent.Run(ctx, submission)

	var events []AgentEvent
	if es, ok := h.agent.(eventSource); ok {
		events = es.Events()
	}
	// Send is best-effort: a send after close (or a full buffer) is ignored by
	// design, so the returned error is passed to bestEffort and discarded.
	for _, ev := range events {
		bestEffort(stream.Send(ev))
	}

	if err != nil {
		stream.SetResult(AgentMessage{Role: "assistant", Content: result.Message}, err)
		bestEffort(stream.Send(errEvent(err)))
		stream.Close()
		return
	}

	stream.SetResult(AgentMessage{Role: "assistant", Content: result.Message}, nil)
	bestEffort(stream.Send(AgentEvent{Kind: "done", Content: result.Message, Timestamp: time.Now()}))
	stream.Close()
}

// bestEffort discards an error that is intentionally not actionable. EventStream.Send
// returns nil after the stream is closed, so a best-effort send must not fail the run.
func bestEffort(_ error) {}

// SetResult records the final result message and error on a stream. It is the
// counterpart of Send/Close and is used by the harness to publish a run outcome.
// After the stream is closed the value is not overwritten.
func (s *EventStreamImpl) SetResult(msg AgentMessage, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.result = msg
	s.hasRes = true
	s.err = err
	slog.Info("core.eventstream.result", "has_res", s.hasRes, "error", err != nil)
}
