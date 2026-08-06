package tools

import (
	"fmt"
	"log/slog"
	"strings"
)

// TruncateStrategy is the contract for truncating content to a bounded size.
// The interpretation of limit depends on the concrete strategy (e.g. characters
// for TruncateHead/TruncateTail, lines for TruncateLine).
type TruncateStrategy interface {
	// Truncate shortens content so that it fits within limit, appending or
	// prepending a marker to indicate that content was removed.
	Truncate(content string, limit int) string
}

// TruncateHead keeps the first limit characters of content and appends a
// truncation marker. When content already fits, it is returned unchanged.
type TruncateHead struct{}

var _ TruncateStrategy = TruncateHead{}

// Truncate keeps the first limit characters and appends "... [truncated]".
func (TruncateHead) Truncate(content string, limit int) string {
	if limit <= 0 || len(content) <= limit {
		return content
	}
	return content[:limit] + "... [truncated]"
}

// TruncateTail keeps the last limit characters of content and prepends a
// truncation marker. When content already fits, it is returned unchanged.
type TruncateTail struct{}

var _ TruncateStrategy = TruncateTail{}

// Truncate keeps the last limit characters and prepends "... [truncated] ...".
func (TruncateTail) Truncate(content string, limit int) string {
	if limit <= 0 || len(content) <= limit {
		return content
	}
	return "... [truncated] ..." + content[len(content)-limit:]
}

// TruncateLine keeps the first limit lines of content and appends a marker
// indicating how many lines were removed. When content already fits, it is
// returned unchanged.
type TruncateLine struct{}

var _ TruncateStrategy = TruncateLine{}

// Truncate keeps the first limit lines and appends "... [N lines truncated]".
func (TruncateLine) Truncate(content string, limit int) string {
	if limit <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= limit {
		return content
	}
	truncated := len(lines) - limit
	kept := strings.Join(lines[:limit], "\n")
	return kept + fmt.Sprintf("\n... [%d lines truncated]", truncated)
}

// TruncateConfig configures a Truncater.
type TruncateConfig struct {
	// Strategy is the truncation strategy to apply. When nil, no truncation
	// occurs.
	Strategy TruncateStrategy
	// Limit is the maximum size passed to the strategy.
	Limit int
	// Enabled controls whether truncation is applied at all.
	Enabled bool
}

// Truncater applies the configured truncation strategy to content.
type Truncater struct {
	config TruncateConfig
}

// NewTruncater returns a Truncater with the given configuration.
func NewTruncater(cfg TruncateConfig) *Truncater {
	return &Truncater{config: cfg}
}

// Apply truncates content according to the configured strategy, limit, and
// enabled flag. When truncation is disabled, the strategy is nil, or the limit
// is non-positive, content is returned unchanged.
func (tr *Truncater) Apply(content string) string {
	if !tr.config.Enabled || tr.config.Strategy == nil || tr.config.Limit <= 0 {
		return content
	}

	result := tr.config.Strategy.Truncate(content, tr.config.Limit)
	if result != content {
		slog.Debug("truncater.applied",
			"strategy", fmt.Sprintf("%T", tr.config.Strategy),
			"limit", tr.config.Limit,
			"input_len", len(content),
			"output_len", len(result))
	}
	return result
}
