// Package cli provides the core CLI execution framework.
package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pengjunchen/go-cli/internal/config"
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

// NewDoctorRunner returns a DoctorRunner with all eight default checkers
// registered.
func NewDoctorRunner() *DoctorRunner {
	return &DoctorRunner{checks: []DoctorChecker{
		NewGoVersionChecker(),
		NewGoModChecker(""),
		NewMakefileChecker(""),
		NewConfigChecker(""),
		NewToolsChecker(nil),
		NewPermissionsChecker(nil),
		NewNetworkChecker("", 0),
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

// defaultNetworkTarget is the endpoint probed for connectivity.
const defaultNetworkTarget = "8.8.8.8:80"

// NetworkChecker verifies network connectivity by dialing a known endpoint.
type NetworkChecker struct {
	target  string        // host:port to dial; empty means defaultNetworkTarget
	timeout time.Duration // dial timeout; zero means 2s
}

// NewNetworkChecker returns a checker for target. When target is empty the
// default endpoint is used.
func NewNetworkChecker(target string, timeout time.Duration) *NetworkChecker {
	return &NetworkChecker{target: target, timeout: timeout}
}

// Check implements DoctorChecker.
func (c *NetworkChecker) Check(ctx context.Context) DoctorCheck {
	target := c.target
	if target == "" {
		target = defaultNetworkTarget
	}
	timeout := c.timeout
	if timeout == 0 {
		timeout = 2 * time.Second
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

// diskFreeBytes returns the available bytes on the filesystem holding path.
func diskFreeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
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
	_ DoctorChecker = (*DiskSpaceChecker)(nil)
	_ DoctorChecker = (*OSInfoChecker)(nil)
)
