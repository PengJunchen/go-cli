package llm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrimaryCoverageAccumulator(t *testing.T) {
	complete := &Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12, PrimaryCoverage: UsageCoverageComplete}
	zero := &Usage{PrimaryCoverage: UsageCoverageComplete}
	partial := &Usage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5, PrimaryCoverage: UsageCoveragePartial}
	legacy := &Usage{InputTokens: 3, OutputTokens: 1, TotalTokens: 4}
	for _, test := range []struct {
		name       string
		segments   []*Usage
		coverage   PrimaryUsageCoverage
		input      int
		output     int
		wantAbsent bool
	}{
		{"empty", nil, UsageCoverageUnknown, 0, 0, true},
		{"missing-only", []*Usage{nil}, UsageCoverageUnknown, 0, 0, true},
		{"missing-missing", []*Usage{nil, nil}, UsageCoverageUnknown, 0, 0, true},
		{"known-only", []*Usage{complete}, UsageCoverageComplete, 10, 2, false},
		{"known-zero", []*Usage{zero}, UsageCoverageComplete, 0, 0, false},
		{"complete-complete", []*Usage{complete, complete}, UsageCoverageComplete, 20, 4, false},
		{"missing-known", []*Usage{nil, complete}, UsageCoveragePartial, 10, 2, false},
		{"known-missing", []*Usage{complete, nil}, UsageCoveragePartial, 10, 2, false},
		{"missing-missing-known", []*Usage{nil, nil, complete}, UsageCoveragePartial, 10, 2, false},
		{"partial-complete", []*Usage{partial, complete}, UsageCoveragePartial, 14, 3, false},
		{"complete-partial", []*Usage{complete, partial}, UsageCoveragePartial, 14, 3, false},
		{"partial-partial", []*Usage{partial, partial}, UsageCoveragePartial, 8, 2, false},
		{"legacy-complete", []*Usage{legacy, complete}, UsageCoverageUnknown, 13, 3, false},
		{"complete-legacy", []*Usage{complete, legacy}, UsageCoverageUnknown, 13, 3, false},
		{"legacy-missing-complete", []*Usage{legacy, nil, complete}, UsageCoveragePartial, 13, 3, false},
		{"missing-legacy-complete", []*Usage{nil, legacy, complete}, UsageCoveragePartial, 13, 3, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var acc ContinuationUsageAccumulator
			for _, segment := range test.segments {
				acc.Observe(segment)
			}
			got := acc.Usage()
			if test.wantAbsent {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, test.coverage, got.PrimaryCoverage)
			assert.Equal(t, test.input, got.InputTokens)
			assert.Equal(t, test.output, got.OutputTokens)
			got.InputTokens = -1
			assert.Equal(t, test.input, acc.Usage().InputTokens, "returned usage is a copy")
		})
	}
	var nested ContinuationUsageAccumulator
	nested.Observe(nil)
	nested.Observe(complete)
	var outer ContinuationUsageAccumulator
	outer.Observe(complete)
	outer.Observe(nested.Usage())
	assert.Equal(t, UsageCoveragePartial, outer.Usage().PrimaryCoverage)
	assert.Equal(t, 20, outer.Usage().InputTokens)
	var reversed ContinuationUsageAccumulator
	reversed.Observe(nested.Usage())
	reversed.Observe(complete)
	assert.Equal(t, UsageCoveragePartial, reversed.Usage().PrimaryCoverage)
	assert.Equal(t, 20, reversed.Usage().InputTokens)
	assert.Equal(t, 10, complete.InputTokens, "caller-owned usage must not be mutated")
}

func TestPrimaryCoverageCompatibilityAndPresence(t *testing.T) {
	var old Usage
	require.NoError(t, json.Unmarshal([]byte(`{"input_tokens":10,"output_tokens":2,"total_tokens":12}`), &old))
	assert.Equal(t, UsageCoverageUnknown, old.PrimaryCoverage.Normalized())
	assert.Equal(t, UsageCoverageUnknown, (Usage{}).PrimaryCoverage.Normalized())
	assert.Equal(t, UsageCoverageUnknown, PrimaryUsageCoverage("invented").Normalized())
	bytes, err := json.Marshal(old)
	require.NoError(t, err)
	assert.NotContains(t, string(bytes), "primary_coverage", "historical rows are not rewritten")
	encoded, err := json.Marshal(Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12, PrimaryCoverage: UsageCoveragePartial})
	require.NoError(t, err)
	var oldReader struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	}
	require.NoError(t, json.Unmarshal(encoded, &oldReader))
	assert.Equal(t, 10, oldReader.InputTokens)
	assert.Equal(t, 2, oldReader.OutputTokens)
	assert.Equal(t, 12, oldReader.TotalTokens)

	for _, test := range []struct {
		name, input string
		read        func([]byte) (*Usage, error)
		known       bool
	}{
		{"openai-zero", `{"prompt_tokens":0,"completion_tokens":0}`, func(data []byte) (*Usage, error) {
			var u openAIUsage
			err := json.Unmarshal(data, &u)
			return u.toUsage(), err
		}, true},
		{"openai-missing", `{"prompt_tokens":0}`, func(data []byte) (*Usage, error) {
			var u openAIUsage
			err := json.Unmarshal(data, &u)
			return u.toUsage(), err
		}, false},
		{"claude-zero", `{"input_tokens":0,"output_tokens":0}`, func(data []byte) (*Usage, error) {
			var u claudeUsage
			err := json.Unmarshal(data, &u)
			return u.toUsage(), err
		}, true},
		{"claude-missing", `{"input_tokens":0}`, func(data []byte) (*Usage, error) {
			var u claudeUsage
			err := json.Unmarshal(data, &u)
			return u.toUsage(), err
		}, false},
		{"gemini-zero", `{"promptTokenCount":0,"candidatesTokenCount":0}`, func(data []byte) (*Usage, error) {
			var u geminiUsage
			err := json.Unmarshal(data, &u)
			return u.toUsage(), err
		}, true},
		{"gemini-missing", `{"promptTokenCount":0}`, func(data []byte) (*Usage, error) {
			var u geminiUsage
			err := json.Unmarshal(data, &u)
			return u.toUsage(), err
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.read([]byte(test.input))
			require.NoError(t, err)
			if !test.known {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, UsageCoverageComplete, got.PrimaryCoverage)
			assert.Zero(t, got.InputTokens)
			assert.Zero(t, got.OutputTokens)
			assert.False(t, got.CacheReadInputTokensReported)
			assert.False(t, got.ReasoningOutputTokensReported)
		})
	}
}

func TestPrimaryCoverageRejectsNegativeProviderCounts(t *testing.T) {
	for _, test := range []struct {
		name  string
		usage *Usage
	}{
		{"openai", (&openAIUsage{PromptTokens: primaryIntPointer(-1), CompletionTokens: primaryIntPointer(0)}).toUsage()},
		{"claude", (&claudeUsage{InputTokens: primaryIntPointer(0), OutputTokens: primaryIntPointer(-1)}).toUsage()},
		{"gemini", (&geminiUsage{PromptTokenCount: primaryIntPointer(-1), CandidatesTokenCount: primaryIntPointer(0)}).toUsage()},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NotNil(t, test.usage)
			assert.Equal(t, UsageCoverageUnknown, test.usage.PrimaryCoverage.Normalized())
		})
	}
}

func primaryIntPointer(value int) *int { return &value }
