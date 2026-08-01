package mock

import "fmt"

// LongConversationGenerator produces ConversationTemplates with many user turns
// for long-conversation testing (compaction, context management, memory
// stability). Every toolInterval turns a read_file and bash tool-call sequence
// is injected so generators exercise tool-driven multi-turn flows
// deterministically.
type LongConversationGenerator struct {
	// turnCount is the number of user turns in the generated template.
	turnCount int
	// toolInterval: every this many user turns injects a tool-call sequence.
	toolInterval int
	// fileCount is the number of distinct pseudo-file paths to cycle through.
	fileCount int
}

// NewLongConversationGenerator creates a long-conversation generator.
func NewLongConversationGenerator(turnCount, toolInterval, fileCount int) *LongConversationGenerator {
	if turnCount < 0 {
		turnCount = 0
	}
	if toolInterval < 1 {
		toolInterval = 1
	}
	if fileCount < 1 {
		fileCount = 1
	}
	return &LongConversationGenerator{turnCount: turnCount, toolInterval: toolInterval, fileCount: fileCount}
}

// Generate returns a ConversationTemplate with turnCount user-driven turns. The
// returned template is deterministic for a given set of generator parameters.
func (g *LongConversationGenerator) Generate() *ConversationTemplate {
	files := g.generateFileNames()

	// Each user turn yields up to 4 llm responses (assistant→tool→assistant→
	// tool) plus the final one, so presize generously.
	turns := make([]ConversationTurn, 0, g.turnCount*3)

	for i := 0; i < g.turnCount; i++ {
		filename := files[i%len(files)]

		// User turn is represented indirectly: MockLLMServer only defines
		// assistant responses. The user message is implied by the first
		// assistant turn of each cycle, so we emit an assistant turn that
		// either continues the conversation or issues tool calls.
		if g.toolInterval > 0 && i%g.toolInterval == 0 {
			turns = append(turns,
				ConversationTurn{AssistantToolCalls: []ExpectedToolCall{
					{ID: genID(i, "read"), Name: "read_file",
						Args: map[string]any{"path": filename}},
				}},
				ConversationTurn{AssistantToolCalls: []ExpectedToolCall{
					{ID: genID(i, "bash"), Name: "bash",
						Args: map[string]any{"command": "go test ./..."}},
				}},
				ConversationTurn{AssistantContent: fmt.Sprintf("iteration %d complete for %s", i, filename)},
			)
		} else {
			turns = append(turns, ConversationTurn{
				AssistantContent: fmt.Sprintf("iteration %d noted; status nominal", i),
			})
		}
	}

	return &ConversationTemplate{
		ID:    fmt.Sprintf("L-G%03d", g.turnCount),
		Name:  fmt.Sprintf("long-conversation-%d-turns", g.turnCount),
		Turns: turns,
	}
}

// generateFileNames produces the pseudo-file paths used by the generator.
func (g *LongConversationGenerator) generateFileNames() []string {
	files := make([]string, g.fileCount)
	for i := 0; i < g.fileCount; i++ {
		files[i] = fmt.Sprintf("/project/module%d/main.go", i)
	}
	return files
}

// genID builds a deterministic tool-call id for a user turn and tool.
func genID(turn int, tool string) string {
	return fmt.Sprintf("call-%d-%s", turn, tool)
}
