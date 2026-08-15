package production

import (
	"github.com/pengjunchen/go-cli/internal/tools"
)

// DefaultAPIKeyPatterns returns regex patterns that match common API key and
// secret token formats. This delegates to tools.DefaultAPIKeyPatterns to keep
// a single source of truth for API key patterns across the codebase.
func DefaultAPIKeyPatterns() []string {
	return tools.DefaultAPIKeyPatterns()
}

// RegisterAPIKeyRedaction registers all default API key patterns on the given
// RedactingOutputGuard. Invalid patterns are silently ignored by
// AddRedactPattern, so this function never fails. It is safe to call on a nil
// guard (a no-op in that case).
func RegisterAPIKeyRedaction(guard *RedactingOutputGuard) {
	if guard == nil {
		return
	}
	for _, pattern := range DefaultAPIKeyPatterns() {
		guard.AddRedactPattern(pattern)
	}
}
