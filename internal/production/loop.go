// Package production provides resilience and safety components for the
// go-cli runtime: loop detection, a circuit breaker, and a default retry
// policy. These components are intentionally decoupled from any service or
// extension package; they operate on core.AgentEvent values and generic
// func()(any, error) callables so downstream integrations (e.g. wrapping an
// LLM provider) remain thin, cycle-free adapters.
//
// Components
//
//   - LoopDetector (loop.go) watches an agent event stream for recurring
//     edit / test-failure / identical-tool-call patterns and reports whether
//     the runtime looks stuck in a loop.
//   - CircuitBreaker (circuit.go) is a three-state machine (Closed / Open /
//     HalfOpen) around an arbitrary callable, with optional fallback.
//   - DefaultRetryPolicy (retry.go) classifies errors and computes exponential
//     backoff with jitter.
//
// Integration with internal/llm
//
// DefaultCircuitBreaker deliberately operates on a generic func()(any, error)
// callable and never imports internal/llm, which keeps this package free of
// dependency cycles. A model provider can be wrapped by a thin adapter that
// captures the provider's Build result in a closure:
//
//	// pseudo-code (lives in a downstream package that may import llm):
//	model, release, err := provider.Build(ctx, cfg)
//	breakeredModel := func() (any, error) { return model.Generate(ctx, msgs) }
//	out, err := breaker.Execute(ctx, breakeredModel)
//
// The adapter owns the provider span; the breaker owns the circuit span. Any
// BaseChatModel may be protected without the breaker knowing about llm at all.
package production

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// Loop detection dimension identifiers returned by LoopDetectionResult.Dimension.
const (
	// DimensionEditCount tracks repeated edits to the same target file.
	DimensionEditCount = "edit_count"
	// DimensionTestFailure tracks consecutive test failures.
	DimensionTestFailure = "test_failure"
	// DimensionSameToolCall tracks repeated identical tool invocations.
	DimensionSameToolCall = "same_tool_call"
)

// Default event kinds routed to each LoopDetector dimension. These classify
// the AgentEvent.Kind field. Consumers may override them via config.
const (
	// KindEdit is the event kind marking a file edit (Content is the target path).
	KindEdit = "edit"
	// KindTestFailure is the event kind marking a failing test invocation.
	KindTestFailure = "test_failure"
	// KindToolCall is the event kind marking a tool invocation (Content carries
	// the tool name + arguments payload).
	KindToolCall = "tool"
)

// Disposition is the recommended runtime action when a loop is detected.
type Disposition string

// Disposition outcomes chosen when a dimension exceeds its threshold.
const (
	// DispositionWarn logs the loop and continues, leaving the decision to the
	// operator / orchestrator.
	DispositionWarn Disposition = "warn"
	// DispositionTerminate asks the runtime to abort the current loop.
	DispositionTerminate Disposition = "terminate"
	// DispositionSteer asks the runtime to steer the agent toward a different
	// direction (e.g. alter prompting or tool selection).
	DispositionSteer Disposition = "steer"
)

// String returns the canonical string form of the disposition.
func (d Disposition) String() string { return string(d) }

// LoopDetectionConfig tunes the thresholds and disposition of a LoopDetector.
type LoopDetectionConfig struct {
	// EditThreshold triggers DimensionEditCount once a single file has been
	// edited this many times.
	EditThreshold int
	// TestFailureThreshold triggers DimensionTestFailure after this many
	// consecutive test failures.
	TestFailureThreshold int
	// SameToolCallThreshold triggers DimensionSameToolCall after this many
	// repeated identical tool invocations.
	SameToolCallThreshold int
	// Disposition is the action recommended when any dimension triggers.
	Disposition Disposition

	// EditKind, TestFailureKind and ToolCallKind classify incoming event
	// kinds. Leave zero-valued to accept the package defaults.
	EditKind        string
	TestFailureKind string
	ToolCallKind    string
}

// LoopDetector watches an agent event stream for loop-indicating patterns and
// reports whether the runtime is stuck.
type LoopDetector interface {
	// Observe ingests an agent event and updates the relevant counters.
	Observe(ctx context.Context, event core.AgentEvent) error
	// Check reports whether a loop threshold has been exceeded.
	Check(ctx context.Context) LoopDetectionResult
	// Reset clears all tracked counters.
	Reset(ctx context.Context) error
	// Name returns the detector identifier.
	Name() string
}

// LoopDetectionResult is the outcome of a loop check.
type LoopDetectionResult struct {
	// Detected reports whether a loop was detected.
	Detected bool
	// Dimension is the identifier that exceeded its threshold ("", if none).
	Dimension string
	// Count is the observed count for the offending dimension.
	Count int
	// Threshold is the configured threshold for that dimension.
	Threshold int
	// Message is a human-readable summary ("" when nothing is detected).
	Message string
	// Disposition is the recommended action when Detected is true.
	Disposition Disposition
}

// DefaultLoopDetector is the default LoopDetector. It tracks per-file edit
// counts, a consecutive test-failure count, and consecutive identical tool
// invocations under a single read-write lock.
type DefaultLoopDetector struct {
	mu   sync.RWMutex
	cfg  LoopDetectionConfig
	name string

	editCounts   map[string]int
	testFailures int

	lastToolKey string
	toolCount   int
}

// Compile-time assertion that DefaultLoopDetector satisfies LoopDetector.
var _ LoopDetector = (*DefaultLoopDetector)(nil)

// NewDefaultLoopDetector returns a DefaultLoopDetector backed by cfg, filling
// in sensible defaults for any zero-valued fields.
func NewDefaultLoopDetector(cfg LoopDetectionConfig, opts ...Option) LoopDetector {
	applyDefaults(&cfg)
	o := applyOptions(opts)
	name := o.name
	if name == "" {
		name = "loop-detector"
	}
	return &DefaultLoopDetector{
		cfg:        cfg,
		name:       name,
		editCounts: make(map[string]int),
	}
}

// applyDefaults fills zero-valued threshold/kind/name fields with safe defaults.
func applyDefaults(cfg *LoopDetectionConfig) {
	if cfg.EditThreshold <= 0 {
		cfg.EditThreshold = 5
	}
	if cfg.TestFailureThreshold <= 0 {
		cfg.TestFailureThreshold = 3
	}
	if cfg.SameToolCallThreshold <= 0 {
		cfg.SameToolCallThreshold = 3
	}
	if cfg.Disposition == "" {
		cfg.Disposition = DispositionWarn
	}
	if cfg.EditKind == "" {
		cfg.EditKind = KindEdit
	}
	if cfg.TestFailureKind == "" {
		cfg.TestFailureKind = KindTestFailure
	}
	if cfg.ToolCallKind == "" {
		cfg.ToolCallKind = KindToolCall
	}
}

// Observe updates per-dimension counters based on the event kind.
func (d *DefaultLoopDetector) Observe(_ context.Context, event core.AgentEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch event.Kind {
	case d.cfg.EditKind:
		if event.Content == "" {
			return nil
		}
		d.editCounts[event.Content]++
	case d.cfg.TestFailureKind:
		d.testFailures++
	case d.cfg.ToolCallKind:
		key := toolCallKey(event.Content)
		if key == d.lastToolKey && d.lastToolKey != "" {
			d.toolCount++
		} else {
			d.lastToolKey = key
			d.toolCount = 1
		}
	default:
		// Unrecognized events are ignored.
		return nil
	}
	return nil
}

// toolCallKey hashes the tool-call payload (tool name + arguments) to a
// stable, collision-safe key so repeated identical calls share one counter.
func toolCallKey(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])[:24]
}

// Check reports the first dimension whose threshold is exceeded.
func (d *DefaultLoopDetector) Check(ctx context.Context) LoopDetectionResult {
	span, ctx := tracing.SpanFromContext(ctx, "production.loop_detect", tracing.SpanKindInternal)
	defer span.End()

	d.mu.RLock()
	defer d.mu.RUnlock()

	if res, ok := d.checkEditCount(); ok {
		d.emitDetection(ctx, span, res)
		return res
	}
	if res, ok := d.checkTestFailure(ctx, span); ok {
		return res
	}
	if res, ok := d.checkSameTool(ctx, span); ok {
		return res
	}

	return LoopDetectionResult{Detected: false}
}

// checkEditCount returns a detection when a single file has been edited past
// its threshold.
func (d *DefaultLoopDetector) checkEditCount() (LoopDetectionResult, bool) {
	if d.cfg.EditThreshold <= 0 || len(d.editCounts) == 0 {
		return LoopDetectionResult{}, false
	}
	maxFile := ""
	maxCount := 0
	for file, count := range d.editCounts {
		if count > maxCount {
			maxFile, maxCount = file, count
		}
	}
	if maxCount >= d.cfg.EditThreshold {
		return LoopDetectionResult{
			Detected:    true,
			Dimension:   DimensionEditCount,
			Count:       maxCount,
			Threshold:   d.cfg.EditThreshold,
			Disposition: d.cfg.Disposition,
			Message:     fmt.Sprintf("file %q edited %d times (threshold %d)", maxFile, maxCount, d.cfg.EditThreshold),
		}, true
	}
	return LoopDetectionResult{}, false
}

// checkTestFailure returns a detection when consecutive test failures exceed
// the threshold.
func (d *DefaultLoopDetector) checkTestFailure(ctx context.Context, span tracing.TraceSpan) (LoopDetectionResult, bool) {
	if d.cfg.TestFailureThreshold > 0 && d.testFailures >= d.cfg.TestFailureThreshold {
		res := LoopDetectionResult{
			Detected:    true,
			Dimension:   DimensionTestFailure,
			Count:       d.testFailures,
			Threshold:   d.cfg.TestFailureThreshold,
			Disposition: d.cfg.Disposition,
			Message:     fmt.Sprintf("%d consecutive test failures (threshold %d)", d.testFailures, d.cfg.TestFailureThreshold),
		}
		d.emitDetection(ctx, span, res)
		return res, true
	}
	return LoopDetectionResult{}, false
}

// checkSameTool returns a detection when the same tool call repeats past its
// threshold.
func (d *DefaultLoopDetector) checkSameTool(ctx context.Context, span tracing.TraceSpan) (LoopDetectionResult, bool) {
	if d.cfg.SameToolCallThreshold > 0 && d.toolCount >= d.cfg.SameToolCallThreshold {
		res := LoopDetectionResult{
			Detected:    true,
			Dimension:   DimensionSameToolCall,
			Count:       d.toolCount,
			Threshold:   d.cfg.SameToolCallThreshold,
			Disposition: d.cfg.Disposition,
			Message:     fmt.Sprintf("identical tool call repeated %d times (threshold %d)", d.toolCount, d.cfg.SameToolCallThreshold),
		}
		d.emitDetection(ctx, span, res)
		return res, true
	}
	return LoopDetectionResult{}, false
}

// emitDetection records the loop detection on the span and logs it.
func (d *DefaultLoopDetector) emitDetection(ctx context.Context, span tracing.TraceSpan, res LoopDetectionResult) {
	span.SetAttributes(
		tracing.Attribute{Key: "dimension", Value: res.Dimension},
		tracing.Attribute{Key: "count", Value: res.Count},
		tracing.Attribute{Key: "threshold", Value: res.Threshold},
	)
	logger := tracing.NewTraceLogger(span, slog.Default())
	logger.WarnContext(ctx, "loop_detected",
		"dimension", res.Dimension,
		"count", res.Count,
		"threshold", res.Threshold,
		"message", res.Message,
	)
}

// Reset clears all tracked counters.
func (d *DefaultLoopDetector) Reset(_ context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.editCounts = make(map[string]int)
	d.testFailures = 0
	d.lastToolKey = ""
	d.toolCount = 0
	return nil
}

// Name returns the detector identifier.
func (d *DefaultLoopDetector) Name() string { return d.name }
