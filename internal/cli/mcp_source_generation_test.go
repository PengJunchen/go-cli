package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPSourceSelectionUsesPresenceAndTrustPrecedence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	projectPath := filepath.Join(root, ".go-cli", "mcp.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(projectPath), 0o700))
	require.NoError(t, os.WriteFile(projectPath, []byte(`{"servers":[]}`), 0o600))
	writeGlobalMCPTestFile(t, `{"servers":[{"name":"global","url":"http://127.0.0.1:9"}]}`)
	changeMCPTestWorkingDirectory(t, root)

	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	trustMCPTestProjectSnapshot(t, root, manager)
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err)
	require.NotNil(t, cfg.VerifiedProjectSources())

	source, err := selectMCPServerSource(context.Background(), cfg, manager)
	require.NoError(t, err)
	require.NotNil(t, source)
	assert.Equal(t, mcpServerSourceProjectFile, source.kind)
	assert.Equal(t, mcpServerSourceIDProjectFile, source.id)
	assert.Empty(t, source.servers, "a present valid empty project source must remain selected")
	assert.NoError(t, source.initialErr)
	assert.True(t, source.reloadable())
}

func TestMCPSourceSelectionMalformedProjectDoesNotFallThroughToGlobalFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	projectPath := filepath.Join(root, ".go-cli", "mcp.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(projectPath), 0o700))
	require.NoError(t, os.WriteFile(projectPath, []byte(`{"servers":[`), 0o600))
	writeGlobalMCPTestFile(t, `{"servers":[{"name":"global","url":"http://127.0.0.1:9"}]}`)
	changeMCPTestWorkingDirectory(t, root)

	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	cfg, err := config.LoadTrustedWithManager(context.Background(), manager)
	require.NoError(t, err, "capture failures should leave a safe global-only config")
	assert.Nil(t, cfg.VerifiedProjectSources())

	source, err := selectMCPServerSource(context.Background(), cfg, manager)
	require.NoError(t, err)
	require.NotNil(t, source)
	assert.Equal(t, mcpServerSourceProjectFile, source.kind)
	assert.NotEqual(t, mcpServerSourceUserGlobalFile, source.kind)
	assert.ErrorIs(t, source.initialErr, errMCPSourceUnverified)
}

func TestMCPSourceSelectionRequiresExplicitInlineProvenance(t *testing.T) {
	cfg := &config.Config{MCP: config.MCPConfig{Servers: []config.MCPServerConfig{{Name: "ambiguous", URL: "http://127.0.0.1:9"}}}}
	source, err := selectMCPServerSource(context.Background(), cfg, nil)
	assert.Nil(t, source)
	assert.ErrorIs(t, err, errMCPSourceConfigUnknown)
}

func TestMCPSourceSelectionAcceptsOnlyUserGlobalInlineConfigAsStatic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalConfigPath := filepath.Join(home, ".go-cli", "config.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(globalConfigPath), 0o700))
	require.NoError(t, os.WriteFile(globalConfigPath, []byte(`{"mcp":{"servers":[{"name":"global","command":"trusted-global"}]}}`), 0o600))

	cfg, err := config.LoadTrustedWithManager(context.Background(), approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore()))
	require.NoError(t, err)
	assert.Equal(t, config.MCPServersFromUserGlobal, cfg.MCPServersProvenance())

	source, err := selectMCPServerSource(context.Background(), cfg, approval.GetTrustManager())
	require.NoError(t, err)
	require.NotNil(t, source)
	assert.Equal(t, mcpServerSourceUserGlobalConfig, source.kind)
	assert.Equal(t, []string{"global"}, mcpConfigServerNames(source.servers))
	assert.False(t, source.reloadable())
}

func TestParseMCPServerConfigBytesPreservesEmptyAndSortsMapFormat(t *testing.T) {
	empty, err := parseMCPServerConfigBytes([]byte(`{"servers":[]}`))
	require.NoError(t, err)
	assert.Empty(t, empty)

	servers, err := parseMCPServerConfigBytes([]byte(`{"mcpServers":{"zeta":{"url":"http://z"},"alpha":{"command":"alpha"}}}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "zeta"}, mcpConfigServerNames(servers))

	_, err = parseMCPServerConfigBytes([]byte(`{"servers":null}`))
	assert.ErrorIs(t, err, errMCPSourceMalformed)
	_, err = parseMCPServerConfigBytes([]byte(`[]`))
	assert.ErrorIs(t, err, errMCPSourceMalformed)
}

func writeGlobalMCPTestFile(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), ".config", "go-cli", "mcp.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func changeMCPTestWorkingDirectory(t *testing.T, path string) {
	t.Helper()
	old, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(path))
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func trustMCPTestProjectSnapshot(t *testing.T, root string, manager approval.TrustManager) {
	t.Helper()
	snapshot, err := config.CaptureProjectSources(context.Background(), root, nil)
	require.NoError(t, err)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(context.Background(), snapshot))
}

func mcpConfigServerNames(servers []config.MCPServerConfig) []string {
	names := make([]string, len(servers))
	for index, server := range servers {
		names[index] = server.Name
	}
	return names
}
