package tracing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildSpanTreeLinksChildren(t *testing.T) {
	spans := []SpanData{
		{SpanID: "1", ParentSpanID: "", StartTime: "2026-01-01T00:00:00Z"},
		{SpanID: "2", ParentSpanID: "1", StartTime: "2026-01-01T00:00:02Z"},
		{SpanID: "3", ParentSpanID: "1", StartTime: "2026-01-01T00:00:01Z"},
	}
	root, err := buildSpanTree(spans)
	require.NoError(t, err)
	require.NotNil(t, root)
	assert.Equal(t, "1", root.Span.SpanID)
	require.Len(t, root.Children, 2)
}

func TestBuildSpanTreeEmptyIsNil(t *testing.T) {
	root, err := buildSpanTree(nil)
	require.NoError(t, err)
	assert.Nil(t, root)
}

func TestLoadTraceRebuildsTree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	lines := []string{
		`{"trace_id":"t","span_id":"1","name":"root","start_time":"2026-01-01T00:00:00Z","end_time":"2026-01-01T00:00:01Z"}`,
		`{"trace_id":"t","span_id":"2","parent_span_id":"1","name":"child","start_time":"2026-01-01T00:00:01Z","end_time":"2026-01-01T00:00:02Z"}`,
		`this is not json`, // must be skipped
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600))

	root, err := LoadTrace(path)
	require.NoError(t, err)
	require.NotNil(t, root)
	assert.Equal(t, "1", root.Span.SpanID)
	require.Len(t, root.Children, 1)
	assert.Equal(t, "2", root.Children[0].Span.SpanID)
}

func TestLoadTraceMissingFile(t *testing.T) {
	_, err := LoadTrace(filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	require.Error(t, err)
}

func TestPrintTreeDoesNotPanic(t *testing.T) {
	root := &SpanNode{Span: SpanData{SpanID: "1", Name: "root", Status: SpanStatusError, StatusMessage: "boom"}}
	child := &SpanNode{Span: SpanData{SpanID: "2", ParentSpanID: "1", Name: "child", Status: SpanStatusOK}}
	root.Children = []*SpanNode{child}

	PrintTree(root, "")
	PrintTree(nil, "")
}
