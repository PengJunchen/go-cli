package cli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
	results := runner.Run(context.Background())
	assert.NotEmpty(t, results)
	for _, r := range results {
		assert.Contains(t, []string{doctorPass, doctorWarn, doctorFail}, r.Status,
			"check %q returned invalid status %q", r.Name, r.Status)
		assert.NotEmpty(t, r.Name)
	}
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
