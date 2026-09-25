package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultSSEParserBoundsLineAndFrame(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "single line",
			input: "data: " + strings.Repeat("x", GuardedStreamMaxBytes) + "\n\n",
			want:  "token too long",
		},
		{
			name:  "frame line count",
			input: strings.Repeat("data: x\n", maxSSEFrameLines+1) + "\n",
			want:  "data lines",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, err := NewDefaultSSEParser().Parse(context.Background(), strings.NewReader(tt.input))
			require.NoError(t, err)
			var parseErr error
			for event := range events {
				if event.Error != nil {
					parseErr = event.Error
				}
			}
			require.Error(t, parseErr)
			require.ErrorContains(t, parseErr, tt.want)
		})
	}
}
