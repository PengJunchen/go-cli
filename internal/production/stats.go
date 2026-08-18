package production

import (
	"log/slog"
	"sync"
	"time"
)

// SessionStats tracks per-session statistics.
type SessionStats struct {
	// SessionID identifies the session these stats belong to.
	SessionID string
	// Turns is the number of agent turns completed.
	Turns int
	// ToolCalls is the number of tool invocations performed.
	ToolCalls int
	// TokensIn is the cumulative input tokens consumed.
	TokensIn int
	// TokensOut is the cumulative output tokens produced.
	TokensOut int
	// Cost is the accumulated monetary cost in USD.
	Cost float64
	// Duration is the total wall-clock time spent in the session.
	Duration time.Duration
	// Errors is the number of errors encountered.
	Errors int
}

// StatsRegistry collects stats across sessions. It is safe for concurrent
// use.
type StatsRegistry struct {
	mu    sync.RWMutex
	stats map[string]*SessionStats
}

// Compile-time assertion that StatsRegistry can be instantiated.
var _ = NewStatsRegistry

// NewStatsRegistry returns an empty StatsRegistry.
func NewStatsRegistry() *StatsRegistry {
	return &StatsRegistry{stats: make(map[string]*SessionStats)}
}

// GetOrCreate returns the SessionStats for sessionID, creating an empty entry
// if one does not yet exist. It returns a defensive copy so callers cannot
// race with internal Record* methods.
func (r *StatsRegistry) GetOrCreate(sessionID string) *SessionStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stats[sessionID]
	if !ok {
		s = &SessionStats{SessionID: sessionID}
		r.stats[sessionID] = s
		slog.Debug("production.stats.create", "session", sessionID)
	}
	cp := *s
	return &cp
}

// RecordTurn increments the turn counter for the given session.
func (r *StatsRegistry) RecordTurn(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stats[sessionID]
	if !ok {
		s = &SessionStats{SessionID: sessionID}
		r.stats[sessionID] = s
	}
	s.Turns++
	slog.Debug("production.stats.turn", "session", sessionID, "turns", s.Turns)
}

// RecordToolCall increments the tool-call counter for the given session.
func (r *StatsRegistry) RecordToolCall(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stats[sessionID]
	if !ok {
		s = &SessionStats{SessionID: sessionID}
		r.stats[sessionID] = s
	}
	s.ToolCalls++
	slog.Debug("production.stats.tool_call", "session", sessionID, "tool_calls", s.ToolCalls)
}

// RecordTokens adds input and output token counts to the session totals.
func (r *StatsRegistry) RecordTokens(sessionID string, in, out int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.stats[sessionID]
	if !ok {
		s = &SessionStats{SessionID: sessionID}
		r.stats[sessionID] = s
	}
	s.TokensIn += in
	s.TokensOut += out
	slog.Debug("production.stats.tokens",
		"session", sessionID,
		"tokens_in", s.TokensIn,
		"tokens_out", s.TokensOut,
	)
}

// GetSessionStats returns a copy of the stats for the given session and
// whether it existed.
func (r *StatsRegistry) GetSessionStats(sessionID string) (*SessionStats, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.stats[sessionID]
	if !ok {
		return nil, false
	}
	cp := *s
	return &cp, ok
}

// GetAll returns a snapshot of all session stats keyed by session ID. The
// returned map and all SessionStats values are copies and may be modified
// freely.
func (r *StatsRegistry) GetAll() map[string]*SessionStats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*SessionStats, len(r.stats))
	for k, v := range r.stats {
		cp := *v
		out[k] = &cp
	}
	return out
}
