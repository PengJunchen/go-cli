package mock

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLongConversationGeneratorDeterministic(t *testing.T) {
	g1 := NewLongConversationGenerator(100, 5, 4)
	g2 := NewLongConversationGenerator(100, 5, 4)

	t1 := g1.Generate()
	t2 := g2.Generate()

	require.Equal(t, len(t1.Turns), len(t2.Turns))
	assert.Equal(t, t1, t2, "generated templates must be deterministic")
}

func TestLongConversationGeneratorTurnCounts(t *testing.T) {
	for _, tt := range []struct {
		name      string
		turnCount int
		toolEvery int
		fileCount int
		minTurns  int
	}{
		{"100", 100, 3, 5, 100},
		{"200", 200, 2, 8, 200},
		{"500", 500, 5, 3, 500},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewLongConversationGenerator(tt.turnCount, tt.toolEvery, tt.fileCount)
			tmpl := g.Generate()
			// Every user turn yields at least one assistant turn.
			assert.GreaterOrEqual(t, len(tmpl.Turns), tt.minTurns)
			for _, turn := range tmpl.Turns {
				require.True(t, turn.AssistantContent != "" || len(turn.AssistantToolCalls) > 0,
					"every assistant turn must have content or tool calls")
			}
		})
	}
}

func TestLongConversationGeneratorInjectsToolTurns(t *testing.T) {
	g := NewLongConversationGenerator(20, 4, 2)
	tmpl := g.Generate()

	toolTurns := 0
	fileTurns := 0
	bashTurns := 0
	for _, turn := range tmpl.Turns {
		for _, tc := range turn.AssistantToolCalls {
			toolTurns++
			switch tc.Name {
			case "read_file":
				fileTurns++
			case "bash":
				bashTurns++
			}
		}
	}
	// With 20 turns and toolEvery=4 there are 5 tool-bearing cycles, each with
	// a read_file and a bash call.
	assert.Equal(t, 10, toolTurns)
	assert.Equal(t, 5, fileTurns)
	assert.Equal(t, 5, bashTurns)
}

func TestLongConversationGeneratorToolIDsDeterministic(t *testing.T) {
	g := NewLongConversationGenerator(10, 2, 1)
	tmpl := g.Generate()

	// Collect an id from the first tool turn and regenerate to confirm stability.
	t1 := g.Generate()
	require.Equal(t, len(tmpl.Turns), len(t1.Turns))

	var firstID string
	for _, turn := range tmpl.Turns {
		if len(turn.AssistantToolCalls) > 0 {
			firstID = turn.AssistantToolCalls[0].ID
			break
		}
	}
	for _, turn := range t1.Turns {
		if len(turn.AssistantToolCalls) > 0 {
			assert.Equal(t, firstID, turn.AssistantToolCalls[0].ID)
			break
		}
	}
}

func TestLongConversationGeneratorEdgeParams(t *testing.T) {
	// Zero and negative params must not panic and should clamp gracefully.
	g := NewLongConversationGenerator(0, 0, 0)
	tmpl := g.Generate()
	assert.Empty(t, tmpl.Turns)
	assert.NotEmpty(t, tmpl.ID)
}
