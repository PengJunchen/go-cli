package llm

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOverflowContinuationPrimaryCoverage(t *testing.T) {
	first := `{"input_tokens":100,"output_tokens":20,"total_tokens":120,"primary_coverage":"complete"}`
	second := `{"input_tokens":50,"output_tokens":10,"total_tokens":60,"primary_coverage":"complete"}`
	zero := `{"input_tokens":0,"output_tokens":0,"total_tokens":0,"primary_coverage":"complete"}`
	for _, test := range []struct {
		name     string
		segments []string
		coverage string
		input    int
		output   int
	}{
		{"single-complete", []string{first}, "complete", 100, 20},
		{"single-zero", []string{zero}, "complete", 0, 0},
		{"complete", []string{first, second}, "complete", 150, 30},
		{"missing-first", []string{"", second}, "partial", 50, 10},
		{"missing-second", []string{first, ""}, "partial", 100, 20},
		{"both-missing", []string{"", ""}, "", 0, 0},
		{"two-missing-then-known", []string{"", "", second}, "partial", 50, 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			model := &mockModel{generateFn: func(context.Context, []Message, ...Option) (*Message, error) {
				segment := test.segments[calls]
				calls++
				var usage *Usage
				if segment != "" {
					usage = &Usage{}
					require.NoError(t, json.Unmarshal([]byte(segment), usage))
				}
				reason := "length"
				if calls == len(test.segments) {
					reason = "stop"
				}
				return &Message{Role: RoleAssistant, Content: "part", FinishReason: reason, Usage: usage}, nil
			}}
			result, err := NewOverflowRecoveryMiddleware().WrapModel(model).Generate(context.Background(), nil)
			require.NoError(t, err)
			require.Equal(t, len(test.segments), calls)
			if test.coverage == "" {
				assert.Nil(t, result.Usage)
				return
			}
			got := usageJSON(t, result.Usage)
			assert.Equal(t, test.coverage, got["primary_coverage"])
			assert.Equal(t, float64(test.input), got["input_tokens"])
			assert.Equal(t, float64(test.output), got["output_tokens"])
		})
	}
}
