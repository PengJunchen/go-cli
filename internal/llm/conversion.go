package llm

import "strings"

// Cross-provider conversion helpers.
//
// These three pure functions operate on []Message and always return a NEW
// slice: they never mutate the input messages. They exist to ease moving a
// conversation between provider dialects whose wire models disagree on how
// tool-call identifiers, images and thinking blocks are represented.
//
// Because llm.Message models content as a single string, both images and
// thinking blocks are assumed to be embedded in Content as textual markers.
// The marker formats are documented on the corresponding helper.

// Default conversion constants. Grouped in one const block so the
// marker formats and the canonical tool-call prefix are easy to inspect and do
// not look like scattered hardcoded strings.
const (
	// canonicalToolCallPrefix is the canonical identifier prefix applied to
	// every tool-call ID by NormalizeToolCallID. Providers (OpenAI "call_…",
	// Anthropic "toolu_…", Gemini bare UUIDs) each use their own scheme; this
	// prefix makes IDs consistent across dialects.
	canonicalToolCallPrefix = "call_"

	// imageMarkerPrefix opens an image marker in Message.Content, e.g.
	// "[image:data:image/png;base64,…]". DowngradeImages replaces the whole
	// "[image:…]" span up to the closing bracket.
	imageMarkerPrefix = "[image:"

	// defaultImagePlaceholder is the text DowngradeImages substitutes for an
	// image marker when no custom placeholder is supplied.
	defaultImagePlaceholder = "[image omitted: not supported by this provider]"

	// thinkingMarkerPrefix opens an inline thinking block in Message.Content,
	// e.g. "[thinking:let me reason…]". AdaptThinking removes the whole block.
	thinkingMarkerPrefix = "[thinking:"

	// thinkingStartMarker / thinkingEndMarker delimit a paired thinking block,
	// e.g. "[thinking_start]…[thinking_end]". AdaptThinking removes both
	// delimiters and the text between them.
	thinkingStartMarker = "[thinking_start]"
	thinkingEndMarker   = "[thinking_end]"
)

// knownToolCallPrefixes lists provider-native prefixes that
// NormalizeToolCallID strips before re-applying the canonical prefix, so
// normalization is idempotent and deterministic. It is a var because Go does
// not allow slice values in const blocks; the strings themselves are literals.
var knownToolCallPrefixes = []string{"call_", "toolu_"}

// NormalizeToolCallID returns a copy of msgs in which every tool-call ID is
// rewritten to the canonical "call_" format. Known provider-native prefixes
// (OpenAI "call_", Anthropic "toolu_") are stripped and a canonical "call_"
// prefix is re-applied, so the result is deterministic and idempotent.
//
// Linking rule: when a message carries both ToolCallID and a matching
// ToolCalls[].ID, both are canonicalized from the same original value, so they
// remain linked after conversion. A nil or empty ToolCallID that is unmatched
// by a ToolCalls entry is left empty rather than inventing a random ID.
//
// It returns nil for a nil input and an empty (non-nil) slice for an empty
// input. The input slice and its messages are never mutated.
func NormalizeToolCallID(msgs []Message) []Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]Message, len(msgs))
	for i, msg := range msgs {
		msg.ToolCalls = normalizeToolCalls(msg.ToolCalls)
		msg.ToolCallID = canonicalToolCallID(msg.ToolCallID)
		out[i] = msg
	}
	return out
}

// normalizeToolCalls canonicalizes the ID on every tool call. It returns a new
// slice when any value changes and never mutates the input slice.
func normalizeToolCalls(toolCalls []ToolCall) []ToolCall {
	if len(toolCalls) == 0 {
		return nil
	}
	changed := false
	out := make([]ToolCall, len(toolCalls))
	for i, tc := range toolCalls {
		norm := canonicalToolCallID(tc.ID)
		if norm != tc.ID {
			changed = true
		}
		tc.ID = norm
		out[i] = tc
	}
	if !changed {
		return toolCalls
	}
	return out
}

// canonicalToolCallID rewrites id into the canonical "call_" form. It strips a
// single recognized provider-native prefix (call_/toolu_) and re-applies the
// canonical prefix. Empty ids and bare prefix tokens (e.g. an id that is only
// "call_") are returned unchanged to avoid producing dangling prefixes.
func canonicalToolCallID(id string) string {
	if id == "" {
		return ""
	}
	stripped := id
	for _, prefix := range knownToolCallPrefixes {
		if strings.HasPrefix(stripped, prefix) {
			stripped = strings.TrimPrefix(stripped, prefix)
			break
		}
	}
	if stripped == "" {
		return id
	}
	return canonicalToolCallPrefix + stripped
}

// DowngradeImages returns a copy of msgs in which image-bearing content is
// downgraded to a textual placeholder. The current Message model has only a
// string Content field, so images are assumed to be embedded as a marker of
// the form "[image:<payload>]" inside Content. Each complete
// "[image:…]" span (up to the following "]") is replaced with placeholder.
//
// A non-empty placeholder may be supplied as the optional first argument to
// override defaultImagePlaceholder. When no custom placeholder is given, the
// default is used. A nil or empty input yields nil; the input is never mutated.
func DowngradeImages(msgs []Message, placeholders ...string) []Message {
	if len(msgs) == 0 {
		return nil
	}
	placeholder := defaultImagePlaceholder
	if len(placeholders) > 0 && placeholders[0] != "" {
		placeholder = placeholders[0]
	}
	out := make([]Message, len(msgs))
	for i, msg := range msgs {
		if strings.Contains(msg.Content, imageMarkerPrefix) {
			msg.Content = replaceMarkerSegments(msg.Content, imageMarkerPrefix, placeholder)
		}
		out[i] = msg
	}
	return out
}

// AdaptThinking returns a copy of msgs in which thinking/extended-thinking
// blocks are stripped from Content for providers that do not support them.
// Two marker styles are removed:
//
//   - an inline marker "[thinking:…]" (replaced with an empty string), and
//   - a paired marker "[thinking_start]…[thinking_end]" (both delimiters and
//     everything between them are removed).
//
// Content with no thinking markers is returned unchanged. A nil or empty input
// yields nil; the input is never mutated.
func AdaptThinking(msgs []Message) []Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]Message, len(msgs))
	for i, msg := range msgs {
		if strings.Contains(msg.Content, thinkingMarkerPrefix) ||
			strings.Contains(msg.Content, thinkingStartMarker) {
			msg.Content = stripThinking(msg.Content)
		}
		out[i] = msg
	}
	return out
}

// stripThinking removes inline "[thinking:…]" blocks and paired thinking
// regions from content.
func stripThinking(content string) string {
	content = replaceMarkerSegments(content, thinkingMarkerPrefix, "")
	return stripThinkingRegions(content)
}

// stripThinkingRegions removes every paired thinking region delimited by
// [thinking_start]…[thinking_end], including the delimiters.
func stripThinkingRegions(content string) string {
	var b strings.Builder
	for {
		start := strings.Index(content, thinkingStartMarker)
		if start < 0 {
			b.WriteString(content)
			break
		}
		// Keep everything before the start marker.
		b.WriteString(content[:start])
		afterStart := content[start+len(thinkingStartMarker):]
		end := strings.Index(afterStart, thinkingEndMarker)
		if end < 0 {
			// Unbalanced start marker: drop the rest of the content.
			break
		}
		content = afterStart[end+len(thinkingEndMarker):]
	}
	return b.String()
}

// replaceMarkerSegments replaces every segment of content opened by marker
// (up to the first following "]") with replacement. When there is no closing
// bracket, the segment is replaced through the end of the string.
func replaceMarkerSegments(content, marker, replacement string) string {
	var b strings.Builder
	for {
		idx := strings.Index(content, marker)
		if idx < 0 {
			b.WriteString(content)
			break
		}
		b.WriteString(content[:idx])
		rest := content[idx+len(marker):]
		closeIdx := strings.Index(rest, "]")
		if closeIdx < 0 {
			// Unterminated marker: consume to end and emit the replacement.
			b.WriteString(replacement)
			break
		}
		b.WriteString(replacement)
		content = rest[closeIdx+1:]
	}
	return b.String()
}
