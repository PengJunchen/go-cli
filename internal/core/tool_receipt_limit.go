package core

import (
	"unicode/utf8"
)

// Reserve the actual escaped tool-call ID and name, plus a conservative 64 KiB
// for the durable row's remaining fields. The session writer still enforces
// its 1 MiB limit for unusually large remaining envelope fields.
const maxSuccessfulReceiptJSONBytes = (1 << 20) - (64 << 10)
const successfulReceiptTruncatedMarker = "\n[tool output truncated for durable receipt]"

// boundedSuccessfulReceiptOutput measures the JSON string encoding actually
// used by durable sessions, including HTML escaping. Invalid UTF-8 is replaced
// as in JSON before any live or persisted projection. Valid, fitting text is
// unchanged. Source capture and execution outcome are not changed.
func boundedSuccessfulReceiptOutput(output, toolCallID, toolName string) (string, bool) {
	budget := maxSuccessfulReceiptJSONBytes
	for _, field := range []string{toolCallID, toolName} {
		budget -= jsonStringSizeWithin(field, budget)
		if budget <= len(successfulReceiptTruncatedMarker)+3 {
			// The conservative estimate leaves no room for the marker. Keep
			// the result and let the durable writer apply its exact hard limit.
			return validReceiptUTF8(output), false
		}
	}
	encodedSize := 2 // JSON string quotes.
	// The marker's newline takes one extra byte when JSON-escaped.
	keepLimit := budget - len(successfulReceiptTruncatedMarker) - 1
	retainedEnd := 0
	for pos := 0; pos < len(output); {
		size, encoded := jsonStringRuneSize(output[pos:])
		if size == 1 && output[pos] >= utf8.RuneSelf {
			// Invalid source bytes become a three-byte U+FFFD before JSON.
			encoded = 3
		}
		encodedSize += encoded
		pos += size
		if encodedSize <= keepLimit {
			retainedEnd = pos
		}
		if encodedSize > budget {
			return validReceiptUTF8(output[:retainedEnd]) + successfulReceiptTruncatedMarker, true
		}
	}
	return validReceiptUTF8(output), false
}

func validReceiptUTF8(output string) string {
	if utf8.ValidString(output) {
		return output
	}
	return string([]rune(output))
}

func jsonStringSizeWithin(text string, limit int) int {
	size := 2 // JSON string quotes.
	for pos := 0; pos < len(text); {
		sourceBytes, encodedBytes := jsonStringRuneSize(text[pos:])
		pos += sourceBytes
		size += encodedBytes
		if size > limit {
			return size
		}
	}
	return size
}

// jsonStringRuneSize matches encoding/json's default string escaping,
// including invalid UTF-8 and U+2028/U+2029.
func jsonStringRuneSize(text string) (sourceBytes, encodedBytes int) {
	c := text[0]
	if c < utf8.RuneSelf {
		switch c {
		case '\\', '"', '\b', '\f', '\n', '\r', '\t':
			return 1, 2
		case '<', '>', '&':
			return 1, 6
		default:
			if c < 0x20 {
				return 1, 6
			}
			return 1, 1
		}
	}
	r, size := utf8.DecodeRuneInString(text)
	if (r == utf8.RuneError && size == 1) || r == '\u2028' || r == '\u2029' {
		return size, 6
	}
	return size, size
}
