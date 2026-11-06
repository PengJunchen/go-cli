package core

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestBoundedSuccessfulReceiptOutputUsesActualJSONEncoding(t *testing.T) {
	cases := []struct {
		name   string
		output string
	}{
		{name: "small special text", output: "small <&> \"\\\n\x01 中 \u2028 \u2029 \ufffd"},
		{name: "small invalid UTF-8", output: "a\xff\xfeb"},
		{name: "at budget", output: strings.Repeat("a", maxSuccessfulReceiptJSONBytes-6)},
		{name: "above budget", output: strings.Repeat("a", maxSuccessfulReceiptJSONBytes-5)},
		{name: "controls HTML and Unicode", output: strings.Repeat("<&>\"\\\b\f\n\r\t\x00\x1f中\u2028\u2029\ufffd", 40000)},
		{name: "invalid UTF-8", output: strings.Repeat("a", maxSuccessfulReceiptJSONBytes-100) + strings.Repeat("\xff\xfe", 100)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			normalized := string([]rune(tc.output))
			originalJSON, err := json.Marshal(normalized)
			require.NoError(t, err)
			got, truncated := boundedSuccessfulReceiptOutput(tc.output, "", "")
			if len(originalJSON)+4 <= maxSuccessfulReceiptJSONBytes {
				require.False(t, truncated)
				require.Equal(t, normalized, got)
				return
			}
			require.True(t, truncated)
			require.True(t, utf8.ValidString(got))
			require.True(t, strings.HasSuffix(got, successfulReceiptTruncatedMarker))
			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			require.LessOrEqual(t, len(encoded)+4, maxSuccessfulReceiptJSONBytes)
			prefix := strings.TrimSuffix(got, successfulReceiptTruncatedMarker)
			if utf8.ValidString(tc.output) {
				require.True(t, strings.HasPrefix(tc.output, prefix))
			} else {
				require.True(t, strings.HasPrefix(string([]rune(tc.output)), prefix))
			}
		})
	}
}

func TestJSONStringRuneSizeMatchesJSONEncoder(t *testing.T) {
	for c := 0; c < utf8.RuneSelf; c++ {
		input := string(rune(c))
		sourceBytes, encodedBytes := jsonStringRuneSize(input)
		encoded, err := json.Marshal(input)
		require.NoError(t, err)
		require.Equal(t, 1, sourceBytes)
		require.Equal(t, len(encoded)-2, encodedBytes, "ASCII byte %d", c)
	}
	for _, input := range []string{"a", "\x00", "\b", "\f", "\n", "\r", "\t", "\x1f", "\"", "\\", "<", ">", "&", "中", "\u2028", "\u2029", "\ufffd", "\xff"} {
		sourceBytes, encodedBytes := jsonStringRuneSize(input)
		encoded, err := json.Marshal(input)
		require.NoError(t, err)
		require.Equal(t, len(input), sourceBytes)
		require.Equal(t, len(encoded)-2, encodedBytes)
	}
}

func TestBoundedSuccessfulReceiptAccountsForEscapedIdentity(t *testing.T) {
	id := strings.Repeat("<", 20000)
	name := "read&name"
	output := strings.Repeat("x", maxSuccessfulReceiptJSONBytes)
	got, truncated := boundedSuccessfulReceiptOutput(output, id, name)
	require.True(t, truncated)
	outputJSON, err := json.Marshal(got)
	require.NoError(t, err)
	idJSON, err := json.Marshal(id)
	require.NoError(t, err)
	nameJSON, err := json.Marshal(name)
	require.NoError(t, err)
	require.LessOrEqual(t, len(outputJSON)+len(idJSON)+len(nameJSON), maxSuccessfulReceiptJSONBytes)
	require.Greater(t, len(got), 800000)
}
