package compaction

import (
	"context"
	"sync"
)

// recordingCompactor is a configurable Compactor fake that records the order in
// which it was consulted and returns a canned result or error.
type recordingCompactor struct {
	mu    sync.Mutex
	name  string
	out   []TurnItem
	err   error
	calls []string
}

// Compile-time assertion that recordingCompactor satisfies Compactor.
var _ Compactor = (*recordingCompactor)(nil)

// NewRecordingCompactor returns a fake compactor recording calls under name.
func NewRecordingCompactor(name string) *recordingCompactor {
	return &recordingCompactor{name: name}
}

// WithResult configures the canned success result.
func (r *recordingCompactor) WithResult(out []TurnItem) *recordingCompactor {
	r.out = out
	return r
}

// WithError configures the canned error.
func (r *recordingCompactor) WithError(err error) *recordingCompactor {
	r.err = err
	return r
}

// Compact records the call and returns the configured result.
func (r *recordingCompactor) Compact(_ context.Context, _ []TurnItem, _ int, _ TokenEstimator) ([]TurnItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, r.name)
	if r.err != nil {
		return nil, r.err
	}
	return r.out, nil
}

// Called reports the ordered names of every compactor invoked.
func (r *recordingCompactor) Called() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}
