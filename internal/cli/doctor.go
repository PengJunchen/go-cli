// Package cli provides the core CLI execution framework.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// Doctor check statuses.
const (
	doctorPass = "pass"
	doctorWarn = "warn"
	doctorFail = "fail"
)

// DoctorCheck represents a single diagnostic check.
type DoctorCheck struct {
	Name    string
	Status  string // "pass", "warn", "fail"
	Message string
}

// DoctorChecker performs a single diagnostic.
type DoctorChecker interface {
	Check(ctx context.Context) DoctorCheck
}

// DoctorRunner runs all diagnostic checks.
type DoctorRunner struct {
	checks []DoctorChecker
}

// NewDoctorRunner returns a DoctorRunner with all default checkers registered.
func NewDoctorRunner() *DoctorRunner {
	return &DoctorRunner{checks: []DoctorChecker{
		NewGoVersionChecker(),
		NewGoModChecker(""),
		NewMakefileChecker(""),
		NewConfigChecker(""),
		NewToolsChecker(nil),
		NewPermissionsChecker(nil),
		NewNetworkChecker("", 0),
		NewAPIConnectivityChecker(),
		NewMCPServersChecker(),
		NewLSPChecker(),
		NewDiskSpaceChecker("", 0),
		NewOSInfoChecker(),
	}}
}

// WithCheckers replaces the checker list. Intended for testing.
func (r *DoctorRunner) WithCheckers(checks []DoctorChecker) *DoctorRunner {
	r.checks = append([]DoctorChecker(nil), checks...)
	return r
}

// Run executes all registered checks and returns their results.
func (r *DoctorRunner) Run(ctx context.Context) []DoctorCheck {
	span, _ := tracing.SpanFromContext(ctx, "cli.doctor.run", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())

	results := make([]DoctorCheck, 0, len(r.checks))
	for _, c := range r.checks {
		chk := c.Check(ctx)
		logger.DebugContext(ctx, "cli.doctor.check",
			"name", chk.Name,
			"status", chk.Status,
		)
		span.AddEvent("check", tracing.Attribute{Key: "name", Value: chk.Name},
			tracing.Attribute{Key: "status", Value: chk.Status})
		results = append(results, chk)
	}
	span.SetAttributes(tracing.Attribute{Key: "checks", Value: len(results)})
	return results
}

// Format renders the checks as a human-readable table.
func Format(checks []DoctorCheck) string {
	var b strings.Builder
	for _, c := range checks {
		fmt.Fprintf(&b, "%-16s [%s]  %s\n", c.Name, strings.ToUpper(c.Status), c.Message) //nolint:errcheck
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// GoVersionChecker
// ---------------------------------------------------------------------------

// minGoMajor/minGoMinor encode the minimum supported Go version (1.24, per
// go.mod).
const (
	minGoMajor = 1
	minGoMinor = 24
)

// GoVersionChecker verifies the running Go version meets the minimum.
type GoVersionChecker struct {
	version string // override for testing; empty means runtime.Version()
}

// NewGoVersionChecker returns a checker using the current runtime version.
func NewGoVersionChecker() *GoVersionChecker {
	return &GoVersionChecker{}
}

// Check implements DoctorChecker.
func (c *GoVersionChecker) Check(_ context.Context) DoctorCheck {
	ver := c.version
	if ver == "" {
		ver = runtime.Version()
	}
	major, minor, ok := parseGoVersion(ver)
	if !ok {
		return DoctorCheck{Name: "go-version", Status: doctorWarn,
			Message: fmt.Sprintf("could not parse Go version %q", ver)}
	}
	if major > minGoMajor || (major == minGoMajor && minor >= minGoMinor) {
		return DoctorCheck{Name: "go-version", Status: doctorPass,
			Message: fmt.Sprintf("Go version %s meets minimum go%d.%d", ver, minGoMajor, minGoMinor)}
	}
	return DoctorCheck{Name: "go-version", Status: doctorFail,
		Message: fmt.Sprintf("Go version %s is below minimum go%d.%d", ver, minGoMajor, minGoMinor)}
}

// parseGoVersion parses a version string like "go1.24.0" or "go1.24rc1" into
// major and minor integers.
func parseGoVersion(ver string) (int, int, bool) {
	ver = strings.TrimPrefix(ver, "go")
	parts := strings.SplitN(ver, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	// The minor part may have a suffix like "rc1"; trim non-digit chars.
	minorStr := parts[1]
	for i, r := range minorStr {
		if r < '0' || r > '9' {
			minorStr = minorStr[:i]
			break
		}
	}
	minor, err := strconv.Atoi(minorStr)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// ---------------------------------------------------------------------------
// GoModChecker
// ---------------------------------------------------------------------------

// GoModChecker verifies that a go.mod file exists and is syntactically valid.
type GoModChecker struct {
	dir string // directory to check; empty means CWD
}

// NewGoModChecker returns a checker for dir. When dir is empty the CWD is used.
func NewGoModChecker(dir string) *GoModChecker {
	return &GoModChecker{dir: dir}
}

// Check implements DoctorChecker.
func (c *GoModChecker) Check(_ context.Context) DoctorCheck {
	dir := c.dir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return DoctorCheck{Name: "go-mod", Status: doctorFail, Message: "cannot determine working directory: " + err.Error()}
		}
	}
	path := findUp(dir, "go.mod")
	if path == "" {
		return DoctorCheck{Name: "go-mod", Status: doctorWarn, Message: "go.mod not found under " + dir}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return DoctorCheck{Name: "go-mod", Status: doctorFail, Message: "cannot read go.mod: " + err.Error()}
	}
	if !goModIsValid(string(data)) {
		return DoctorCheck{Name: "go-mod", Status: doctorFail, Message: "go.mod at " + path + " is missing module/go directive"}
	}
	return DoctorCheck{Name: "go-mod", Status: doctorPass, Message: "go.mod found at " + path}
}

// goModIsValid returns true when content contains both a module directive and
// a go version directive.
func goModIsValid(content string) bool {
	hasModule := false
	hasGo := false
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			hasModule = true
		}
		if strings.HasPrefix(line, "go ") {
			hasGo = true
		}
	}
	return hasModule && hasGo
}

// findUp searches for a file named name starting at dir and walking up to the
// filesystem root. It returns the first match or "" when not found.
func findUp(dir, name string) string {
	dir = filepath.Clean(dir)
	for {
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// ---------------------------------------------------------------------------
// MakefileChecker
// ---------------------------------------------------------------------------

// MakefileChecker verifies that a Makefile exists in a directory.
type MakefileChecker struct {
	dir string // directory to check; empty means CWD
}

// NewMakefileChecker returns a checker for dir. When dir is empty the CWD is
// used.
func NewMakefileChecker(dir string) *MakefileChecker {
	return &MakefileChecker{dir: dir}
}

// Check implements DoctorChecker.
func (c *MakefileChecker) Check(_ context.Context) DoctorCheck {
	dir := c.dir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return DoctorCheck{Name: "makefile", Status: doctorFail, Message: "cannot determine working directory: " + err.Error()}
		}
	}
	for _, name := range []string{"GNUmakefile", "makefile", "Makefile"} {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return DoctorCheck{Name: "makefile", Status: doctorPass, Message: name + " found at " + path}
		}
	}
	return DoctorCheck{Name: "makefile", Status: doctorWarn, Message: "no Makefile found in " + dir}
}

// ---------------------------------------------------------------------------
// ConfigChecker
// ---------------------------------------------------------------------------

// ConfigChecker verifies that the configuration file (when present) is valid.
type ConfigChecker struct {
	path string // explicit config path; empty means use config.Load()
}

// NewConfigChecker returns a checker for path. When path is empty the default
// config probing (config.Load) is used.
func NewConfigChecker(path string) *ConfigChecker {
	return &ConfigChecker{path: path}
}

// Check implements DoctorChecker.
func (c *ConfigChecker) Check(ctx context.Context) DoctorCheck {
	if c.path != "" {
		if _, err := os.Stat(c.path); err != nil {
			return DoctorCheck{Name: "config", Status: doctorWarn, Message: "config file " + c.path + " does not exist"}
		}
		if _, err := config.NewLoader().WithFile(c.path).Load(ctx); err != nil {
			return DoctorCheck{Name: "config", Status: doctorFail, Message: "config invalid: " + err.Error()}
		}
		return DoctorCheck{Name: "config", Status: doctorPass, Message: "config file " + c.path + " is valid"}
	}
	if _, err := config.Load(); err != nil {
		return DoctorCheck{Name: "config", Status: doctorFail, Message: "config load failed: " + err.Error()}
	}
	return DoctorCheck{Name: "config", Status: doctorPass, Message: "configuration valid"}
}

// ---------------------------------------------------------------------------
// ToolsChecker
// ---------------------------------------------------------------------------

// defaultRequiredTools are the tools the doctor verifies by default.
var defaultRequiredTools = []string{"go", "git", "make"}

// ToolsChecker verifies that required tools are installed and on PATH.
type ToolsChecker struct {
	tools []string // tools to check; empty means defaultRequiredTools
}

// NewToolsChecker returns a checker for tools. When tools is empty the default
// set (go, git, make) is used.
func NewToolsChecker(tools []string) *ToolsChecker {
	return &ToolsChecker{tools: tools}
}

// Check implements DoctorChecker.
func (c *ToolsChecker) Check(_ context.Context) DoctorCheck {
	tools := c.tools
	if len(tools) == 0 {
		tools = defaultRequiredTools
	}
	var missing []string
	for _, t := range tools {
		if _, err := exec.LookPath(t); err != nil {
			missing = append(missing, t)
		}
	}
	if len(missing) == 0 {
		return DoctorCheck{Name: "tools", Status: doctorPass,
			Message: fmt.Sprintf("all %d required tools installed (%s)", len(tools), strings.Join(tools, ", "))}
	}
	return DoctorCheck{Name: "tools", Status: doctorFail,
		Message: "missing tools: " + strings.Join(missing, ", ")}
}

// ---------------------------------------------------------------------------
// PermissionsChecker
// ---------------------------------------------------------------------------

// PermissionsChecker verifies that sensitive files are not writable by group
// or others.
type PermissionsChecker struct {
	paths []string // paths to check; empty means the default auth.json path
}

// NewPermissionsChecker returns a checker for paths. When paths is empty the
// default auth.json path (~/.config/go-cli/auth.json) is used.
func NewPermissionsChecker(paths []string) *PermissionsChecker {
	return &PermissionsChecker{paths: paths}
}

// Check implements DoctorChecker.
func (c *PermissionsChecker) Check(_ context.Context) DoctorCheck {
	paths := c.paths
	if len(paths) == 0 {
		paths = []string{defaultAuthPath()}
	}
	var problems []string
	checked := 0
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue // skip non-existent files
		}
		checked++
		mode := info.Mode().Perm()
		if mode&0o022 != 0 {
			problems = append(problems, fmt.Sprintf("%s is writable by group/others (mode %o)", p, mode))
		}
	}
	if len(problems) == 0 {
		return DoctorCheck{Name: "permissions", Status: doctorPass,
			Message: fmt.Sprintf("%d file(s) checked, permissions OK", checked)}
	}
	return DoctorCheck{Name: "permissions", Status: doctorWarn,
		Message: strings.Join(problems, "; ")}
}

// defaultAuthPath returns the default auth.json location.
func defaultAuthPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".config", "go-cli", "auth.json")
	}
	return filepath.Join(home, ".config", "go-cli", "auth.json")
}

// ---------------------------------------------------------------------------
// NetworkChecker
// ---------------------------------------------------------------------------

// defaultNetworkTimeout is the dial timeout used when the caller does not
// provide one.
const defaultNetworkTimeout = 2 * time.Second

// NetworkChecker verifies network connectivity by dialing a known endpoint.
// When target is empty the checker loads the application config and dials the
// configured provider API host — this replaces the former hardcoded 8.8.8.8
// probe so the doctor validates the endpoint that actually matters.
type NetworkChecker struct {
	target       string        // host:port to dial; empty means derive from provider config
	timeout      time.Duration // dial timeout; zero means defaultNetworkTimeout
	configLoader func() (*config.Config, error)
}

// NewNetworkChecker returns a checker for target. When target is empty the
// provider API host (from config) is used.
func NewNetworkChecker(target string, timeout time.Duration) *NetworkChecker {
	return &NetworkChecker{target: target, timeout: timeout, configLoader: config.Load}
}

// Check implements DoctorChecker.
func (c *NetworkChecker) Check(ctx context.Context) DoctorCheck {
	target := c.target
	if target == "" {
		cfg, _ := c.configLoader()
		baseURL := resolveProviderBaseURL(cfg)
		resolved, err := hostPortFromURL(baseURL)
		if err != nil {
			return DoctorCheck{Name: "network", Status: doctorWarn,
				Message: "cannot parse provider API URL (" + baseURL + "): " + err.Error() +
					". Advice: check the base_url in your configuration."}
		}
		target = resolved
	}
	timeout := c.timeout
	if timeout == 0 {
		timeout = defaultNetworkTimeout
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		return DoctorCheck{Name: "network", Status: doctorWarn,
			Message: "cannot reach " + target + ": " + err.Error()}
	}
	_ = conn.Close() //nolint:errcheck
	return DoctorCheck{Name: "network", Status: doctorPass,
		Message: "network reachable (" + target + ")"}
}

// ---------------------------------------------------------------------------
// Provider URL helpers
// ---------------------------------------------------------------------------

// defaultProviderBaseURL returns the default API endpoint for a provider name.
// This mirrors the defaults in internal/llm/native.go so the doctor can probe
// the correct host even when BaseURL is not explicitly configured.
func defaultProviderBaseURL(name string) string {
	switch strings.ToLower(name) {
	case "openai":
		return "https://api.openai.com/v1"
	case "claude", "anthropic":
		return "https://api.anthropic.com/v1"
	case "gemini":
		return "https://generativelanguage.googleapis.com"
	default:
		return "https://api.openai.com/v1"
	}
}

// resolveProviderBaseURL returns the effective provider BaseURL from cfg,
// falling back to the provider-name default when unset.
func resolveProviderBaseURL(cfg *config.Config) string {
	if cfg != nil && cfg.Provider.BaseURL != "" {
		return cfg.Provider.BaseURL
	}
	name := ""
	if cfg != nil {
		name = cfg.Provider.Name
	}
	return defaultProviderBaseURL(name)
}

// hostPortFromURL extracts the host:port from a URL string, defaulting the
// port based on the scheme when omitted.
func hostPortFromURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("no host in URL %q", rawURL)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port), nil
}

// ---------------------------------------------------------------------------
// APIConnectivityChecker
// ---------------------------------------------------------------------------

// apiConnectivityTimeout bounds the HTTP probe to the provider API.
const apiConnectivityTimeout = 5 * time.Second

// APIConnectivityChecker sends a minimal HTTP request to the provider BaseURL
// to verify the API endpoint is reachable. Any HTTP response (even 401/404)
// counts as success; a connection error counts as failure.
type APIConnectivityChecker struct {
	configLoader func() (*config.Config, error)
	httpClient   *http.Client
}

// NewAPIConnectivityChecker returns a checker that probes the provider API.
func NewAPIConnectivityChecker() *APIConnectivityChecker {
	return &APIConnectivityChecker{
		configLoader: config.Load,
		httpClient:   &http.Client{Timeout: apiConnectivityTimeout},
	}
}

// Check implements DoctorChecker.
func (c *APIConnectivityChecker) Check(ctx context.Context) DoctorCheck {
	cfg, _ := c.configLoader()
	baseURL := resolveProviderBaseURL(cfg)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, baseURL, nil)
	if err != nil {
		return DoctorCheck{Name: "api-connectivity", Status: doctorFail,
			Message: "cannot build request to " + baseURL + ": " + err.Error() +
				". Advice: check the base_url in your configuration."}
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return DoctorCheck{Name: "api-connectivity", Status: doctorFail,
			Message: "cannot reach provider API at " + baseURL + ": " + err.Error() +
				". Advice: verify network connectivity and that base_url is correct."}
	}
	resp.Body.Close() //nolint:errcheck,gosec
	return DoctorCheck{Name: "api-connectivity", Status: doctorPass,
		Message: fmt.Sprintf("provider API reachable at %s (HTTP %d)", baseURL, resp.StatusCode)}
}

// ---------------------------------------------------------------------------
// MCPServersChecker
// ---------------------------------------------------------------------------

// mcpConnectTimeout bounds each MCP server connect + ListTools attempt.
const mcpConnectTimeout = 5 * time.Second

// MCPServersChecker connects to each configured MCP server, calls ListTools,
// and reports a summary of which servers are available and which failed.
type MCPServersChecker struct {
	configLoader func() (*config.Config, error)
}

// NewMCPServersChecker returns a checker that probes configured MCP servers.
func NewMCPServersChecker() *MCPServersChecker {
	return &MCPServersChecker{configLoader: config.Load}
}

// Check implements DoctorChecker.
func (c *MCPServersChecker) Check(ctx context.Context) DoctorCheck {
	cfg, _ := c.configLoader()
	servers := loadMCPServers(cfg)
	if len(servers) == 0 {
		return DoctorCheck{Name: "mcp-servers", Status: doctorPass,
			Message: "no MCP servers configured"}
	}

	var ok, failed []string
	for _, srv := range servers {
		mcpCfg := toMCPConfig(srv)
		client := newMCPClientForDoctor(mcpCfg)

		connectCtx, cancel := context.WithTimeout(ctx, mcpConnectTimeout)
		connectErr := client.Connect(connectCtx)
		if connectErr != nil {
			cancel()
			failed = append(failed, fmt.Sprintf("%s: connect failed: %s", srv.Name, connectErr))
			continue
		}

		tools, listErr := client.ListTools(connectCtx)
		cancel()
		_ = client.Disconnect(ctx) //nolint:errcheck
		if listErr != nil {
			failed = append(failed, fmt.Sprintf("%s: list tools failed: %s", srv.Name, listErr))
			continue
		}
		ok = append(ok, fmt.Sprintf("%s (%d tools)", srv.Name, len(tools)))
	}

	if len(failed) == 0 {
		return DoctorCheck{Name: "mcp-servers", Status: doctorPass,
			Message: fmt.Sprintf("%d/%d server(s) ok (%s)", len(ok), len(servers), strings.Join(ok, ", "))}
	}
	return DoctorCheck{Name: "mcp-servers", Status: doctorWarn,
		Message: fmt.Sprintf("%d ok, %d failed. Failed: %s. Advice: verify the server command/URL and that the server process starts correctly.",
			len(ok), len(failed), strings.Join(failed, "; "))}
}

// toMCPConfig converts a config.MCPServerConfig into the mcp.MCPServerConfig
// expected by the MCP client adapters. The transport is inferred from whether
// Command (stdio) or URL (SSE/HTTP) is set.
func toMCPConfig(srv config.MCPServerConfig) mcp.MCPServerConfig {
	cfg := mcp.MCPServerConfig{
		Name: srv.Name,
		URL:  srv.URL,
	}
	if srv.Command != "" {
		cfg.Transport = mcp.MCPTransportStdio
		cfg.Command = srv.Command
		cfg.Args = srv.Args
		for k, v := range srv.Env {
			cfg.Env = append(cfg.Env, k+"="+v)
		}
	} else if srv.URL != "" {
		cfg.Transport = mcp.MCPTransportSSE
	}
	return cfg
}

// newMCPClientForDoctor creates an MCPClient appropriate for the transport
// mode in cfg.
func newMCPClientForDoctor(cfg mcp.MCPServerConfig) mcp.MCPClient {
	if cfg.Transport == mcp.MCPTransportSSE {
		return mcp.NewHTTPClientAdapter(cfg)
	}
	return mcp.NewOfficialSDKAdapter(cfg)
}

// ---------------------------------------------------------------------------
// LSPChecker
// ---------------------------------------------------------------------------

// LSPChecker verifies that each configured LSP server command is installed and
// available on PATH.
type LSPChecker struct {
	configLoader func() (*config.Config, error)
	lookPath     func(string) (string, error)
}

// NewLSPChecker returns a checker that probes configured LSP servers.
func NewLSPChecker() *LSPChecker {
	return &LSPChecker{configLoader: config.Load, lookPath: exec.LookPath}
}

// Check implements DoctorChecker.
func (c *LSPChecker) Check(_ context.Context) DoctorCheck {
	cfg, _ := c.configLoader()
	commands := collectLSPCommands(cfg)
	if len(commands) == 0 {
		return DoctorCheck{Name: "lsp", Status: doctorPass,
			Message: "no LSP servers configured"}
	}

	seen := map[string]bool{}
	var missing []string
	checked := 0
	for _, cmd := range commands {
		if seen[cmd] {
			continue
		}
		seen[cmd] = true
		checked++
		if _, err := c.lookPath(cmd); err != nil {
			missing = append(missing, cmd)
		}
	}

	if len(missing) == 0 {
		return DoctorCheck{Name: "lsp", Status: doctorPass,
			Message: fmt.Sprintf("all %d LSP server command(s) found on PATH", checked)}
	}
	return DoctorCheck{Name: "lsp", Status: doctorFail,
		Message: "missing LSP commands: " + strings.Join(missing, ", ") +
			". Advice: install the missing server(s), e.g. go install golang.org/x/tools/gopls@latest"}
}

// collectLSPCommands gathers the executable names from both the legacy
// single-server field and the multi-server list, preserving order.
func collectLSPCommands(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	var commands []string
	if len(cfg.LSP.ServerCommand) > 0 {
		commands = append(commands, cfg.LSP.ServerCommand[0])
	}
	for _, s := range cfg.LSP.Servers {
		if len(s.ServerCommand) > 0 {
			commands = append(commands, s.ServerCommand[0])
		}
	}
	return commands
}

// ---------------------------------------------------------------------------
// DiskSpaceChecker
// ---------------------------------------------------------------------------

// defaultMinDiskBytes is the minimum free disk space required (100 MB).
const defaultMinDiskBytes = 100 * 1024 * 1024

// DiskSpaceChecker verifies sufficient free disk space on the volume holding
// dir.
type DiskSpaceChecker struct {
	dir      string // path on the volume to check; empty means CWD
	minBytes uint64 // minimum free bytes; zero means defaultMinDiskBytes
}

// NewDiskSpaceChecker returns a checker for dir with a minimum of minBytes.
func NewDiskSpaceChecker(dir string, minBytes uint64) *DiskSpaceChecker {
	return &DiskSpaceChecker{dir: dir, minBytes: minBytes}
}

// Check implements DoctorChecker.
func (c *DiskSpaceChecker) Check(_ context.Context) DoctorCheck {
	dir := c.dir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return DoctorCheck{Name: "disk-space", Status: doctorFail, Message: "cannot determine working directory: " + err.Error()}
		}
	}
	min := c.minBytes
	if min == 0 {
		min = defaultMinDiskBytes
	}
	avail, err := diskFreeBytes(dir)
	if err != nil {
		return DoctorCheck{Name: "disk-space", Status: doctorWarn, Message: "cannot stat filesystem: " + err.Error()}
	}
	if avail >= min {
		return DoctorCheck{Name: "disk-space", Status: doctorPass,
			Message: fmt.Sprintf("%s available (min %s)", humanBytes(avail), humanBytes(min))}
	}
	return DoctorCheck{Name: "disk-space", Status: doctorFail,
		Message: fmt.Sprintf("only %s available (min %s)", humanBytes(avail), humanBytes(min))}
}

// humanBytes formats n as a human-readable size.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ---------------------------------------------------------------------------
// OSInfoChecker
// ---------------------------------------------------------------------------

// OSInfoChecker reports operating system information.
type OSInfoChecker struct{}

// NewOSInfoChecker returns a checker that reports OS information.
func NewOSInfoChecker() *OSInfoChecker {
	return &OSInfoChecker{}
}

// Check implements DoctorChecker.
func (c *OSInfoChecker) Check(_ context.Context) DoctorCheck {
	return DoctorCheck{Name: "os-info", Status: doctorPass, Message: getOSInfo()}
}

// getOSInfo returns a human-readable description of the operating system.
// It branches on runtime.GOOS so that the correct mechanism is used on each
// platform:
//   - darwin: calls sw_vers
//   - linux: reads /etc/os-release (falling back to /proc/version)
//   - other: returns a generic string from runtime values
func getOSInfo() string {
	switch runtime.GOOS {
	case "darwin":
		return getDarwinOSInfo()
	case "linux":
		return getLinuxOSInfo()
	default:
		return runtime.GOOS + " " + runtime.GOARCH
	}
}

// getDarwinOSInfo calls sw_vers and returns "ProductName ProductVersion".
func getDarwinOSInfo() string {
	out, err := exec.Command("sw_vers").CombinedOutput()
	if err != nil {
		return "macOS (version unknown)"
	}
	var name, version string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "ProductName:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "ProductName:"))
		}
		if strings.HasPrefix(line, "ProductVersion:") {
			version = strings.TrimSpace(strings.TrimPrefix(line, "ProductVersion:"))
		}
	}
	if name != "" && version != "" {
		return name + " " + version
	}
	return strings.TrimSpace(string(out))
}

// getLinuxOSInfo reads /etc/os-release (falling back to /proc/version) to
// produce a distribution name and version string.
func getLinuxOSInfo() string {
	data, err := os.ReadFile("/etc/os-release")
	if err == nil {
		var name, version string
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
			}
			if strings.HasPrefix(line, "NAME=") {
				name = strings.Trim(strings.TrimPrefix(line, "NAME="), `"`)
			}
			if strings.HasPrefix(line, "VERSION=") {
				version = strings.Trim(strings.TrimPrefix(line, "VERSION="), `"`)
			}
		}
		if name != "" {
			if version != "" {
				return name + " " + version
			}
			return name
		}
	}
	data, err = os.ReadFile("/proc/version")
	if err == nil {
		return strings.TrimSpace(string(data))
	}
	return "Linux (distribution unknown)"
}

// doctorCmd implements Command and runs diagnostic checks.
type doctorCmd struct {
	out io.Writer
}

// newDoctorCmd creates a doctor command writing to out.
func newDoctorCmd(out io.Writer) *doctorCmd {
	return &doctorCmd{out: out}
}

// Name implements Command.
func (c *doctorCmd) Name() string { return "doctor" }

// Synopsis implements Command.
func (c *doctorCmd) Synopsis() string { return "Run diagnostic checks" }

// Run implements Command. It runs all doctor checks and prints the results.
// A non-zero number of failed checks returns an ExecutionError.
func (c *doctorCmd) Run(ctx context.Context, cfg Config, args []string) error {
	runner := NewDoctorRunner()
	checks := runner.Run(ctx)
	fmt.Fprint(c.out, Format(checks)) //nolint:errcheck
	for _, ch := range checks {
		if ch.Status == doctorFail {
			return newExecutionError(fmt.Sprintf("doctor: %s check failed", ch.Name), nil)
		}
	}
	return nil
}

var _ Command = (*doctorCmd)(nil)

// Compile-time assertions that each checker satisfies DoctorChecker.
var (
	_ DoctorChecker = (*GoVersionChecker)(nil)
	_ DoctorChecker = (*GoModChecker)(nil)
	_ DoctorChecker = (*MakefileChecker)(nil)
	_ DoctorChecker = (*ConfigChecker)(nil)
	_ DoctorChecker = (*ToolsChecker)(nil)
	_ DoctorChecker = (*PermissionsChecker)(nil)
	_ DoctorChecker = (*NetworkChecker)(nil)
	_ DoctorChecker = (*APIConnectivityChecker)(nil)
	_ DoctorChecker = (*MCPServersChecker)(nil)
	_ DoctorChecker = (*LSPChecker)(nil)
	_ DoctorChecker = (*DiskSpaceChecker)(nil)
	_ DoctorChecker = (*OSInfoChecker)(nil)
)
