package production

// This file provides default API key redaction patterns for the
// RedactingOutputGuard. The patterns cover common cloud-provider and
// LLM-vendor key formats that may leak into model output, connection
// strings, or configuration dumps.
//
// The patterns are intentionally conservative — they require a distinctive
// prefix or keyword (sk-, AIza, AKIA, Bearer, aws_secret_access_key) followed
// by a minimum-length high-entropy body so that ordinary prose is never
// matched.

// DefaultAPIKeyPatterns returns regex patterns that match common API key and
// secret token formats. The patterns are ordered from most-specific to
// least-specific so that when they are registered sequentially on a
// RedactingOutputGuard the more specific prefix (e.g. sk-ant-) is matched
// before the general one (e.g. sk-).
//
// Supported formats:
//   - Anthropic/Claude: sk-ant-…
//   - OpenAI: sk-…
//   - Google/Gemini: AIza…
//   - Generic bearer token: Bearer <token>
//   - AWS Access Key ID: AKIA…
//   - AWS Secret Access Key: aws_secret_access_key=<value>
func DefaultAPIKeyPatterns() []string {
	return []string{
		// Anthropic/Claude API key. The "sk-ant-" prefix is more specific
		// than OpenAI's "sk-" prefix, so it must appear first.
		`sk-ant-[a-zA-Z0-9_-]{20,}`,

		// OpenAI API key.
		`sk-[a-zA-Z0-9]{20,}`,

		// Google/Gemini API key. The "AIza" prefix is followed by exactly
		// 35 characters of base64-url-safe alphabet.
		`AIza[a-zA-Z0-9_-]{35}`,

		// Generic bearer token in an Authorization header.
		`Bearer\s+[a-zA-Z0-9_.-]{20,}`,

		// AWS Access Key ID. The "AKIA" prefix is followed by exactly 16
		// uppercase alphanumeric characters.
		`AKIA[0-9A-Z]{16}`,

		// AWS Secret Access Key in assignment form (e.g. environment
		// variables or config files). The key name is matched
		// case-insensitively, separated by = or :, and the value is a
		// 40-character base64 string.
		`(?i)aws_secret_access_key\s*[=:]\s*[A-Za-z0-9/+=]{40}`,
	}
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
