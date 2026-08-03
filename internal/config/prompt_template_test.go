package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPromptTemplateLoad(t *testing.T) {
	dir := t.TempDir()
	body := "Hello {{.name}}, you are a {{.role}}."
	require.NoError(t, os.WriteFile(filepath.Join(dir, "greet.tmpl"), []byte(body), 0o644))

	l := NewPromptTemplateLoader(dir)
	tmpl, err := l.Load("greet")
	require.NoError(t, err)
	assert.Equal(t, "greet", tmpl.Name)
	assert.Equal(t, body, tmpl.Template)
	assert.ElementsMatch(t, []string{"name", "role"}, tmpl.Parameters)
}

func TestPromptTemplateLoadWithDescription(t *testing.T) {
	dir := t.TempDir()
	body := "# description: A greeting template\nHello {{.name}}!"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "greet.tmpl"), []byte(body), 0o644))

	l := NewPromptTemplateLoader(dir)
	tmpl, err := l.Load("greet")
	require.NoError(t, err)
	assert.Equal(t, "A greeting template", tmpl.Description)
	assert.Equal(t, "Hello {{.name}}!", tmpl.Template)
	assert.ElementsMatch(t, []string{"name"}, tmpl.Parameters)
}

func TestPromptTemplateLoadMissing(t *testing.T) {
	l := NewPromptTemplateLoader(t.TempDir())
	_, err := l.Load("nope")
	require.Error(t, err)
}

func TestPromptTemplateLoadAll(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.tmpl"), []byte("A {{.x}}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.tmpl"), []byte("B {{.y}}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notmpl.txt"), []byte("ignore"), 0o644))

	l := NewPromptTemplateLoader(dir)
	all, err := l.LoadAll()
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, "a", all[0].Name)
	assert.Equal(t, "b", all[1].Name)
}

func TestPromptTemplateLoadAllMissingDir(t *testing.T) {
	l := NewPromptTemplateLoader(filepath.Join(t.TempDir(), "missing"))
	all, err := l.LoadAll()
	require.NoError(t, err)
	assert.Empty(t, all)
}

func TestPromptTemplateRender(t *testing.T) {
	l := NewPromptTemplateLoader(t.TempDir())
	tmpl := &PromptTemplate{
		Name:     "greet",
		Template: "Hello {{.name}}, you are a {{.role}}.",
	}
	out, err := l.Render(tmpl, map[string]string{"name": "Alice", "role": "dev"})
	require.NoError(t, err)
	assert.Equal(t, "Hello Alice, you are a dev.", out)
}

func TestPromptTemplateRenderMissingParam(t *testing.T) {
	l := NewPromptTemplateLoader(t.TempDir())
	tmpl := &PromptTemplate{
		Name:     "greet",
		Template: "Hello {{.name}}.",
	}
	// Missing param renders as empty.
	out, err := l.Render(tmpl, map[string]string{})
	require.NoError(t, err)
	assert.Equal(t, "Hello .", out)
}

func TestPromptTemplateRenderInvalidTemplate(t *testing.T) {
	l := NewPromptTemplateLoader(t.TempDir())
	tmpl := &PromptTemplate{
		Name:     "bad",
		Template: "Hello {{.name",
	}
	_, err := l.Render(tmpl, nil)
	require.Error(t, err)
}

func TestExtractParametersDedupes(t *testing.T) {
	params := extractParameters("{{.a}} and {{.b}} and {{.a}} again")
	assert.Equal(t, []string{"a", "b"}, params)
}
