// Package verify provides the verification framework for go-cli.
// It implements log-based process verification, AST scanning for mock/hardcoded
// bypass detection, and Go-specific safety checks (goroutine leaks, race conditions).
package verify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// TestingT is the minimal subset of *testing.T used by verify assertions.
// Tests pass *testing.T directly; this interface enables reuse outside testing.
type TestingT interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// LogEntry represents a single captured log entry.
type LogEntry struct {
	// Level is the slog level of the entry.
	Level slog.Level
	// Message is the human-readable log message.
	Message string
	// Fields holds the structured key-value pairs attached to the entry.
	Fields map[string]any
	// Time is the timestamp recorded by slog when the entry was emitted.
	Time time.Time
}

// LogCapturer captures all log output for test assertions.
// It replaces the default slog handler with an in-memory capture handler,
// ensuring tests verify real runtime behavior through log output rather than mocks.
//
// Warning: Attach/Detach modifies the global slog default logger.
// Do NOT use with t.Parallel() — tests sharing the global logger will interfere.
// For parallel tests, pass a custom logger via context instead.
type LogCapturer struct {
	mu      sync.Mutex
	entries []LogEntry
	handler slog.Handler
	prev    slog.Handler
}

// NewLogCapturer creates a new LogCapturer that will capture slog output.
func NewLogCapturer() *LogCapturer {
	return &LogCapturer{}
}

// captureHandler is a slog.Handler that records all log entries.
type captureHandler struct {
	capturer *LogCapturer
	level    slog.Level
	attrs    []slog.Attr // pre-set attrs from WithAttrs
	group    string      // group prefix from WithGroup
}

// Enabled reports whether the handler handles records at the given level.
func (h *captureHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

// Handle processes a log record by storing it as a LogEntry.
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	entry := LogEntry{
		Level:   r.Level,
		Message: r.Message,
		Fields:  make(map[string]any),
		Time:    r.Time,
	}

	// Add pre-set attrs from WithAttrs first.
	for _, a := range h.attrs {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		entry.Fields[key] = a.Value.Any()
	}

	// Add record-specific attrs.
	r.Attrs(func(a slog.Attr) bool {
		key := a.Key
		if h.group != "" {
			key = h.group + "." + key
		}
		entry.Fields[key] = a.Value.Any()
		return true
	})

	h.capturer.mu.Lock()
	h.capturer.entries = append(h.capturer.entries, entry)
	h.capturer.mu.Unlock()

	return nil
}

// WithAttrs returns a new handler with the given attributes added.
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	newAttrs = append(newAttrs, h.attrs...)
	newAttrs = append(newAttrs, attrs...)
	return &captureHandler{
		capturer: h.capturer,
		level:    h.level,
		attrs:    newAttrs,
		group:    h.group,
	}
}

// WithGroup returns a new handler with the given group name prepended to attribute keys.
func (h *captureHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	newGroup := name
	if h.group != "" {
		newGroup = h.group + "." + name
	}
	return &captureHandler{
		capturer: h.capturer,
		level:    h.level,
		attrs:    h.attrs,
		group:    newGroup,
	}
}

// Attach replaces the default slog handler with a capture handler and returns
// the context unchanged. The caller must call Detach to restore the original handler.
//
// Note: This modifies global state. Do not use with t.Parallel().
func (c *LogCapturer) Attach(ctx context.Context) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If already attached, detach first to preserve the original handler.
	if c.prev != nil {
		slog.SetDefault(slog.New(c.prev))
	}

	c.prev = slog.Default().Handler()
	ch := &captureHandler{
		capturer: c,
		level:    slog.LevelDebug,
	}
	c.handler = ch

	logger := slog.New(ch)
	slog.SetDefault(logger)

	return ctx
}

// Detach restores the original slog handler.
func (c *LogCapturer) Detach() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.prev != nil {
		slog.SetDefault(slog.New(c.prev))
		c.prev = nil
	}
}

// Entries returns all captured log entries. The slice is a copy; the caller
// may safely iterate without holding a lock.
func (c *LogCapturer) Entries() []LogEntry {
	c.mu.Lock()
	defer c.mu.Unlock()

	result := make([]LogEntry, len(c.entries))
	copy(result, c.entries)
	return result
}

// Reset clears all captured entries.
func (c *LogCapturer) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

// Count returns the number of captured entries.
func (c *LogCapturer) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// LogMatch defines criteria for matching log entries in assertions.
type LogMatch struct {
	// Op matches the "op" field of the log entry.
	Op string
	// Level matches the log level when HasLevel is true.
	Level slog.Level
	// HasLevel must be set to true to enable level matching.
	HasLevel bool
	// Fields matches all specified keys as a subset of the entry's fields.
	Fields map[string]any
}

// AssertLogEntry asserts that at least one captured entry matches the criteria.
// It returns the matching entry if found.
func AssertLogEntry(t TestingT, entries []LogEntry, match LogMatch) LogEntry {
	t.Helper()

	for _, entry := range entries {
		if matchLogEntry(entry, match) {
			return entry
		}
	}

	t.Fatalf("no log entry matched criteria: op=%q hasLevel=%v level=%s fields=%v\nCaptured %d entries:\n%s",
		match.Op, match.HasLevel, match.Level, match.Fields, len(entries), formatEntries(entries))
	return LogEntry{}
}

// AssertLogSequence asserts that the captured entries contain a subsequence
// matching the given criteria in order.
func AssertLogSequence(t TestingT, entries []LogEntry, matches []LogMatch) {
	t.Helper()

	idx := 0
	for _, match := range matches {
		found := false
		for i := idx; i < len(entries); i++ {
			if matchLogEntry(entries[i], match) {
				idx = i + 1
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("log sequence mismatch: could not find match for op=%q after index %d\nCaptured entries:\n%s",
				match.Op, idx, formatEntries(entries))
		}
	}
}

// AssertNoLogEntry asserts that no captured entry matches the criteria.
func AssertNoLogEntry(t TestingT, entries []LogEntry, match LogMatch) {
	t.Helper()

	for _, entry := range entries {
		if matchLogEntry(entry, match) {
			t.Fatalf("unexpected log entry matched criteria: op=%q fields=%v\nFull entry: %s",
				match.Op, match.Fields, formatEntry(entry))
		}
	}
}

func matchLogEntry(entry LogEntry, match LogMatch) bool {
	if match.Op != "" {
		op, ok := entry.Fields["op"].(string)
		if !ok || op != match.Op {
			return false
		}
	}

	if match.HasLevel && entry.Level != match.Level {
		return false
	}

	for k, v := range match.Fields {
		ev, ok := entry.Fields[k]
		if !ok || ev != v {
			return false
		}
	}

	return true
}

func formatEntries(entries []LogEntry) string {
	var sb strings.Builder
	for i, e := range entries {
		sb.WriteString(formatEntry(e))
		if i < len(entries)-1 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func formatEntry(e LogEntry) string {
	var sb strings.Builder
	sb.WriteString(e.Level.String())
	sb.WriteString(": ")
	sb.WriteString(e.Message)
	for k, v := range e.Fields {
		sb.WriteString(" ")
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(fmt.Sprintf("%v", v))
	}
	return sb.String()
}
