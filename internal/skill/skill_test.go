package skill_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/skill"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// captureExporter is a concurrency-safe in-memory TraceExporter used to assert
// the spans emitted by the skill loader and adapter. It is defined locally so
// the skill tests do not depend on the mock package.
type captureExporter struct {
	mu      sync.Mutex
	exports []tracing.SpanData
}

var _ tracing.TraceExporter = (*captureExporter)(nil)

func (e *captureExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.exports = append(e.exports, tracing.SpanToData(span))
	return nil
}

func (e *captureExporter) Shutdown(context.Context) error { return nil }

func (e *captureExporter) spans() []tracing.SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]tracing.SpanData, len(e.exports))
	copy(out, e.exports)
	return out
}

// waitForSpan blocks until a span with the given name is collected or the
// timeout expires.
func waitForSpan(t *testing.T, e *captureExporter, name string) tracing.SpanData {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range e.spans() {
			if s.Name == name {
				return s
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for span %q (have %d spans)", name, len(e.spans()))
	return tracing.SpanData{}
}

// rootContext returns a context that carries a fresh tracer mapped to e, so
// spans created via tracing.SpanFromContext are exported to e.
func rootContext(e *captureExporter) context.Context {
	tr := tracing.NewTracer("test-trace", e)
	root, ctx := tr.Start(context.Background(), "test.root", tracing.SpanKindInternal)
	_ = root
	return ctx
}

func writeSkillFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

const sampleSkill = `---
name: example
description: an example skill
version: 1.0.0
category: coding
prompt: |
  You are a coding assistant skill.
  Follow the instructions carefully.
tools:
  - bash
  - read
trigger_hint: "fix bug"
parameters:
  max_attempts: 3
---
optional body markdown that is ignored when a prompt is declared
`

// AC-1: Load parses a YAML-frontmatter skill file and populates all 8 fields.
func TestYAMLLoaderLoadPopulatesAllFields(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := writeSkillFile(t, dir, "example.md", sampleSkill)

	loader := skill.NewYAMLSkillLoader()
	def, err := loader.Load(context.Background(), path)
	require.NoError(t, err)
	require.NotNil(t, def)
	d := *def

	assert.Equal(t, "example", d.Name())
	assert.Equal(t, "an example skill", d.Description())
	assert.Equal(t, "1.0.0", d.Version())
	assert.Equal(t, "coding", d.Category())
	assert.Contains(t, d.Prompt(), "You are a coding assistant skill.")

	assert.ElementsMatch(t, []string{"bash", "read"}, d.Tools())
	require.Contains(t, d.Parameters(), "max_attempts")
	assert.Equal(t, "fix bug", d.TriggerHint())
}

// AC-2: LoadDir loads every skill file in a directory.
func TestYAMLLoaderLoadDir(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	writeSkillFile(t, dir, "one.md", `---
name: one
description: first skill
---
body one
`)
	sub := filepath.Join(dir, "nested")
	require.NoError(t, os.MkdirAll(sub, 0o700))
	writeSkillFile(t, sub, "two.skill.md", `---
name: two
description: second skill
---
body two
`)
	writeSkillFile(t, dir, "ignore.txt", "not a skill file")

	loader := skill.NewYAMLSkillLoader()
	defs, err := loader.LoadDir(context.Background(), dir)
	require.NoError(t, err)
	require.Len(t, defs, 2)

	names := map[string]string{}
	for _, d := range defs {
		def := *d
		names[def.Name()] = def.Prompt()
	}
	assert.Contains(t, names, "one")
	assert.Contains(t, names, "two")
	assert.Contains(t, names["two"], "body two")
}

// AC-2 shared: Prompt falls back to the body when no frontmatter prompt.
func TestYAMLLoaderPromptFallsBackToBody(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := writeSkillFile(t, dir, "noprompt.md", `---
name: plain
description: no explicit prompt here
---
the body is the prompt
`)

	loader := skill.NewYAMLSkillLoader()
	def, err := loader.Load(context.Background(), path)
	require.NoError(t, err)
	d := *def
	assert.Equal(t, "the body is the prompt", d.Prompt())
}

// AC-1 tolerant: unknown keys ignored, missing optional fields default empty.
func TestYAMLLoaderTolerantParse(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := writeSkillFile(t, dir, "minimal.md", `---
name: minimal
unknown_key: ignored
version: 2.0.0
parameters:
  retries: 5
  verbose: true
  label: "hello"
---
body
`)

	loader := skill.NewYAMLSkillLoader()
	def, err := loader.Load(context.Background(), path)
	require.NoError(t, err)
	d := *def
	assert.Equal(t, "minimal", d.Name())
	assert.Empty(t, d.Description())
	assert.Equal(t, "2.0.0", d.Version())
	assert.Empty(t, d.Category())
	assert.Empty(t, d.TriggerHint())
	assert.Empty(t, d.Tools())
	require.Equal(t, 5, d.Parameters()["retries"])
	require.Equal(t, true, d.Parameters()["verbose"])
	require.Equal(t, "hello", d.Parameters()["label"])
	assert.Equal(t, "body", d.Prompt())
}

// AC-1 error: malformed file returns a wrapped error.
func TestYAMLLoaderParseError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := writeSkillFile(t, dir, "bad.md", "no frontmatter delimiter here\n")
	loader := skill.NewYAMLSkillLoader()
	_, err := loader.Load(context.Background(), path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skill")
}

// AC-3: Register / Get / List / Match / Unregister on the default registry.
func TestDefaultRegistryLifecycle(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()

	a := skill.NewSkill("alpha", skill.WithDescription("do alpha"), skill.WithCategory("coding"))
	b := skill.NewSkill("beta", skill.WithDescription("do beta"), skill.WithCategory("writing"))

	require.NoError(t, reg.Register(ctx, a))
	require.NoError(t, reg.Register(ctx, b))

	got, ok := reg.Get(ctx, "alpha")
	require.True(t, ok)
	assert.Equal(t, "alpha", got.Name())

	_, missing := reg.Get(ctx, "nope")
	assert.False(t, missing)

	list := reg.List(ctx)
	require.Len(t, list, 2)
	assert.Equal(t, "alpha", list[0].Name())
	assert.Equal(t, "beta", list[1].Name())

	require.NoError(t, reg.Unregister(ctx, "alpha"))
	_, gone := reg.Get(ctx, "alpha")
	assert.False(t, gone)
	list = reg.List(ctx)
	require.Len(t, list, 1)
	assert.Equal(t, "beta", list[0].Name())

	err := reg.Unregister(ctx, "alpha")
	require.ErrorIs(t, err, skill.ErrSkillNotFound)
}

// AC-3 errors: nil registration and empty name are rejected.
func TestDefaultRegistryRegisterInvalid(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	reg := skill.NewDefaultSkillRegistry()
	require.ErrorIs(t, reg.Register(context.Background(), nil), skill.ErrNilSkill)
	require.Error(t, reg.Register(context.Background(), skill.NewSkill("")))

	dupe := skill.NewSkill("x", skill.WithCategory("coding"))
	require.NoError(t, reg.Register(context.Background(), dupe))
	// Last registration wins; order preserved.
	require.NoError(t, reg.Register(context.Background(), skill.NewSkill("x", skill.WithCategory("other"))))
	got, ok := reg.Get(context.Background(), "x")
	require.True(t, ok)
	assert.Equal(t, "other", got.Category())
	assert.Len(t, reg.List(context.Background()), 1)
}

// AC-4: List filters by category; Match finds by name/description/trigger.
func TestDefaultRegistryProgressiveDisclosure(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := skill.NewDefaultSkillRegistry()

	for _, s := range []skill.SkillDefinition{
		skill.NewSkill("fix-bug", skill.WithDescription("patch a defect"),
			skill.WithCategory("coding"), skill.WithTriggerHint("fix bug")),
		skill.NewSkill("write-doc", skill.WithDescription("produce markdown docs"),
			skill.WithCategory("writing"), skill.WithTriggerHint("documentation")),
		skill.NewSkill("review", skill.WithDescription("review the diff"),
			skill.WithCategory("coding"), skill.WithTriggerHint("check the pr")),
	} {
		require.NoError(t, reg.Register(ctx, s))
	}

	// Category filter returns only skills in that category.
	coding := reg.List(ctx, "coding")
	require.Len(t, coding, 2)
	for _, c := range coding {
		assert.Equal(t, "coding", c.Category())
	}

	// No category returns all.
	assert.Len(t, reg.List(ctx), 3)

	// Exact name match ranks first.
	byName := reg.Match(ctx, "fix-bug")
	require.NotEmpty(t, byName)
	assert.Equal(t, "fix-bug", byName[0].Name())

	// Hint matches on trigger hint.
	byTrigger := reg.Match(ctx, "fix bug")
	require.NotEmpty(t, byTrigger)
	assert.Equal(t, "fix-bug", byTrigger[0].Name())

	// Hint matches on description substring, case-insensitive.
	byDesc := reg.Match(ctx, "MarkDown DOCS")
	require.NotEmpty(t, byDesc)
	assert.Equal(t, "write-doc", byDesc[0].Name())

	// Non-matching hint yields nothing.
	assert.Empty(t, reg.Match(ctx, "zzz-no-such-thing"))
	assert.Empty(t, reg.Match(ctx, "   "))
}

// AC-5: SkillAdapter maps a SkillDefinition onto tools.ToolDefinition.
func TestSkillAdapterImplementsToolDefinition(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	def := skill.NewSkill(
		"adapter-skill",
		skill.WithDescription("adapter description"),
		skill.WithTools("bash", "read"),
		skill.WithParameters(map[string]any{"attempts": 3}),
		skill.WithPrompt("run the adapter prompt"),
	)
	adapter := skill.NewSkillAdapter(def)

	// tools.ToolDefinition contract.
	assert.Equal(t, "adapter-skill", adapter.Name())
	desc := adapter.Description()
	assert.Contains(t, desc, "adapter description")
	assert.Contains(t, desc, "bash")
	assert.Contains(t, desc, "attempts")

	call := tools.ToolCall{ID: "call-1", Name: "adapter-skill"}
	res, err := adapter.Execute(ctx, call)
	require.NoError(t, err)
	assert.Equal(t, "call-1", res.ToolCallID)
	assert.Contains(t, res.Output, "run the adapter prompt")
	assert.Contains(t, res.Output, "adapter-skill")
}

// AC-6: Execute returns the prompt and lists tools/parameters in metadata.
func TestSkillAdapterExecuteListsToolsAndParams(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	def := skill.NewSkill(
		"meta",
		skill.WithTools("bash", "grep"),
		skill.WithParameters(map[string]any{"max": 10}),
		skill.WithPrompt("the skill prompt"),
	)
	adapter := skill.NewSkillAdapter(def)
	res, err := adapter.Execute(context.Background(), tools.ToolCall{ID: "mc"})
	require.NoError(t, err)

	meta := res.Metadata
	require.Contains(t, meta, "tools")
	toolsList, ok := meta["tools"].([]string)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"bash", "grep"}, toolsList)
	require.Contains(t, meta, "parameters")
	params, ok := meta["parameters"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"max": 10}, params)
}

// AC-7: skill.load span emitted with skill_name / source_path attrs.
func TestLoadEmitsSkillLoadSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	exporter := &captureExporter{}
	ctx := rootContext(exporter)

	dir := t.TempDir()
	path := writeSkillFile(t, dir, "traced.md", `---
name: traced
description: traced skill
---
body
`)

	loader := skill.NewYAMLSkillLoader()
	_, err := loader.Load(ctx, path)
	require.NoError(t, err)

	span := waitForSpan(t, exporter, "skill.load")
	assert.Equal(t, tracing.SpanKindInternal, span.SpanKind)
	assert.Equal(t, tracing.SpanStatusOK, span.Status)
	assert.Equal(t, "traced", attrValue(span, "skill_name"))
	assert.True(t, strings.HasSuffix(attrString(span, "source_path"), "traced.md"))
}

// AC-8: skill.execute span emitted with skill_name / success attrs.
func TestExecuteEmitsSkillExecuteSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	exporter := &captureExporter{}
	ctx := rootContext(exporter)

	def := skill.NewSkill("exec-skill", skill.WithPrompt("p"))
	adapter := skill.NewSkillAdapter(def)
	_, err := adapter.Execute(ctx, tools.ToolCall{ID: "e1"})
	require.NoError(t, err)

	span := waitForSpan(t, exporter, "skill.execute")
	assert.Equal(t, tracing.SpanKindInternal, span.SpanKind)
	assert.Equal(t, "exec-skill", attrValue(span, "skill_name"))
	assert.Equal(t, true, attrValue(span, "success"))

	// A failing execution records success=false.
	_, err = adapter.Execute(context.Background(), tools.ToolCall{})
	require.NoError(t, err)
}

// AC-9: trace_id consistent and parent_span_id traceable across spans.
func TestTraceChainConsistency(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	exporter := &captureExporter{}
	tr := tracing.NewTracer("chain-trace", exporter)
	root, ctx := tr.Start(context.Background(), "test.root", tracing.SpanKindInternal)

	dir := t.TempDir()
	path := writeSkillFile(t, dir, "chain.md", "---\nname: chain\n---\nbody\n")
	loader := skill.NewYAMLSkillLoader()
	_, err := loader.Load(ctx, path)
	require.NoError(t, err)

	span := waitForSpan(t, exporter, "skill.load")
	assert.Equal(t, "chain-trace", span.TraceID)
	assert.Equal(t, root.SpanID(), span.ParentSpanID, "skill.load is a child of the root span")
}

// AC-12: context cancellation propagates through Load / Register / Execute.
func TestContextCancellation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dir := t.TempDir()
	path := writeSkillFile(t, dir, "c.md", "---\nname: c\n---\nbody\n")

	loader := skill.NewYAMLSkillLoader()
	_, err := loader.Load(ctx, path)
	assert.Error(t, err)

	_, err = loader.LoadDir(ctx, dir)
	assert.Error(t, err)

	reg := skill.NewDefaultSkillRegistry()
	err = reg.Register(ctx, skill.NewSkill("c"))
	assert.Error(t, err)
	err = reg.Unregister(ctx, "c")
	assert.Error(t, err)

	adapter := skill.NewSkillAdapter(skill.NewSkill("c", skill.WithPrompt("p")))
	_, err = adapter.Execute(ctx, tools.ToolCall{})
	assert.Error(t, err)
}

// Helpers.

func attrValue(span tracing.SpanData, key string) any {
	for _, a := range span.Attributes {
		if a.Key == key {
			return a.Value
		}
	}
	return nil
}

func attrString(span tracing.SpanData, key string) string {
	v, ok := attrValue(span, key).(string)
	if !ok {
		return ""
	}
	return v
}

func TestYAMLLoaderTabIndentedPromptBlock(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := writeSkillFile(t, dir, "tabbed.md", "---\nname: tabbed\nprompt: |\n\tline one\n\tline two\n---\nbody\n")

	loader := skill.NewYAMLSkillLoader()
	def, err := loader.Load(context.Background(), path)
	require.NoError(t, err)
	d := *def
	assert.Equal(t, "line one\nline two", d.Prompt())
}

func TestYAMLLoaderBlockScalarPreservesBlankLines(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := writeSkillFile(t, dir, "blank.md", "---\nname: blank\nprompt: |\n  para one\n\n  para two\n---\nbody\n")

	loader := skill.NewYAMLSkillLoader()
	def, err := loader.Load(context.Background(), path)
	require.NoError(t, err)
	d := *def
	assert.Equal(t, "para one\n\npara two", d.Prompt())
}

func TestYAMLLoaderCoercesFloatParameters(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := writeSkillFile(t, dir, "floats.md", "---\nname: floats\nparameters:\n  ratio: 0.75\n  factor: 1.5\n---\nbody\n")

	loader := skill.NewYAMLSkillLoader()
	def, err := loader.Load(context.Background(), path)
	require.NoError(t, err)
	d := *def

	ratio, ok := d.Parameters()["ratio"].(float64)
	require.True(t, ok, "ratio should be float64, got %T", d.Parameters()["ratio"])
	assert.Equal(t, 0.75, ratio)

	factor, ok := d.Parameters()["factor"].(float64)
	require.True(t, ok, "factor should be float64, got %T", d.Parameters()["factor"])
	assert.Equal(t, 1.5, factor)
}

func TestYAMLLoaderCoercesBoolParameters(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := writeSkillFile(t, dir, "bools.md", "---\nname: bools\nparameters:\n  enabled: true\n  verbose: false\n---\nbody\n")

	loader := skill.NewYAMLSkillLoader()
	def, err := loader.Load(context.Background(), path)
	require.NoError(t, err)
	d := *def

	enabled, ok := d.Parameters()["enabled"].(bool)
	require.True(t, ok, "enabled should be bool, got %T", d.Parameters()["enabled"])
	assert.True(t, enabled)

	verbose, ok := d.Parameters()["verbose"].(bool)
	require.True(t, ok, "verbose should be bool, got %T", d.Parameters()["verbose"])
	assert.False(t, verbose)
}
