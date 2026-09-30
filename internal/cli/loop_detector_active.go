package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type loopDetectorRunContextKey struct{}

type loopDetectorRunState struct {
	mu          sync.Mutex
	threshold   int
	disposition production.Disposition
	cancel      context.CancelCauseFunc
	lastKey     [32]byte
	hasLastKey  bool
	count       int
	detection   *production.LoopDetectionResult
}

func newLoopDetectorRunState(cfg production.LoopDetectionConfig, cancel context.CancelCauseFunc) *loopDetectorRunState {
	threshold := cfg.SameToolCallThreshold
	if threshold <= 0 {
		threshold = 3
	}
	disposition := cfg.Disposition
	if disposition == "" {
		disposition = production.DispositionWarn
	}
	return &loopDetectorRunState{threshold: threshold, disposition: disposition, cancel: cancel}
}

func (state *loopDetectorRunState) observe(key [32]byte) (production.LoopDetectionResult, bool) {
	if state == nil {
		return production.LoopDetectionResult{}, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.detection != nil {
		return *state.detection, false
	}
	if state.hasLastKey && state.lastKey == key {
		state.count++
	} else {
		state.lastKey = key
		state.hasLastKey = true
		state.count = 1
	}
	if state.count < state.threshold {
		return production.LoopDetectionResult{}, false
	}
	result := production.LoopDetectionResult{
		Detected:    true,
		Dimension:   production.DimensionSameToolCall,
		Count:       state.count,
		Threshold:   state.threshold,
		Disposition: state.disposition,
		Message:     fmt.Sprintf("identical prepared tool action repeated %d times (threshold %d)", state.count, state.threshold),
	}
	state.detection = &result
	return result, true
}

func (state *loopDetectorRunState) result() (production.LoopDetectionResult, bool) {
	if state == nil {
		return production.LoopDetectionResult{}, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.detection == nil {
		return production.LoopDetectionResult{}, false
	}
	return *state.detection, true
}

func loopDetectorAttemptKey(attempt tools.FrozenPreparedAttempt) [32]byte {
	identity := attempt.ToolIdentity().Snapshot()
	digest := attempt.ArgumentDigest()
	var encoded bytes.Buffer
	encoded.WriteString("go-cli/prepared-tool-loop/v1\x00")
	writeLoopDetectorKeyPart(&encoded, []byte(identity.RegistrationSource))
	writeLoopDetectorKeyPart(&encoded, []byte(identity.ToolName))
	var generation [8]byte
	binary.BigEndian.PutUint64(generation[:], identity.RegistrationGeneration)
	writeLoopDetectorKeyPart(&encoded, generation[:])
	writeLoopDetectorKeyPart(&encoded, []byte(identity.ExecutorFingerprint))
	writeLoopDetectorKeyPart(&encoded, []byte(identity.ScopeFingerprint))
	writeLoopDetectorKeyPart(&encoded, digest[:])
	return sha256.Sum256(encoded.Bytes())
}

func writeLoopDetectorKeyPart(encoded *bytes.Buffer, part []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(part)))
	_, _ = encoded.Write(size[:])
	_, _ = encoded.Write(part)
}

func observePreparedToolAttempt(ctx context.Context, attempt tools.FrozenPreparedAttempt) error {
	state, _ := ctx.Value(loopDetectorRunContextKey{}).(*loopDetectorRunState)
	if state == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	result, newlyDetected := state.observe(loopDetectorAttemptKey(attempt))
	if !newlyDetected {
		return nil
	}
	slog.WarnContext(ctx, "loop_detected",
		"dimension", result.Dimension,
		"count", result.Count,
		"threshold", result.Threshold,
		"disposition", result.Disposition,
		"message", result.Message,
	)
	if result.Disposition != production.DispositionTerminate {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if state.cancel != nil {
		state.cancel(core.ErrLoopDetected)
	}
	if cause := context.Cause(ctx); cause != nil {
		if errors.Is(cause, core.ErrLoopDetected) {
			return core.ErrLoopDetected
		}
		return cause
	}
	return core.ErrLoopDetected
}

func newLoopDetectorRunContext(parent context.Context, cfg production.LoopDetectionConfig) (context.Context, context.CancelCauseFunc, *loopDetectorRunState) {
	runCtx, cancel := context.WithCancelCause(parent)
	state := newLoopDetectorRunState(cfg, cancel)
	runCtx = context.WithValue(runCtx, loopDetectorRunContextKey{}, state)
	return runCtx, cancel, state
}

func normalizeLoopDetectorRunError(ctx context.Context, err error) error {
	if !errors.Is(context.Cause(ctx), core.ErrLoopDetected) {
		return err
	}
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return core.ErrLoopDetected
	}
	if errors.Is(err, core.ErrLoopDetected) {
		return err
	}
	return errors.Join(core.ErrLoopDetected, err)
}

func addLoopDetectorReminder(manager *core.DefaultSystemReminderManager, state *loopDetectorRunState) {
	result, detected := state.result()
	if !detected || result.Disposition == production.DispositionTerminate || manager == nil {
		return
	}
	manager.AddReminder(core.SystemReminder{
		ID:      "loop-detector",
		Content: fmt.Sprintf("Loop detected: %s. Please try a different approach.", result.Message),
	})
}
