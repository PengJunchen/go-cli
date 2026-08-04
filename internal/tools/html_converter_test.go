package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTMLConverterCompileTimeCheck(t *testing.T) {
	var c HTMLToMarkdownConverter = NewDefaultHTMLConverter()
	assert.NotNil(t, c)
}

func TestHTMLConverterH1(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<h1>Title</h1>")
	require.NoError(t, err)
	assert.Equal(t, "# Title", out)
}

func TestHTMLConverterH2(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<h2>Section</h2>")
	require.NoError(t, err)
	assert.Equal(t, "## Section", out)
}

func TestHTMLConverterAllHeadings(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<h1>One</h1><h2>Two</h2><h3>Three</h3><h4>Four</h4><h5>Five</h5><h6>Six</h6>")
	require.NoError(t, err)
	assert.Contains(t, out, "# One")
	assert.Contains(t, out, "## Two")
	assert.Contains(t, out, "### Three")
	assert.Contains(t, out, "#### Four")
	assert.Contains(t, out, "##### Five")
	assert.Contains(t, out, "###### Six")
}

func TestHTMLConverterLink(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert(`<a href="http://example.com">link</a>`)
	require.NoError(t, err)
	assert.Equal(t, "[link](http://example.com)", out)
}

func TestHTMLConverterPreCode(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<pre><code>code block</code></pre>")
	require.NoError(t, err)
	assert.Equal(t, "```\ncode block\n```", out)
}

func TestHTMLConverterInlineCode(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<code>inline</code>")
	require.NoError(t, err)
	assert.Equal(t, "`inline`", out)
}

func TestHTMLConverterUnorderedList(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<ul><li>item1</li><li>item2</li></ul>")
	require.NoError(t, err)
	assert.Equal(t, "- item1\n- item2", out)
}

func TestHTMLConverterOrderedList(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<ol><li>first</li><li>second</li></ol>")
	require.NoError(t, err)
	assert.Equal(t, "1. first\n2. second", out)
}

func TestHTMLConverterParagraph(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<p>paragraph</p>")
	require.NoError(t, err)
	assert.Equal(t, "paragraph", out)
}

func TestHTMLConverterParagraphSeparation(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<p>one</p><p>two</p>")
	require.NoError(t, err)
	assert.Equal(t, "one\n\ntwo", out)
}

func TestHTMLConverterBlockquote(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<blockquote>quote</blockquote>")
	require.NoError(t, err)
	assert.Equal(t, "> quote", out)
}

func TestHTMLConverterBold(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<strong>bold</strong>")
	require.NoError(t, err)
	assert.Equal(t, "**bold**", out)
}

func TestHTMLConverterBoldBTag(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<b>bold</b>")
	require.NoError(t, err)
	assert.Equal(t, "**bold**", out)
}

func TestHTMLConverterItalic(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<em>italic</em>")
	require.NoError(t, err)
	assert.Equal(t, "*italic*", out)
}

func TestHTMLConverterItalicITag(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<i>italic</i>")
	require.NoError(t, err)
	assert.Equal(t, "*italic*", out)
}

func TestHTMLConverterBreak(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("line one<br>line two")
	require.NoError(t, err)
	assert.Equal(t, "line one\nline two", out)
}

func TestHTMLConverterHr(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("<p>above</p><hr><p>below</p>")
	require.NoError(t, err)
	assert.Contains(t, out, "---")
	assert.Contains(t, out, "above")
	assert.Contains(t, out, "below")
}

func TestHTMLConverterRemovesScript(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert(`<p>before</p><script>alert('xss')</script><p>after</p>`)
	require.NoError(t, err)
	assert.NotContains(t, out, "alert")
	assert.NotContains(t, out, "<script>")
	assert.Contains(t, out, "before")
	assert.Contains(t, out, "after")
}

func TestHTMLConverterRemovesStyle(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert(`<p>text</p><style>body { color: red; }</style>`)
	require.NoError(t, err)
	assert.NotContains(t, out, "color")
	assert.NotContains(t, out, "<style>")
	assert.Contains(t, out, "text")
}

func TestHTMLConverterRemovesNavFooter(t *testing.T) {
	c := NewDefaultHTMLConverter()
	html := `<nav><a href="/home">Home</a></nav><main><p>content</p></main><footer>copyright</footer>`
	out, err := c.Convert(html)
	require.NoError(t, err)
	assert.NotContains(t, out, "Home")
	assert.NotContains(t, out, "copyright")
	assert.Contains(t, out, "content")
}

func TestHTMLConverterArticlePriority(t *testing.T) {
	c := NewDefaultHTMLConverter()
	html := `<html><body><article><p>article content</p></article><p>body only</p></body></html>`
	out, err := c.Convert(html)
	require.NoError(t, err)
	assert.Contains(t, out, "article content")
	assert.NotContains(t, out, "body only")
}

func TestHTMLConverterFullDocument(t *testing.T) {
	c := NewDefaultHTMLConverter()
	html := `<!DOCTYPE html><html><head><title>Page</title></head><body><h1>Hello</h1><p>World</p></body></html>`
	out, err := c.Convert(html)
	require.NoError(t, err)
	assert.Contains(t, out, "# Hello")
	assert.Contains(t, out, "World")
	assert.NotContains(t, out, "<html>")
	assert.NotContains(t, out, "<head>")
	assert.NotContains(t, out, "<title>")
}

func TestHTMLConverterNonHTML(t *testing.T) {
	c := NewDefaultHTMLConverter()
	out, err := c.Convert("just plain text here")
	require.NoError(t, err)
	assert.Equal(t, "just plain text here", out)
}

func TestHTMLConverterTruncation(t *testing.T) {
	c := NewDefaultHTMLConverter(WithMaxLines(10))
	var html strings.Builder
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&html, "<h2>Heading %d</h2>", i)
	}
	out, err := c.Convert(html.String())
	require.NoError(t, err)
	lines := strings.Split(out, "\n")
	assert.LessOrEqual(t, len(lines), 10)
	assert.Greater(t, len(lines), 0)
	assert.Contains(t, out, "Heading 1")
	assert.NotContains(t, out, "Heading 25")
}

func TestHTMLConverterTruncationNoCodeBlockBreak(t *testing.T) {
	// A code block spanning many lines must not be left unclosed by truncation.
	c := NewDefaultHTMLConverter(WithMaxLines(4))
	var code strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&code, "line %d\n", i)
	}
	html := "<pre><code>" + code.String() + "</code></pre>"
	out, err := c.Convert(html)
	require.NoError(t, err)
	// The output must not contain an opening fence without a closing one.
	fences := strings.Count(out, "```")
	assert.Equal(t, 0, fences%2, "code block fences must be balanced after truncation")
}

func TestWebFetchHTMLConversion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<h1>Hi</h1>"))
	}))
	defer srv.Close()

	tool := NewWebFetchTool(WithHTMLConverter(NewDefaultHTMLConverter()))
	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-1",
		Name: "web_fetch",
		Args: map[string]any{"url": srv.URL},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "# Hi")
	assert.Equal(t, "call-1", res.ToolCallID)
}

func TestWebFetchNonHTMLNoConversion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("raw bytes"))
	}))
	defer srv.Close()

	tool := NewWebFetchTool(WithHTMLConverter(NewDefaultHTMLConverter()))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"url": srv.URL},
	})
	require.NoError(t, err)
	assert.Equal(t, "raw bytes", res.Output)
}

func TestWebFetchNoConverterRawHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<h1>Hi</h1>"))
	}))
	defer srv.Close()

	// Without a converter the tool returns the raw body (backward compatible).
	tool := NewWebFetchTool()
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"url": srv.URL},
	})
	require.NoError(t, err)
	assert.Equal(t, "<h1>Hi</h1>", res.Output)
}
