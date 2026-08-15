package cli

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoctorFormat(t *testing.T) {
	checks := []DoctorCheck{
		{Name: "go-version", Status: doctorPass, Message: "ok"},
		{Name: "network", Status: doctorWarn, Message: "offline"},
	}
	out := Format(checks)
	assert.Contains(t, out, "go-version")
	assert.Contains(t, out, "[PASS]")
	assert.Contains(t, out, "[WARN]")
	assert.Contains(t, out, "offline")
}

func TestDoctorRunnerRun(t *testing.T) {
	runner := NewDoctorRunner().WithCheckers([]DoctorChecker{
		&stubChecker{check: DoctorCheck{Name: "stub", Status: doctorPass, Message: "ok"}},
	})
	results := runner.Run(context.Background())
	require.Len(t, results, 1)
	assert.Equal(t, "stub", results[0].Name)
	assert.Equal(t, doctorPass, results[0].Status)
}

// --- GoVersionChecker ---

func TestGoVersionCheckerPass(t *testing.T) {
	c := &GoVersionChecker{version: "go1.24.0"}
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
}

func TestGoVersionCheckerFail(t *testing.T) {
	c := &GoVersionChecker{version: "go1.20.0"}
	chk := c.Check(context.Background())
	assert.Equal(t, doctorFail, chk.Status)
}

func TestGoVersionCheckerWarnUnparsable(t *testing.T) {
	c := &GoVersionChecker{version: "garbage"}
	chk := c.Check(context.Background())
	assert.Equal(t, doctorWarn, chk.Status)
}

func TestParseGoVersion(t *testing.T) {
	major, minor, ok := parseGoVersion("go1.24.0")
	require.True(t, ok)
	assert.Equal(t, 1, major)
	assert.Equal(t, 24, minor)

	_, minor, ok = parseGoVersion("go1.24rc1")
	require.True(t, ok)
	assert.Equal(t, 24, minor)
}

// --- GoModChecker ---

func TestGoModCheckerPass(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/foo\ngo 1.24\n"), 0o600))
	c := NewGoModChecker(dir)
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
}

func TestGoModCheckerWarnMissing(t *testing.T) {
	dir := t.TempDir()
	c := NewGoModChecker(dir)
	chk := c.Check(context.Background())
	assert.Equal(t, doctorWarn, chk.Status)
}

func TestGoModCheckerFailInvalid(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("not a real go mod\n"), 0o600))
	c := NewGoModChecker(dir)
	chk := c.Check(context.Background())
	assert.Equal(t, doctorFail, chk.Status)
}

// --- MakefileChecker ---

func TestMakefileCheckerPass(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Makefile"), []byte("build:\n\tgo build\n"), 0o600))
	c := NewMakefileChecker(dir)
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
}

func TestMakefileCheckerWarnMissing(t *testing.T) {
	dir := t.TempDir()
	c := NewMakefileChecker(dir)
	chk := c.Check(context.Background())
	assert.Equal(t, doctorWarn, chk.Status)
}

// --- ConfigChecker ---

func TestConfigCheckerPass(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"provider":{"name":"openai"}}`), 0o600))
	c := NewConfigChecker(path)
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
}

func TestConfigCheckerWarnMissing(t *testing.T) {
	c := NewConfigChecker(filepath.Join(t.TempDir(), "nope.json"))
	chk := c.Check(context.Background())
	assert.Equal(t, doctorWarn, chk.Status)
}

func TestConfigCheckerFailInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{not json`), 0o600))
	c := NewConfigChecker(path)
	chk := c.Check(context.Background())
	assert.Equal(t, doctorFail, chk.Status)
}

// --- ToolsChecker ---

func TestToolsCheckerPass(t *testing.T) {
	c := NewToolsChecker([]string{"go"})
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
}

func TestToolsCheckerFailMissing(t *testing.T) {
	c := NewToolsChecker([]string{"definitely-not-a-real-tool-xyz"})
	chk := c.Check(context.Background())
	assert.Equal(t, doctorFail, chk.Status)
	assert.Contains(t, chk.Message, "definitely-not-a-real-tool-xyz")
}

// --- PermissionsChecker ---

func TestPermissionsCheckerPass(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
	c := NewPermissionsChecker([]string{path})
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
}

func TestPermissionsCheckerWarnWritable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
	// Chmod explicitly to bypass the process umask so the group-write bit is
	// actually set on disk.
	require.NoError(t, os.Chmod(path, 0o626)) //nolint:gosec
	c := NewPermissionsChecker([]string{path})
	chk := c.Check(context.Background())
	assert.Equal(t, doctorWarn, chk.Status)
}

func TestPermissionsCheckerSkipMissing(t *testing.T) {
	c := NewPermissionsChecker([]string{filepath.Join(t.TempDir(), "absent.json")})
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
}

// --- NetworkChecker ---

func TestNetworkCheckerPassLocal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close() //nolint:errcheck
	c := NewNetworkChecker(ln.Addr().String(), time.Second)
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
}

func TestNetworkCheckerWarnUnreachable(t *testing.T) {
	// Use a port that is almost certainly closed.
	c := NewNetworkChecker("127.0.0.1:1", 200*time.Millisecond)
	chk := c.Check(context.Background())
	assert.Equal(t, doctorWarn, chk.Status)
}

// TestNetworkCheckerProbesProviderHost verifies that when no explicit target
// is given, the checker dials the provider API host derived from the loaded
// config (not 8.8.8.8).
func TestNetworkCheckerProbesProviderHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close() //nolint:errcheck

	c := &NetworkChecker{
		configLoader: func() (*config.Config, error) {
			return &config.Config{
				Provider: config.ProviderConfig{
					BaseURL: "http://" + ln.Addr().String(),
				},
			}, nil
		},
	}
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
	assert.Contains(t, chk.Message, ln.Addr().String())
}

// TestNetworkCheckerDefaultProviderURL verifies that with no config file the
// checker falls back to the default provider base URL and produces a valid
// status (pass or warn depending on network).
func TestNetworkCheckerDefaultProviderURL(t *testing.T) {
	c := &NetworkChecker{
		configLoader: func() (*config.Config, error) { return nil, os.ErrNotExist },
	}
	chk := c.Check(context.Background())
	assert.Contains(t, []string{doctorPass, doctorWarn}, chk.Status)
	// Should NOT reference 8.8.8.8.
	assert.NotContains(t, chk.Message, "8.8.8.8")
}

// --- Provider URL helpers ---

func TestDefaultProviderBaseURL(t *testing.T) {
	assert.Equal(t, "https://api.openai.com/v1", defaultProviderBaseURL("openai"))
	assert.Equal(t, "https://api.anthropic.com/v1", defaultProviderBaseURL("claude"))
	assert.Equal(t, "https://api.anthropic.com/v1", defaultProviderBaseURL("anthropic"))
	assert.Equal(t, "https://generativelanguage.googleapis.com", defaultProviderBaseURL("gemini"))
	assert.Equal(t, "https://api.openai.com/v1", defaultProviderBaseURL("unknown"))
}

func TestResolveProviderBaseURL(t *testing.T) {
	// Explicit BaseURL wins.
	cfg := &config.Config{Provider: config.ProviderConfig{BaseURL: "http://custom", Name: "openai"}}
	assert.Equal(t, "http://custom", resolveProviderBaseURL(cfg))

	// Falls back to default for provider name.
	cfg = &config.Config{Provider: config.ProviderConfig{Name: "claude"}}
	assert.Equal(t, "https://api.anthropic.com/v1", resolveProviderBaseURL(cfg))

	// Nil config → openai default.
	assert.Equal(t, "https://api.openai.com/v1", resolveProviderBaseURL(nil))
}

func TestHostPortFromURL(t *testing.T) {
	hp, err := hostPortFromURL("https://api.openai.com/v1")
	require.NoError(t, err)
	assert.Equal(t, "api.openai.com:443", hp)

	hp, err = hostPortFromURL("http://localhost:8080")
	require.NoError(t, err)
	assert.Equal(t, "localhost:8080", hp)

	hp, err = hostPortFromURL("http://example.com")
	require.NoError(t, err)
	assert.Equal(t, "example.com:80", hp)

	_, err = hostPortFromURL("not-a-url")
	require.Error(t, err)
}

// --- APIConnectivityChecker ---

func TestAPIConnectivityCheckerPass(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &APIConnectivityChecker{
		configLoader: func() (*config.Config, error) {
			return &config.Config{Provider: config.ProviderConfig{BaseURL: srv.URL}}, nil
		},
		httpClient: &http.Client{Timeout: apiConnectivityTimeout},
	}
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
	assert.Contains(t, chk.Message, "reachable")
}

func TestAPIConnectivityCheckerFailUnreachable(t *testing.T) {
	c := &APIConnectivityChecker{
		configLoader: func() (*config.Config, error) {
			return &config.Config{Provider: config.ProviderConfig{BaseURL: "http://127.0.0.1:1"}}, nil
		},
		httpClient: &http.Client{Timeout: 500 * time.Millisecond},
	}
	chk := c.Check(context.Background())
	assert.Equal(t, doctorFail, chk.Status)
	assert.Contains(t, chk.Message, "Advice")
	assert.Contains(t, chk.Message, "base_url")
}

// --- MCPServersChecker ---

func TestMCPServersCheckerNoServers(t *testing.T) {
	c := &MCPServersChecker{
		configLoader: func() (*config.Config, error) { return &config.Config{}, nil },
	}
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
	assert.Contains(t, chk.Message, "no MCP servers configured")
}

func TestMCPServersCheckerFailedServer(t *testing.T) {
	c := &MCPServersChecker{
		configLoader: func() (*config.Config, error) {
			return &config.Config{
				MCP: config.MCPConfig{
					Servers: []config.MCPServerConfig{
						{Name: "bad", URL: "http://127.0.0.1:1"},
					},
				},
			}, nil
		},
	}
	chk := c.Check(context.Background())
	assert.Equal(t, doctorWarn, chk.Status)
	assert.Contains(t, chk.Message, "failed")
	assert.Contains(t, chk.Message, "Advice")
}

// --- LSPChecker ---

func TestLSPCheckerNoServers(t *testing.T) {
	c := &LSPChecker{
		configLoader: func() (*config.Config, error) { return &config.Config{}, nil },
		lookPath:     exec.LookPath,
	}
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
	assert.Contains(t, chk.Message, "no LSP servers configured")
}

func TestLSPCheckerPass(t *testing.T) {
	c := &LSPChecker{
		configLoader: func() (*config.Config, error) {
			return &config.Config{
				LSP: config.LSPConfig{
					Servers: []config.LSPServerConfig{
						{ServerCommand: []string{"go"}},
					},
				},
			}, nil
		},
		lookPath: exec.LookPath,
	}
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
	assert.Contains(t, chk.Message, "found on PATH")
}

func TestLSPCheckerFailMissing(t *testing.T) {
	c := &LSPChecker{
		configLoader: func() (*config.Config, error) {
			return &config.Config{
				LSP: config.LSPConfig{
					Servers: []config.LSPServerConfig{
						{ServerCommand: []string{"gopls-not-real-xyz"}},
					},
				},
			}, nil
		},
		lookPath: exec.LookPath,
	}
	chk := c.Check(context.Background())
	assert.Equal(t, doctorFail, chk.Status)
	assert.Contains(t, chk.Message, "gopls-not-real-xyz")
	assert.Contains(t, chk.Message, "Advice")
	assert.Contains(t, chk.Message, "install")
}

// TestLSPCheckerDedup verifies that the same command appearing in both the
// legacy field and the servers list is only checked once.
func TestLSPCheckerDedup(t *testing.T) {
	c := &LSPChecker{
		configLoader: func() (*config.Config, error) {
			return &config.Config{
				LSP: config.LSPConfig{
					ServerCommand: []string{"go"},
					Servers: []config.LSPServerConfig{
						{ServerCommand: []string{"go"}},
					},
				},
			}, nil
		},
		lookPath: exec.LookPath,
	}
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
	assert.Contains(t, chk.Message, "1 LSP server command(s)")
}

// --- DiskSpaceChecker ---

func TestDiskSpaceCheckerPass(t *testing.T) {
	c := NewDiskSpaceChecker(t.TempDir(), 1) // 1 byte minimum
	chk := c.Check(context.Background())
	assert.Equal(t, doctorPass, chk.Status)
}

func TestDiskSpaceCheckerFail(t *testing.T) {
	// Require an absurd amount of space so it always fails.
	c := NewDiskSpaceChecker(t.TempDir(), ^uint64(0))
	chk := c.Check(context.Background())
	assert.Equal(t, doctorFail, chk.Status)
}

// --- helpers ---

type stubChecker struct {
	check DoctorCheck
}

func (s *stubChecker) Check(_ context.Context) DoctorCheck { return s.check }

// --- OSInfoChecker ---

// TestDoctorDarwinOSInfo verifies that getOSInfo returns a non-empty string
// containing "macOS" when running on Darwin.
func TestDoctorDarwinOSInfo(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("test only runs on darwin")
	}
	info := getOSInfo()
	assert.NotEmpty(t, info)
	assert.Contains(t, strings.ToLower(info), "macos")
}

// TestDoctorCheckAllPlatforms verifies that all default doctor checks can
// run on any platform without panicking and each returns a valid status.
func TestDoctorCheckAllPlatforms(t *testing.T) {
	runner := NewDoctorRunner()
	// Bound the run so network-bound checks (api-connectivity, mcp-servers)
	// don't hang the test suite in offline environments.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	results := runner.Run(ctx)
	assert.NotEmpty(t, results)
	for _, r := range results {
		assert.Contains(t, []string{doctorPass, doctorWarn, doctorFail}, r.Status,
			"check %q returned invalid status %q", r.Name, r.Status)
		assert.NotEmpty(t, r.Name)
	}
}

// TestDoctorRunnerIncludesNewChecks verifies that the default runner registers
// the api-connectivity, mcp-servers, and lsp check items.
func TestDoctorRunnerIncludesNewChecks(t *testing.T) {
	runner := NewDoctorRunner()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	results := runner.Run(ctx)
	names := make(map[string]bool, len(results))
	for _, r := range results {
		names[r.Name] = true
	}
	assert.True(t, names["api-connectivity"], "api-connectivity check should be registered")
	assert.True(t, names["mcp-servers"], "mcp-servers check should be registered")
	assert.True(t, names["lsp"], "lsp check should be registered")
}

// TestDoctorMissingToolGraceful verifies that a missing required tool is
// reported with a fail status (not a panic) and the message identifies the
// missing tool.
func TestDoctorMissingToolGraceful(t *testing.T) {
	assert.NotPanics(t, func() {
		c := NewToolsChecker([]string{"definitely-not-a-real-tool-xyz"})
		chk := c.Check(context.Background())
		assert.Equal(t, doctorFail, chk.Status)
		assert.Contains(t, chk.Message, "missing tools")
		assert.Contains(t, chk.Message, "definitely-not-a-real-tool-xyz")
	})
}
