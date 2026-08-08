package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// mockLSPClient is a minimal LSPClient mock for LSPCompleter tests.
type mockLSPClient struct {
	didOpenErr    error
	completionErr error
	items         []tools.CompletionItem
	openedURI     string
	openedContent string
}

func (m *mockLSPClient) Initialize(_ context.Context, _ string) error { return nil }
func (m *mockLSPClient) Definition(_ context.Context, _ string, _, _ int) ([]tools.Location, error) {
	return nil, nil
}
func (m *mockLSPClient) References(_ context.Context, _ string, _, _ int) ([]tools.Location, error) {
	return nil, nil
}
func (m *mockLSPClient) Hover(_ context.Context, _ string, _, _ int) (string, error) {
	return "", nil
}
func (m *mockLSPClient) Diagnostics(_ context.Context, _ string) ([]tools.Diagnostic, error) {
	return nil, nil
}
func (m *mockLSPClient) DidOpen(_ context.Context, uri, content string, _ int) error {
	if m.didOpenErr != nil {
		return m.didOpenErr
	}
	m.openedURI = uri
	m.openedContent = content
	return nil
}
func (m *mockLSPClient) DidChange(_ context.Context, _, _ string, _ int) error { return nil }
func (m *mockLSPClient) Completion(_ context.Context, _ string, _, _ int) ([]tools.CompletionItem, error) {
	if m.completionErr != nil {
		return nil, m.completionErr
	}
	return m.items, nil
}
func (m *mockLSPClient) TypeDefinition(_ context.Context, _ string, _, _ int) ([]tools.Location, error) {
	return nil, nil
}
func (m *mockLSPClient) Rename(_ context.Context, _ string, _, _ int, _ string) (*tools.WorkspaceEdit, error) {
	return nil, nil
}
func (m *mockLSPClient) Shutdown(_ context.Context) error { return nil }

var _ tools.LSPClient = (*mockLSPClient)(nil)

func TestLSPCompleter_NilClient(t *testing.T) {
	c := NewLSPCompleter(nil, "/tmp")
	got, start := c.Complete("fmt.Pr", 6)
	if got != nil || start != 0 {
		t.Fatalf("expected nil,0; got %v,%d", got, start)
	}
}

func TestLSPCompleter_NilReceiver(t *testing.T) {
	var c *LSPCompleter
	got, start := c.Complete("test", 4)
	if got != nil || start != 0 {
		t.Fatalf("expected nil,0; got %v,%d", got, start)
	}
}

func TestLSPCompleter_DidOpenError(t *testing.T) {
	client := &mockLSPClient{didOpenErr: errors.New("boom")}
	c := NewLSPCompleter(client, "/tmp")
	got, start := c.Complete("fmt.Pr", 6)
	if got != nil || start != 0 {
		t.Fatalf("expected nil,0 on DidOpen error; got %v,%d", got, start)
	}
}

func TestLSPCompleter_CompletionError(t *testing.T) {
	client := &mockLSPClient{completionErr: errors.New("boom")}
	c := NewLSPCompleter(client, "/tmp")
	got, start := c.Complete("fmt.Pr", 6)
	if got != nil || start != 0 {
		t.Fatalf("expected nil,0 on Completion error; got %v,%d", got, start)
	}
}

func TestLSPCompleter_Success(t *testing.T) {
	client := &mockLSPClient{
		items: []tools.CompletionItem{
			{Label: "Println", Detail: "func(args ...any)"},
			{Label: "Printf", Detail: "func(format string, args ...any)"},
		},
	}
	c := NewLSPCompleter(client, "/tmp")
	got, start := c.Complete("fmt.Pr", 6)
	if len(got) != 2 {
		t.Fatalf("expected 2 completions; got %d", len(got))
	}
	if got[0].Text != "Println" || got[0].Description != "func(args ...any)" {
		t.Errorf("unexpected first completion: %+v", got[0])
	}
	if got[1].Text != "Printf" {
		t.Errorf("unexpected second completion: %+v", got[1])
	}
	// "fmt.Pr" — start should be after the '.', i.e. at index 4 ("Pr")
	if start != 4 {
		t.Errorf("expected start=4 (after '.'), got %d", start)
	}
}

func TestLSPCompleter_QualifiedNameWordBoundary(t *testing.T) {
	client := &mockLSPClient{
		items: []tools.CompletionItem{{Label: "Println"}},
	}
	c := NewLSPCompleter(client, "/tmp")
	// "fmt.Pr" — cursor at end (pos=6), word boundary should be at 4 (after '.')
	_, start := c.Complete("fmt.Pr", 6)
	if start != 4 {
		t.Errorf("expected start=4 for 'fmt.Pr'; got %d", start)
	}
}

func TestLSPCompleter_UnqualifiedWordBoundary(t *testing.T) {
	client := &mockLSPClient{
		items: []tools.CompletionItem{{Label: "Println"}},
	}
	c := NewLSPCompleter(client, "/tmp")
	// "Pr" — cursor at end (pos=2), no delimiter, start should be 0
	_, start := c.Complete("Pr", 2)
	if start != 0 {
		t.Errorf("expected start=0 for 'Pr'; got %d", start)
	}
}

func TestLSPCompleter_EmptyInput(t *testing.T) {
	client := &mockLSPClient{
		items: []tools.CompletionItem{{Label: "func"}},
	}
	c := NewLSPCompleter(client, "/tmp")
	got, start := c.Complete("", 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 completion; got %d", len(got))
	}
	if start != 0 {
		t.Errorf("expected start=0 for empty input; got %d", start)
	}
}

func TestLSPCompleter_PosClamped(t *testing.T) {
	client := &mockLSPClient{
		items: []tools.CompletionItem{{Label: "test"}},
	}
	c := NewLSPCompleter(client, "/tmp")
	// pos beyond input length should be clamped
	got, _ := c.Complete("abc", 100)
	if got == nil {
		t.Fatal("expected completions despite out-of-range pos")
	}
}

func TestLSPCompleter_DidOpenReceivesInput(t *testing.T) {
	client := &mockLSPClient{
		items: []tools.CompletionItem{{Label: "x"}},
	}
	c := NewLSPCompleter(client, "/tmp")
	c.Complete("fmt.Println", 11)
	if client.openedContent != "fmt.Println" {
		t.Errorf("DidOpen did not receive input text; got %q", client.openedContent)
	}
}
