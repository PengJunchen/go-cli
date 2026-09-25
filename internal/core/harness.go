package core

import (
	"context"
	"log/slog"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// harnessConfig holds the configurable knobs of a HarnessImpl.
type harnessConfig struct {
	startSpan      string
	bufferSize     int
	discard        DiscardPolicy
	tracer         *tracing.Tracer
	runSlot        RunSlotGuard
	cleanupTimeout time.Duration
	lifetimeCtx    context.Context
	bus            EventBus
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
// buffer is full. The policy is forwarded to each EventStreamImpl created by
// the harness. See DiscardPolicy constants for available strategies.
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

// WithHarnessTracer sets a tracing.Tracer on the HarnessImpl. When non-nil, the
// harness injects the Tracer into the context before creating its root span so
// that all downstream components (agent, middleware, loop, tools) share the
// same trace.
func WithHarnessTracer(t *tracing.Tracer) HarnessOption {
	return func(c *harnessConfig) { c.tracer = t }
}

// WithRunSlotGuard sets the RunSlotGuard used to enforce single-run
// exclusivity. When nil (or not set), the harness uses the noop default so
// concurrent Submits are allowed.
func WithRunSlotGuard(g RunSlotGuard) HarnessOption {
	return func(c *harnessConfig) { c.runSlot = g }
}

// WithRunLifetimeContext links every submitted run to its Assembly-owned
// lifetime. Canceling the lifetime stops run input; the run owner still
// supervises and retains any resources that have not quiesced.
func WithRunLifetimeContext(ctx context.Context) HarnessOption {
	return func(c *harnessConfig) { c.lifetimeCtx = ctx }
}

// WithRunCleanupTimeout sets how long the harness waits for owned resources
// after its top-level agent callback returns or cancellation is requested.
func WithRunCleanupTimeout(timeout time.Duration) HarnessOption {
	return func(c *harnessConfig) {
		if timeout > 0 {
			c.cleanupTimeout = timeout
		}
	}
}

// WithHarnessEventBus wires an EventBus that is forwarded to every
// EventStream created by the harness. Each event successfully sent to a
// stream is dual-written to the bus, enabling fan-out to multiple
// consumers (e.g. the SSE /events endpoint). When nil (or not set), no
// dual-write occurs and behavior is unchanged.
func WithHarnessEventBus(bus EventBus) HarnessOption {
	return func(c *harnessConfig) { c.bus = bus }
}

// HarnessImpl is the full runtime facade. It accepts a user message and
// returns an EventStream that streams agent events until the run completes.
type HarnessImpl struct {
	agent          Agent
	startSpanName  string
	bufferSize     int
	discard        DiscardPolicy
	tracer         *tracing.Tracer
	runSlot        RunSlotGuard
	cleanupTimeout time.Duration
	lifetimeCtx    context.Context
	bus            EventBus
}

var _ Harness = (*HarnessImpl)(nil)

// NewHarnessImpl builds a HarnessImpl bound to the given Agent. A nil agent
// panics because a harness with no agent can process no submissions.
func NewHarnessImpl(agent Agent, opts ...HarnessOption) *HarnessImpl {
	if agent == nil {
		panic("core: harness requires a non-nil Agent")
	}
	cfg := harnessConfig{
		startSpan:      "harness.start",
		discard:        BlockUntilConsumed, // preserve backward-compatible blocking
		cleanupTimeout: defaultRunCleanupTimeout,
	}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.runSlot == nil {
		cfg.runSlot = defaultRunSlotGuard
	}
	h := &HarnessImpl{
		agent:          agent,
		startSpanName:  cfg.startSpan,
		bufferSize:     cfg.bufferSize,
		discard:        cfg.discard,
		tracer:         cfg.tracer,
		runSlot:        cfg.runSlot,
		cleanupTimeout: cfg.cleanupTimeout,
		lifetimeCtx:    cfg.lifetimeCtx,
		bus:            cfg.bus,
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
	runInputCtx, inputCancel, stopLifetime := linkRunLifetime(ctx, h.lifetimeCtx)

	// Claim the run slot before launching the goroutine so concurrent
	// Submits fail fast when a run is already in progress. The 200ms
	// timeout bounds how long a caller waits for the slot.
	claimCtx, claimCancel := context.WithTimeout(runInputCtx, 200*time.Millisecond)
	claim, err := h.runSlot.ClaimRun(claimCtx)
	claimCancel()
	if err != nil {
		slog.Warn("core.harness.claim_failed", "err", err)
		stopLifetime()
		inputCancel()
		return nil, err
	}
	wrapperDone, err := registerRunWrapper(h.runSlot, claim, nil)
	if err != nil {
		h.runSlot.Release(claim)
		stopLifetime()
		inputCancel()
		return nil, err
	}
	transferred := false
	var span tracing.TraceSpan
	var stream *EventStreamImpl
	defer func() {
		if transferred {
			return
		}
		defer func() {
			h.runSlot.Release(claim)
			wrapperDone()
			stopLifetime()
			inputCancel()
		}()
		if stream != nil {
			stream.Close()
		}
		if span != nil {
			span.End()
		}
	}()

	// Borrowed tracing and bus producers are touched only after the guard has
	// accepted this run, so a closed Assembly cannot race its teardown with a
	// late Submit.
	ctxWithTracer := runInputCtx
	if h.tracer != nil {
		ctxWithTracer = h.tracer.ContextWithTracer(ctxWithTracer)
	}
	var spanCtx context.Context
	span, spanCtx = tracing.SpanFromContext(ctxWithTracer, h.startSpanName, tracing.SpanKindInternal)
	span.SetAttributes(
		tracing.SensitiveAttribute("user_message", msg),
		tracing.Attribute{Key: "user_message_length", Value: len(msg)},
	)
	logger := tracing.NewTraceLogger(span, nil)
	logger.Info("core.harness.start", "msg_len", len(msg))

	streamOpts := []EventStreamOption{WithEventDiscardPolicy(h.discard)}
	if h.bus != nil {
		streamOpts = append(streamOpts, WithEventBus(h.bus))
	}
	stream = NewEventStream(h.bufferSize, streamOpts...)
	if stream == nil {
		return nil, context.Canceled
	}

	owner := newRunOwner(spanCtx, h.runSlot, claim, func(result Result, err error) {
		stream.SetResult(AgentMessage{Role: "assistant", Content: result.Message}, err)
		if err != nil {
			bestEffort(stream.SendTerminal(errEvent(err)))
		} else {
			bestEffort(stream.SendTerminal(AgentEvent{Kind: "done", Content: result.Message, Timestamp: time.Now()}))
		}
	})
	owner.SetEventGapSource(stream)
	stream.SetRunStatusHandle(owner)
	err = setRunWrapperStatus(h.runSlot, claim, owner)
	if err != nil {
		stream.SetResult(AgentMessage{Role: "assistant"}, err)
		return nil, err
	}

	submission := Submission{Type: SubmissionUserMessage, Content: msg}

	transferred = true
	go h.run(spanCtx, span, stream, submission, owner, wrapperDone, func() {
		stopLifetime()
		inputCancel()
	})

	return stream, nil
}

// run executes the agent and fans its events out to the stream. It always
// records a terminal result, sends a closing event, and closes the stream so
// consumers can observe completion without a goroutine leak. The run owner
// releases its claim when callback resources finish; the wrapper lease keeps
// admission fenced through span export and stream close.
func (h *HarnessImpl) run(ctx context.Context, span tracing.TraceSpan, stream *EventStreamImpl, submission Submission, owner *runOwner, wrapperDone func(), cleanupInput func()) {
	defer cleanupInput()
	defer wrapperDone()
	defer stream.Close()
	defer span.End()
	// Pass the EventStream to the agent so events are emitted in real time
	// as the LLM streams tokens. The agent also stores events internally
	// for backward-compatible retrieval via the eventSource interface.
	result, err := owner.run(h.cleanupTimeout, func(runCtx context.Context) (Result, error) {
		r, e := h.agent.Run(runCtx, submission, stream)
		if stream.SentCount() == 0 {
			if es, ok := h.agent.(eventSource); ok {
				for _, ev := range es.Events() {
					bestEffort(stream.Send(ev))
				}
			}
		}
		return r, e
	})
	span.SetAttributes(
		tracing.Attribute{Key: "run_id", Value: result.RunID},
		tracing.Attribute{Key: "outcome", Value: string(result.Outcome)},
		tracing.Attribute{Key: "execution_state", Value: string(result.ExecutionState)},
	)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
	}
	stream.SetResult(AgentMessage{Role: "assistant", Content: result.Message}, err)
	if err != nil {
		bestEffort(stream.SendTerminal(errEvent(err)))
	} else {
		bestEffort(stream.SendTerminal(AgentEvent{Kind: "done", Content: result.Message, Timestamp: time.Now()}))
	}
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
	if s.closed.Load() {
		return
	}
	s.result = cloneAgentMessage(msg)
	s.hasRes = true
	s.err = err
	slog.Info("core.eventstream.result", "has_res", s.hasRes, "error", err != nil)
}
