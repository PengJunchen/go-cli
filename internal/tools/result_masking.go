package tools

import (
	"log/slog"
	"regexp"
	"sync"
)

// DefaultMaskPatterns are regex patterns for common sensitive data. They match
// API keys, GitHub tokens, password fields in JSON, and credit card numbers.
var DefaultMaskPatterns = []string{
	`(sk-[a-zA-Z0-9]{20,})`,                        // API keys
	`(ghp_[a-zA-Z0-9]{36,})`,                       // GitHub tokens
	`("[^"]*password[^"]*"\s*:\s*")[^"]*(")`,       // Password fields
	`(\b\d{4}[\s-]?\d{4}[\s-]?\d{4}[\s-]?\d{4}\b)`, // Credit card numbers
}

// defaultMask is the replacement text used when no explicit mask is set.
const defaultMask = "[REDACTED]"

// ResultMasker masks sensitive information in tool results. Patterns are
// compiled once at construction and the masker is safe for concurrent use.
type ResultMasker struct {
	mu       sync.RWMutex
	patterns []*regexp.Regexp
	mask     string
}

// NewResultMasker compiles the given patterns into a ResultMasker. If patterns
// is nil or empty, DefaultMaskPatterns is used. The default mask is
// "[REDACTED]".
func NewResultMasker(patterns []string) *ResultMasker {
	if len(patterns) == 0 {
		patterns = DefaultMaskPatterns
	}
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			slog.Debug("tools.result_masker.compile_failed", "pattern", p, "err", err)
			continue
		}
		compiled = append(compiled, re)
	}
	return &ResultMasker{
		patterns: compiled,
		mask:     defaultMask,
	}
}

// Mask replaces every match of every compiled pattern in content with the
// configured mask string. For patterns with two or more capture groups, only
// the sensitive middle portion is replaced, preserving surrounding context
// (e.g. JSON keys and delimiters).
func (m *ResultMasker) Mask(content string) string {
	m.mu.RLock()
	mask := m.mask
	patterns := m.patterns
	m.mu.RUnlock()

	if len(patterns) == 0 {
		return content
	}

	result := content
	for _, re := range patterns {
		if re.NumSubexp() >= 2 {
			// Preserve group 1 (prefix) and group 2 (suffix), masking only
			// the sensitive text between them.
			result = re.ReplaceAllStringFunc(result, func(match string) string {
				subs := re.FindStringSubmatch(match)
				if len(subs) >= 3 {
					return subs[1] + mask + subs[2]
				}
				return mask
			})
		} else {
			result = re.ReplaceAllStringFunc(result, func(string) string { return mask })
		}
	}
	slog.Debug("tools.result_masker.mask", "patterns", len(patterns), "changed", result != content)
	return result
}

// SetMask overrides the replacement string used by Mask.
func (m *ResultMasker) SetMask(mask string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mask == "" {
		mask = defaultMask
	}
	m.mask = mask
}
