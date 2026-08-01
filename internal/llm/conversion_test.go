package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// NormalizeToolCallID
// ---------------------------------------------------------------------------

func TestNormalizeToolCallIDEmptyAndNil(t *testing.T) {
	assert.Nil(t, NormalizeToolCallID(nil))
	out := NormalizeToolCallID([]Message{})
	assert.Empty(t, out)
}

func TestNormalizeToolCallIDInputNotMutated(t *testing.T) {
	input := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "toolu_1", Name: "f"}}},
		{Role: RoleTool, ToolCallID: "call_2"},
	}
	out := NormalizeToolCallID(input)
	// The input slice must be untouched.
	require.Len(t, input, 2)
	assert.Equal(t, "toolu_1", input[0].ToolCalls[0].ID)
	assert.Equal(t, "call_2", input[1].ToolCallID)
	// The returned slice holds canonicalized copies of the input messages.
	require.Len(t, out, 2)
	assert.Equal(t, "call_1", out[0].ToolCalls[0].ID)
	assert.Equal(t, "call_2", out[1].ToolCallID)
}

func TestNormalizeToolCallIDCanonicalizesProviderPrefixes(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "toolu_abc", Name: "f"}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_def", Name: "g"}}},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "7f3e-9a1c", Name: "h"}}},
	}
	out := NormalizeToolCallID(in)
	require.Len(t, out, 3)
	assert.Equal(t, "call_abc", out[0].ToolCalls[0].ID)
	assert.Equal(t, "call_def", out[1].ToolCalls[0].ID)
	// Bare UUID-style ids get the canonical prefix applied.
	assert.Equal(t, "call_7f3e-9a1c", out[2].ToolCalls[0].ID)
}

func TestNormalizeToolCallIDIdempotent(t *testing.T) {
	in := []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "toolu_abc"}}},
	}
	once := NormalizeToolCallID(in)
	twice := NormalizeToolCallID(once)
	require.Equal(t, "call_abc", once[0].ToolCalls[0].ID)
	assert.Equal(t, "call_abc", twice[0].ToolCalls[0].ID)
}

func TestNormalizeToolCallIDLinksToolCallID(t *testing.T) {
	// A message carrying both a ToolCallID and a matching ToolCalls entry keeps
	// the two linked after canonicalization.
	in := []Message{
		{Role: RoleAssistant, ToolCallID: "call_same", ToolCalls: []ToolCall{{ID: "call_same"}}},
		{Role: RoleAssistant, ToolCallID: "toolu_diff", ToolCalls: []ToolCall{{ID: "toolu_diff"}}},
	}
	out := NormalizeToolCallID(in)
	assert.Equal(t, out[0].ToolCallID, out[0].ToolCalls[0].ID)
	assert.Equal(t, "call_same", out[0].ToolCallID)
	assert.Equal(t, out[1].ToolCallID, out[1].ToolCalls[0].ID)
	assert.Equal(t, "call_diff", out[1].ToolCallID)
}

func TestNormalizeToolCallIDUnmatchedEmptyStaysEmpty(t *testing.T) {
	in := []Message{
		{Role: RoleTool, ToolCallID: ""},
		{Role: RoleAssistant, ToolCallID: "toolu_1"},
	}
	out := NormalizeToolCallID(in)
	// Empty id with no matching ToolCalls stays empty (no random id invented).
	assert.Equal(t, "", out[0].ToolCallID)
	assert.Equal(t, "call_1", out[1].ToolCallID)
}

// ---------------------------------------------------------------------------
// DowngradeImages
// ---------------------------------------------------------------------------

func TestDowngradeImagesEmptyAndNil(t *testing.T) {
	assert.Nil(t, DowngradeImages(nil))
	assert.Empty(t, DowngradeImages([]Message{}))
}

func TestDowngradeImagesReplacesMarker(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Content: "look at this [image:data:image/png;base64,AAAA] please"},
		{Role: RoleUser, Content: "no image here"},
	}
	out := DowngradeImages(in)
	require.Len(t, out, 2)
	assert.Equal(t, "look at this [image omitted: not supported by this provider] please", out[0].Content)
	assert.Equal(t, "no image here", out[1].Content)
	// Input not mutated.
	assert.Equal(t, "look at this [image:data:image/png;base64,AAAA] please", in[0].Content)
}

func TestDowngradeImagesCustomPlaceholder(t *testing.T) {
	in := []Message{{Role: RoleUser, Content: "pic [image:a.png] done"}}
	out := DowngradeImages(in, "CUSTOM")
	require.Len(t, out, 1)
	assert.Equal(t, "pic CUSTOM done", out[0].Content)
}

func TestDowngradeImagesMultipleMarkers(t *testing.T) {
	in := []Message{{Role: RoleUser, Content: "[image:1] a [image:2] b"}}
	out := DowngradeImages(in)
	require.Len(t, out, 1)
	assert.Equal(t, "[image omitted: not supported by this provider] a [image omitted: not supported by this provider] b", out[0].Content)
}

func TestDowngradeImagesUnterminatedMarker(t *testing.T) {
	in := []Message{{Role: RoleUser, Content: "before [image:no-closing-bracket"}}
	out := DowngradeImages(in)
	require.Len(t, out, 1)
	assert.Equal(t, "before [image omitted: not supported by this provider]", out[0].Content)
}

// ---------------------------------------------------------------------------
// AdaptThinking
// ---------------------------------------------------------------------------

func TestAdaptThinkingEmptyAndNil(t *testing.T) {
	assert.Nil(t, AdaptThinking(nil))
	assert.Empty(t, AdaptThinking([]Message{}))
}

func TestAdaptThinkingStripsInlineMarker(t *testing.T) {
	in := []Message{{Role: RoleAssistant, Content: "answer [thinking:let me reason] here"}}
	out := AdaptThinking(in)
	require.Len(t, out, 1)
	assert.Equal(t, "answer  here", out[0].Content)
	// Input not mutated.
	assert.Equal(t, "answer [thinking:let me reason] here", in[0].Content)
}

func TestAdaptThinkingStripsPairedMarker(t *testing.T) {
	in := []Message{{Role: RoleAssistant, Content: "pre [thinking_start]internal reasoning[thinking_end] post"}}
	out := AdaptThinking(in)
	require.Len(t, out, 1)
	assert.Equal(t, "pre  post", out[0].Content)
}

func TestAdaptThinkingLeavesCleanContent(t *testing.T) {
	in := []Message{{Role: RoleUser, Content: "plain question"}}
	out := AdaptThinking(in)
	require.Len(t, out, 1)
	assert.Equal(t, "plain question", out[0].Content)
}

func TestAdaptThinkingUnbalancedStartDropsRest(t *testing.T) {
	in := []Message{{Role: RoleAssistant, Content: "pre [thinking_start]no close"}}
	out := AdaptThinking(in)
	require.Len(t, out, 1)
	assert.Equal(t, "pre ", out[0].Content)
}
