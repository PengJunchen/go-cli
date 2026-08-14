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
//   - GitHub PAT: ghp_/gho_/ghu_/ghs_/ghr_…
//   - GitLab PAT: glpat-…
//   - Slack token: xox[baprs]-…
func DefaultAPIKeyPatterns() []string {
	return []string{
		// Anthropic/Claude API key. The "sk-ant-" prefix is more specific
		// than OpenAI's "sk-" prefix, so it must appear first.
		`sk-ant-[a-zA-Z0-9_-]{20,}`,

		// OpenAI project API key. The "sk-proj-" prefix is more specific
		// than the general "sk-" prefix, so it must appear before it.
		`sk-proj-[a-zA-Z0-9_-]{20,}`,

		// OpenAI API key (legacy and current formats).
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

		// GitHub Personal Access Token (classic). The prefixes ghp_
		// (personal), gho_ (OAuth), ghu_ (server-to-server), ghs_ (bot),
		// and ghr_ (refresh) are followed by 36+ alphanumeric characters.
		`gh[pousr]_[a-zA-Z0-9]{36,}`,

		// GitLab Personal Access Token. The "glpat-" prefix is followed
		// by 20+ alphanumeric characters (including underscores and
		// hyphens).
		`glpat-[a-zA-Z0-9_-]{20,}`,

		// Slack token. The "xox[baprs]-" prefix covers bot (b), app (a),
		// user (p), refresh (r), and session (s) tokens, followed by 10+
		// alphanumeric characters (including hyphens).
		`xox[baprs]-[a-zA-Z0-9-]{10,}`,
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
