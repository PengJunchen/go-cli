package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// MultiLSPClient routes LSP requests to per-language clients based on the
// file extension in the document URI. It implements LSPClient by delegating
// every call to the client selected by Route. When no extension matches, the
// defaultClient (the first registered client) is used.
type MultiLSPClient struct {
	clients       map[string]LSPClient // file extension -> client
	defaultClient LSPClient
	mu            sync.RWMutex
}

var _ LSPClient = (*MultiLSPClient)(nil)

// NewMultiLSPClient creates an empty MultiLSPClient. Use Register to add
// clients for specific file extensions.
func NewMultiLSPClient() *MultiLSPClient {
	return &MultiLSPClient{
		clients: make(map[string]LSPClient),
	}
}

// Register associates the given file extensions with a client. The first
// client registered becomes the default used when no extension matches.
func (m *MultiLSPClient) Register(client LSPClient, extensions ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.defaultClient == nil {
		m.defaultClient = client
	}
	for _, ext := range extensions {
		m.clients[ext] = client
	}
}

// SetDefaultClient sets the fallback client used when no extension matches.
func (m *MultiLSPClient) SetDefaultClient(client LSPClient) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultClient = client
}

// Route returns the LSPClient for the given document URI based on its file
// extension. When no extension matches, the default client is returned. When
// no clients are registered, nil is returned.
func (m *MultiLSPClient) Route(uri string) LSPClient {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ext := extractExtension(uri)
	if ext != "" {
		if c, ok := m.clients[ext]; ok {
			return c
		}
	}
	return m.defaultClient
}

// extractExtension returns the lowercase file extension (without the dot)
// from a URI or file path. For example, "file:///a/b/c.go" returns "go".
func extractExtension(uri string) string {
	// Strip query and fragment if present.
	if idx := strings.IndexAny(uri, "?#"); idx >= 0 {
		uri = uri[:idx]
	}
	idx := strings.LastIndex(uri, ".")
	if idx < 0 {
		return ""
	}
	return strings.ToLower(uri[idx+1:])
}

// Initialize sends the initialize request to the default client. In a
// multi-server setup, each client should be initialized individually before
// being registered.
func (m *MultiLSPClient) Initialize(ctx context.Context, rootURI string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.defaultClient == nil {
		return fmt.Errorf("lsp multi: no clients registered")
	}
	return m.defaultClient.Initialize(ctx, rootURI)
}

// Definition delegates to the client routed by URI.
func (m *MultiLSPClient) Definition(ctx context.Context, uri string, line, character int) ([]Location, error) {
	c := m.Route(uri)
	if c == nil {
		return nil, fmt.Errorf("lsp multi: no client for uri %q", uri)
	}
	return c.Definition(ctx, uri, line, character)
}

// References delegates to the client routed by URI.
func (m *MultiLSPClient) References(ctx context.Context, uri string, line, character int) ([]Location, error) {
	c := m.Route(uri)
	if c == nil {
		return nil, fmt.Errorf("lsp multi: no client for uri %q", uri)
	}
	return c.References(ctx, uri, line, character)
}

// Hover delegates to the client routed by URI.
func (m *MultiLSPClient) Hover(ctx context.Context, uri string, line, character int) (string, error) {
	c := m.Route(uri)
	if c == nil {
		return "", fmt.Errorf("lsp multi: no client for uri %q", uri)
	}
	return c.Hover(ctx, uri, line, character)
}

// Diagnostics delegates to the client routed by URI.
func (m *MultiLSPClient) Diagnostics(ctx context.Context, uri string) ([]Diagnostic, error) {
	c := m.Route(uri)
	if c == nil {
		return nil, fmt.Errorf("lsp multi: no client for uri %q", uri)
	}
	return c.Diagnostics(ctx, uri)
}

// DidOpen delegates to the client routed by URI.
func (m *MultiLSPClient) DidOpen(ctx context.Context, uri string, content string, version int) error {
	c := m.Route(uri)
	if c == nil {
		return fmt.Errorf("lsp multi: no client for uri %q", uri)
	}
	return c.DidOpen(ctx, uri, content, version)
}

// DidChange delegates to the client routed by URI.
func (m *MultiLSPClient) DidChange(ctx context.Context, uri string, content string, version int) error {
	c := m.Route(uri)
	if c == nil {
		return fmt.Errorf("lsp multi: no client for uri %q", uri)
	}
	return c.DidChange(ctx, uri, content, version)
}

// Completion delegates to the client routed by URI.
func (m *MultiLSPClient) Completion(ctx context.Context, uri string, line, character int) ([]CompletionItem, error) {
	c := m.Route(uri)
	if c == nil {
		return nil, fmt.Errorf("lsp multi: no client for uri %q", uri)
	}
	return c.Completion(ctx, uri, line, character)
}

// TypeDefinition delegates to the client routed by URI.
func (m *MultiLSPClient) TypeDefinition(ctx context.Context, uri string, line, character int) ([]Location, error) {
	c := m.Route(uri)
	if c == nil {
		return nil, fmt.Errorf("lsp multi: no client for uri %q", uri)
	}
	return c.TypeDefinition(ctx, uri, line, character)
}

// Rename delegates to the client routed by URI.
func (m *MultiLSPClient) Rename(ctx context.Context, uri string, line, character int, newName string) (*WorkspaceEdit, error) {
	c := m.Route(uri)
	if c == nil {
		return nil, fmt.Errorf("lsp multi: no client for uri %q", uri)
	}
	return c.Rename(ctx, uri, line, character, newName)
}

// WorkspaceSymbol delegates to the default client. Workspace symbol queries
// are not document-specific, so routing by URI is not applicable.
func (m *MultiLSPClient) WorkspaceSymbol(ctx context.Context, query string) ([]SymbolInformation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.defaultClient == nil {
		return nil, fmt.Errorf("lsp multi: no clients registered")
	}
	return m.defaultClient.WorkspaceSymbol(ctx, query)
}

// Shutdown sends shutdown to all registered clients.
func (m *MultiLSPClient) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	clients := make(map[LSPClient]struct{}, len(m.clients)+1)
	if m.defaultClient != nil {
		clients[m.defaultClient] = struct{}{}
	}
	for _, c := range m.clients {
		clients[c] = struct{}{}
	}
	m.mu.Unlock()

	var firstErr error
	for c := range clients {
		if err := c.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
