package llm

import (
	"context"
	"encoding/json"
)

// Stream payloads and event counts are bounded at every retained streaming
// layer. The core loop also uses these limits for guarded candidates.
const (
	GuardedStreamMaxBytes     = 400000
	GuardedStreamMaxChunks    = 16384
	GuardedStreamMaxToolCalls = 4096
)

type streamContinuationOwnerKey struct{}
type noReplayContinuationKey struct{}

// WithLoopOwnedContinuation marks a stream whose continuation chain is owned
// by the core loop. Middleware must not start a second continuation chain.
func WithLoopOwnedContinuation(ctx context.Context) context.Context {
	return context.WithValue(ctx, streamContinuationOwnerKey{}, true)
}

// LoopOwnsContinuation reports whether the caller owns length continuation.
func LoopOwnsContinuation(ctx context.Context) bool {
	owned, _ := ctx.Value(streamContinuationOwnerKey{}).(bool)
	return owned
}

// WithContinuationNoReplay marks a provider request that continues an
// incomplete candidate. Retrying, falling back, or starting another
// middleware-owned continuation would replay that candidate request.
func WithContinuationNoReplay(ctx context.Context) context.Context {
	return context.WithValue(ctx, noReplayContinuationKey{}, true)
}

// ContinuationNoReplay reports whether ctx is for one continuation request
// that must not be replayed by nested model middleware.
func ContinuationNoReplay(ctx context.Context) bool {
	noReplay, _ := ctx.Value(noReplayContinuationKey{}).(bool)
	return noReplay
}

func addBoundedStreamBytes(current, additional int) (int, bool) {
	if current < 0 || additional < 0 || current > GuardedStreamMaxBytes || additional > GuardedStreamMaxBytes-current {
		return current, false
	}
	return current + additional, true
}

// RetainedStreamChunkTextBytes returns the UTF-8 bytes appended to the
// logical candidate by this fragment. Per-chunk role/final/usage wrappers
// are not retained independently and are accounted with the final message.
func RetainedStreamChunkTextBytes(chunk MessageChunk) int {
	return len(chunk.Content)
}

// RetainedMessagePayloadBytes measures one complete logical message exactly
// once. Stream chunk framing and repeated per-chunk metadata are deliberately
// excluded; callers separately bound chunk count and accumulated text bytes.
func RetainedMessagePayloadBytes(message *Message) (int, error) {
	encoded, err := json.Marshal(message)
	if err != nil {
		return 0, err
	}
	return len(encoded), nil
}
