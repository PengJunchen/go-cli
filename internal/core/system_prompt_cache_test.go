package core

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// cacheTestTool is a ToolDefinition that also implements PromptGuideliner.
// The guidelineCallCount tracks how many times PromptGuidelines is invoked,
// which lets tests prove that buildInner (and therefore the guideline
// collection) is skipped on a cache hit.
type cacheTestTool struct {
	name        string
	description string
	guidelines  []string

	mu               sync.Mutex
	guidelineCalls   int
}

func (t *cacheTestTool) Name() string        { return t.name }
func (t *cacheTestTool) Description() string { return t.description }
func (t *cacheTestTool) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "ok"}, nil
}

func (t *cacheTestTool) PromptGuidelines() []string {
	t.mu.Lock()
	t.guidelineCalls++
	t.mu.Unlock()
	return t.guidelines
}

func (t *cacheTestTool) guidelineCallCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.guidelineCalls
}

var _ tools.ToolDefinition = (*cacheTestTool)(nil)
var _ tools.PromptGuideliner = (*cacheTestTool)(nil)

// TestSystemPromptCachedOnSameConfig verifies that calling Build twice with
// identical options returns the same prompt and that buildInner (measured via
// PromptGuidelines calls) is only executed once.
func TestSystemPromptCachedOnSameConfig(t *testing.T) {
	tool := &cacheTestTool{
		name:       "bash",
		guidelines: []string{"Use bash for shell commands"},
	}
	builder := NewDefaultSystemPromptBuilder()
	opts := SystemPromptOptions{
		Tools: []tools.ToolDefinition{tool},
		Cwd:   "/home/user/project",
	}

	first := builder.Build(context.Background(), opts)
	assert.Contains(t, first, "bash")
	assert.Contains(t, first, "Use bash for shell commands")
	require.Equal(t, 1, tool.guidelineCallCount(), "buildInner should run on first Build")

	second := builder.Build(context.Background(), opts)
	assert.Equal(t, first, second, "cached prompt must be identical")
	assert.Equal(t, 1, tool.guidelineCallCount(), "buildInner must NOT run on cache hit")
}

// TestSystemPromptRebuiltOnToolChange verifies that changing the tool set
// invalidates the cache and causes a rebuild.
func TestSystemPromptRebuiltOnToolChange(t *testing.T) {
	toolA := &cacheTestTool{name: "read", guidelines: []string{"Read files safely"}}
	toolB := &cacheTestTool{name: "write", guidelines: []string{"Write files carefully"}}

	builder := NewDefaultSystemPromptBuilder()

	first := builder.Build(context.Background(), SystemPromptOptions{
		Tools: []tools.ToolDefinition{toolA},
	})
	assert.Contains(t, first, "read")
	assert.Contains(t, first, "Read files safely")
	require.Equal(t, 1, toolA.guidelineCallCount())

	second := builder.Build(context.Background(), SystemPromptOptions{
		Tools: []tools.ToolDefinition{toolB},
	})
	assert.Contains(t, second, "write")
	assert.Contains(t, second, "Write files carefully")
	assert.NotContains(t, second, "read", "rebuilt prompt must reflect new tools")
	assert.NotEqual(t, first, second, "prompt must change when tools change")
	require.Equal(t, 1, toolB.guidelineCallCount(), "buildInner should run for the new tool set")
}

// TestSystemPromptRebuiltOnConfigChange verifies that changing non-tool config
// (cwd, custom prompt, context files, skills, memories, append prompt)
// invalidates the cache and causes a rebuild.
func TestSystemPromptRebuiltOnConfigChange(t *testing.T) {
	builder := NewDefaultSystemPromptBuilder()

	first := builder.Build(context.Background(), SystemPromptOptions{
		Cwd:          "/dir-a",
		CustomPrompt: "You are assistant A",
	})

	// Change cwd → different prompt.
	second := builder.Build(context.Background(), SystemPromptOptions{
		Cwd:          "/dir-b",
		CustomPrompt: "You are assistant A",
	})
	assert.NotEqual(t, first, second, "changing cwd should rebuild the prompt")
	assert.Contains(t, second, "/dir-b")

	// Change custom prompt → different prompt.
	third := builder.Build(context.Background(), SystemPromptOptions{
		Cwd:          "/dir-b",
		CustomPrompt: "You are assistant B",
	})
	assert.NotEqual(t, second, third, "changing custom prompt should rebuild")
	assert.Contains(t, third, "You are assistant B")

	// Change append prompt → different prompt.
	fourth := builder.Build(context.Background(), SystemPromptOptions{
		Cwd:          "/dir-b",
		CustomPrompt: "You are assistant B",
		AppendPrompt: "Extra rules",
	})
	assert.NotEqual(t, third, fourth, "changing append prompt should rebuild")
	assert.Contains(t, fourth, "Extra rules")

	// Change context files → different prompt.
	fifth := builder.Build(context.Background(), SystemPromptOptions{
		Cwd:          "/dir-b",
		CustomPrompt: "You are assistant B",
		AppendPrompt: "Extra rules",
		ContextFiles: []ContextFile{{Path: "AGENTS.md", Content: "be careful"}},
	})
	assert.NotEqual(t, fourth, fifth, "adding context files should rebuild")
	assert.Contains(t, fifth, "be careful")

	// Change skills → different prompt.
	sixth := builder.Build(context.Background(), SystemPromptOptions{
		Cwd:          "/dir-b",
		CustomPrompt: "You are assistant B",
		AppendPrompt: "Extra rules",
		ContextFiles: []ContextFile{{Path: "AGENTS.md", Content: "be careful"}},
		Skills: []SkillInfo{{Name: "my-skill", Description: "does things"}},
	})
	assert.NotEqual(t, fifth, sixth, "adding skills should rebuild")
	assert.Contains(t, sixth, "my-skill")

	// Change memories → different prompt.
	seventh := builder.Build(context.Background(), SystemPromptOptions{
		Cwd:          "/dir-b",
		CustomPrompt: "You are assistant B",
		AppendPrompt: "Extra rules",
		ContextFiles: []ContextFile{{Path: "AGENTS.md", Content: "be careful"}},
		Skills:      []SkillInfo{{Name: "my-skill", Description: "does things"}},
		Memories:    []MemoryEntry{{ID: "1", Content: "remember this", Category: "fact"}},
	})
	assert.NotEqual(t, sixth, seventh, "adding memories should rebuild")
	assert.Contains(t, seventh, "remember this")
}

// TestSystemPromptCacheConcurrentSafety verifies that concurrent Build calls
// with the same options are race-free and return consistent results.
func TestSystemPromptCacheConcurrentSafety(t *testing.T) {
	builder := NewDefaultSystemPromptBuilder()
	opts := SystemPromptOptions{
		Tools: []tools.ToolDefinition{&cacheTestTool{name: "bash"}},
		Cwd:   "/concurrent/dir",
	}

	var wg sync.WaitGroup
	results := make([]string, 20)
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = builder.Build(context.Background(), opts)
		}(i)
	}
	wg.Wait()

	for i := 1; i < len(results); i++ {
		assert.Equal(t, results[0], results[i], "all concurrent builds must return the same prompt")
	}
}

// TestSystemPromptCacheNoDelimiterCollision verifies that two distinct option
// sets whose raw concatenation used to produce the same cache version (e.g.
// Path "a"+Content "b" == Path "ab"+Content "") no longer share a cache entry.
// This guards against stale-cache bugs caused by unframed field concatenation.
func TestSystemPromptCacheNoDelimiterCollision(t *testing.T) {
	builder := NewDefaultSystemPromptBuilder()

	first := builder.Build(context.Background(), SystemPromptOptions{
		ContextFiles: []ContextFile{{Path: "a", Content: "b"}},
	})
	assert.Contains(t, first, "--- a ---")
	assert.Contains(t, first, "b")

	// Previously collided: "a"+"b" == "ab"+"". The empty-content file renders
	// nothing, so the second prompt must NOT contain the first file's content.
	second := builder.Build(context.Background(), SystemPromptOptions{
		ContextFiles: []ContextFile{{Path: "ab", Content: ""}},
	})
	assert.NotEqual(t, first, second, "colliding inputs must not share a cache entry")
	assert.NotContains(t, second, "--- a ---", "empty-content file must not render stale content from a colliding key")
}
