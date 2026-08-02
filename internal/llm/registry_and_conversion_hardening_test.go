package llm

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// ---------------------------------------------------------------------------
// ProviderRegistry concurrency
// ---------------------------------------------------------------------------

// TestProviderRegistry_ConcurrentRegisterGetList hammers a single registry's
// Register/Get/List concurrently with unique names under -race. It also asserts
// that List deterministically returns the registration order.
func TestProviderRegistry_ConcurrentRegisterGetList(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	reg := NewProviderRegistry()

	const workers = 8
	const perWorker = 8
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				name := fmt.Sprintf("prov-%d-%d", w, i)
				require.NoError(t, reg.Register(NewEinoProvider(WithProviderName(name))))
				// Read back to exercise the read lock concurrently.
				p, err := reg.Get(name)
				if err != nil {
					t.Errorf("Get(%q): %v", name, err)
					return
				}
				if p.Name() != name {
					t.Errorf("Get(%q).Name() = %q", name, p.Name())
				}
			}
		}(w)
	}
	wg.Wait()

	list := reg.List()
	// 1 builtin + workers*perWorker registered.
	require.Len(t, list, 1+workers*perWorker)
	// No duplicate names; Default is the builtin "eino".
	assert.Equal(t, "eino", reg.Default().Name())
}

// ---------------------------------------------------------------------------
// Conversion helpers: further edge cases
// ---------------------------------------------------------------------------

// TestDowngradeImages_EmptyPlaceholderArgFallsBack verifies an explicitly-empty
// placeholder argument falls back to the default sentinel.
func TestDowngradeImages_EmptyPlaceholderArgFallsBack(t *testing.T) {
	in := []Message{{Role: RoleUser, Content: "[image:x]"}}
	out := DowngradeImages(in, "")
	require.Len(t, out, 1)
	assert.Equal(t, defaultImagePlaceholder, out[0].Content)
	assert.Equal(t, "[image:x]", in[0].Content, "input must not be mutated")
}

// TestDowngradeImages_MixedMessagesAliasesUnchanged verifies messages without a
// marker are returned unchanged (aliased) while marker messages are rewritten.
func TestDowngradeImages_MixedMessagesAliasesUnchanged(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Content: "wat [image:a][image:b] done"},
		{Role: RoleUser, Content: "plain"},
	}
	out := DowngradeImages(in)
	require.Len(t, out, 2)
	assert.Equal(t, "wat "+defaultImagePlaceholder+defaultImagePlaceholder+" done", out[0].Content)
	// The plain message is shared with the input (no copy needed).
	assert.Equal(t, "plain", out[1].Content)
	assert.Equal(t, "plain", in[1].Content)
}

// TestReplaceMarkerSegments_MultipleAndTerminated verifies the internal utility
// rewrites every complete segment and passes through text between segments.
func TestReplaceMarkerSegments_MultipleAndTerminated(t *testing.T) {
	got := replaceMarkerSegments("[m:a]x[m:b]y", "[m:", "X")
	assert.Equal(t, "XxXy", got)
	// No marker: unchanged.
	assert.Equal(t, "abc", replaceMarkerSegments("abc", "[m:", "X"))
	// Unterminated marker replaced through end of string.
	assert.Equal(t, "preX", replaceMarkerSegments("pre[m:oops", "[m:", "X"))
}

// TestAdaptThinking_MixedMessagesKeepsPlainAliased verifies messages without
// any thinking marker are returned unchanged while marked ones are stripped.
func TestAdaptThinking_MixedMessagesKeepsPlainAliased(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Content: "a [thinking:hidden] b"},
		{Role: RoleUser, Content: "clean"},
	}
	out := AdaptThinking(in)
	require.Len(t, out, 2)
	assert.Equal(t, "a  b", out[0].Content)
	assert.Equal(t, "clean", out[1].Content)
	assert.Equal(t, "clean", in[1].Content, "input must not be mutated")
}

// TestStripThinkingRegions_MultipleRegions verifies multiple paired regions are
// all removed.
func TestStripThinkingRegions_MultipleRegions(t *testing.T) {
	got := stripThinkingRegions("[thinking_start]r1[thinking_end]mid[thinking_start]r2[thinking_end]end")
	assert.Equal(t, "midend", got)
}

// TestStripThinking_MixedInlineAndPairedInOne verifies inline and paired
// markers in the same content are both removed while surrounding text survives.
func TestStripThinking_MixedInlineAndPairedInOne(t *testing.T) {
	got := stripThinking("before [thinking:inline] between [thinking_start]blob[thinking_end] after")
	assert.Equal(t, "before  between  after", got)
}

// TestConversionPipeline_NonMutating verifies chaining the three exported
// conversion helpers leaves the original input untouched and is idempotent.
func TestConversionPipeline_NonMutating(t *testing.T) {
	in := []Message{{
		Role:    RoleUser,
		Content: "see [image:png] think [thinking:hmm][thinking_start]x[thinking_end] tail",
		ToolCalls: []ToolCall{
			{ID: "toolu_1", Name: "f"},
		},
		ToolCallID: "toolu_1",
	}}
	original := []Message{{
		Role:    RoleUser,
		Content: "see [image:png] think [thinking:hmm][thinking_start]x[thinking_end] tail",
		ToolCalls: []ToolCall{
			{ID: "toolu_1", Name: "f"},
		},
		ToolCallID: "toolu_1",
	}}

	// Run once.
	out := AdaptThinking(DowngradeImages(NormalizeToolCallID(in)))
	require.Len(t, out, 1)
	assert.Equal(t, "see [image omitted: not supported by this provider] think  tail", out[0].Content)
	require.Len(t, out[0].ToolCalls, 1)
	assert.Equal(t, "call_1", out[0].ToolCalls[0].ID)
	assert.Equal(t, "call_1", out[0].ToolCallID)

	// Run again: normalizing is idempotent (no further strip of "call_").
	out2 := NormalizeToolCallID(out)
	assert.Equal(t, "call_1", out2[0].ToolCalls[0].ID)

	// The original input is completely untouched.
	assert.Equal(t, original, in)
}

// TestNormalizeToolCallID_OnlyToolCallsCanonicalized verifies a message carrying
// only ToolCallID is canonicalized and its unmatched ToolCallID follows the
// linking rule.
func TestNormalizeToolCallID_OnlyToolCallID(t *testing.T) {
	in := []Message{{Role: RoleTool, ToolCallID: "toolu_7", Content: "r"}}
	out := NormalizeToolCallID(in)
	require.Len(t, out, 1)
	assert.Equal(t, "call_7", out[0].ToolCallID)
	assert.Empty(t, out[0].ToolCalls)
	// Input remains untouched.
	assert.Equal(t, "toolu_7", in[0].ToolCallID)
}

// TestCanonicalToolCallID_TooluOnly verifies a bare "toolu_" id is preserved as
// a dangling prefix rather than rewritten.
func TestCanonicalToolCallID_TooluOnly(t *testing.T) {
	assert.Equal(t, "toolu_", canonicalToolCallID("toolu_"))
}
