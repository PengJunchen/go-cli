package verify

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanner_NoFindingsOnCleanCode(t *testing.T) {
	dir := t.TempDir()

	// Write a clean Go file.
	cleanCode := `package example

import (
	"context"
	"fmt"
)

func Run(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	return nil
}
`
	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(cleanCode), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if report.HasErrors() {
		t.Errorf("expected no errors on clean code, got %d findings:\n%s",
			len(report.Findings), report.FormatText())
	}
}

func TestScanner_DetectsMockImport(t *testing.T) {
	dir := t.TempDir()

	code := `package example

import (
	"github.com/stretchr/testify/mock"
)

type Service struct{}
`
	err := os.WriteFile(filepath.Join(dir, "service.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-001" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected SCAN-001 finding for mock import, got none")
	}
}

func TestScanner_DetectsTestingUsage(t *testing.T) {
	dir := t.TempDir()

	code := `package example

import "testing"

func IsTestMode() bool {
	return testing.Testing()
}
`
	err := os.WriteFile(filepath.Join(dir, "mode.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-002" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected SCAN-002 finding for testing usage, got none")
	}
}

func TestScanner_DetectsHardcodedSecret(t *testing.T) {
	dir := t.TempDir()

	code := `package example

const APIKey = "sk-1234567890abcdefghijklmnopqrstuv"

func GetKey() string {
	return APIKey
}
`
	err := os.WriteFile(filepath.Join(dir, "config.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-003" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected SCAN-003 finding for hardcoded secret, got none")
	}
}

func TestScanner_DetectsTestURL(t *testing.T) {
	dir := t.TempDir()

	code := `package example

const BaseURL = "http://localhost:8080/api"

func GetURL() string {
	return BaseURL
}
`
	err := os.WriteFile(filepath.Join(dir, "url.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-004" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected SCAN-004 finding for test URL, got none")
	}
}

func TestScanner_DetectsBuildTag(t *testing.T) {
	dir := t.TempDir()

	code := `//go:build test

package example

func Run() {}
`
	err := os.WriteFile(filepath.Join(dir, "tagged.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-005" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected SCAN-005 finding for build tag, got none")
	}
}

func TestScanner_DetectsExportOnlyUsedByTest(t *testing.T) {
	dir := t.TempDir()

	// Production code with an exported function only used by tests.
	prodCode := `package example

func UsedByProd() string { return "prod" }
func OnlyUsedByTest() string { return "test-only" }
`
	err := os.WriteFile(filepath.Join(dir, "service.go"), []byte(prodCode), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// Test file that references OnlyUsedByTest.
	testCode := `package example

import "testing"

func TestOnlyUsedByTest(t *testing.T) {
	OnlyUsedByTest()
}
`
	err = os.WriteFile(filepath.Join(dir, "service_test.go"), []byte(testCode), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-006" && f.Snippet == "OnlyUsedByTest" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected SCAN-006 finding for OnlyUsedByTest, got none")
	}
}

func TestScanner_ExportUsedByProdNotFlagged(t *testing.T) {
	dir := t.TempDir()

	// Production code with two exported functions.
	prodCode := `package example

func UsedEverywhere() string { return "all" }
`
	err := os.WriteFile(filepath.Join(dir, "service.go"), []byte(prodCode), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// Another production file that uses the exported function.
	otherCode := `package example

func CallIt() string { return UsedEverywhere() }
`
	err = os.WriteFile(filepath.Join(dir, "other.go"), []byte(otherCode), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range report.Findings {
		if f.RuleID == "SCAN-006" {
			t.Errorf("expected no SCAN-006 finding when export is used in prod, got: %s", f.Message)
		}
	}
}

func TestScanner_DetectsExcessiveNolint(t *testing.T) {
	dir := t.TempDir()

	code := `package example

//nolint:errcheck
func A() {}

//nolint:unused
func B() {}

//nolint:ineffassign
func C() {}

//nolint:staticcheck
func D() {}
`
	err := os.WriteFile(filepath.Join(dir, "nolint.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	cfg.NolintThreshold = 3
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-007" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected SCAN-007 finding for excessive nolint, got none")
	}
}

// TestScanner_NoHardcodedSecretOnShortText verifies that strings shorter than
// 20 characters or non-secret-looking text do NOT trigger SCAN-003.
func TestScanner_NoHardcodedSecretOnShortText(t *testing.T) {
	dir := t.TempDir()

	code := `package example

var key = "short"

func Get() string {
	return "production_mode"
}
`
	err := os.WriteFile(filepath.Join(dir, "config.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if hasRule(report, "SCAN-003") {
		t.Errorf("expected no SCAN-003 finding for short/non-secret text, got:\n%s", report.FormatText())
	}
}

// TestScanner_NoTestURLOnProductionText verifies that non-test/non-URL strings
// do NOT trigger SCAN-004.
func TestScanner_NoTestURLOnProductionText(t *testing.T) {
	dir := t.TempDir()

	code := `package example

const Endpoint = "https://example.com/api/v1"

func Get() string {
	return "production api integration"
}
`
	err := os.WriteFile(filepath.Join(dir, "url.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if hasRule(report, "SCAN-004") {
		t.Errorf("expected no SCAN-004 finding for production text, got:\n%s", report.FormatText())
	}
}

// TestScanner_NoBuildTagOnLegitimateTags verifies that legitimate build tags
// (without test/integration constraints) do NOT trigger SCAN-005.
func TestScanner_NoBuildTagOnLegitimateTags(t *testing.T) {
	dir := t.TempDir()

	code := `//go:build linux && amd64

package example

func Run() {}
`
	err := os.WriteFile(filepath.Join(dir, "platform.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if hasRule(report, "SCAN-005") {
		t.Errorf("expected no SCAN-005 finding for legitimate build tag, got:\n%s", report.FormatText())
	}
}

// TestScanner_NoNolintBelowThreshold verifies that nolint directives at or
// below the threshold do NOT trigger SCAN-007.
func TestScanner_NoNolintBelowThreshold(t *testing.T) {
	dir := t.TempDir()

	code := `package example

//nolint:errcheck
func A() {}

// nolint:unused
func B() {}
`
	err := os.WriteFile(filepath.Join(dir, "nolint.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	cfg.NolintThreshold = 3
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if hasRule(report, "SCAN-007") {
		t.Errorf("expected no SCAN-007 finding below threshold, got:\n%s", report.FormatText())
	}
}

// TestScanConfigMergePriority_Violation verifies SCAN-011 fires when a
// higher-priority env value is later overwritten by a lower-priority file value.
func TestScanConfigMergePriority_Violation(t *testing.T) {
	dir := t.TempDir()

	code := `package example

import "os"

type Config struct {
	Token string
}

func Load() Config {
	cfg := Config{}
	cfg.Token = os.Getenv("TOKEN")
	cfg.Token = loadFromFile("cfg.json")
	return cfg
}

func loadFromFile(path string) string { return path }
`
	err := os.WriteFile(filepath.Join(dir, "config.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if !hasRule(report, "SCAN-011") {
		t.Errorf("expected SCAN-011 finding for env overwritten by file, got:\n%s", report.FormatText())
	}
}

// TestScanConfigMergePriority_Compliant verifies SCAN-011 does NOT fire when
// config fields are assigned in ascending priority (default → file → env).
func TestScanConfigMergePriority_Compliant(t *testing.T) {
	dir := t.TempDir()

	code := `package example

import "os"

type Config struct {
	Port int
}

func Load() Config {
	cfg := Config{Port: 8080}
	cfg.Port = loadPortFromFile("cfg.json")
	cfg.Port = lookupPortFromEnv()
	return cfg
}

func loadPortFromFile(path string) int { return 8080 }
func lookupPortFromEnv() int           { return 9090 }
`
	err := os.WriteFile(filepath.Join(dir, "config.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if hasRule(report, "SCAN-011") {
		t.Errorf("expected no SCAN-011 finding for ascending priority, got:\n%s", report.FormatText())
	}
}

func TestScanner_ExcludesTestFiles(t *testing.T) {
	dir := t.TempDir()

	// Test file with mock import — should NOT trigger SCAN-001.
	testCode := `package example_test

import (
	"testing"
	"github.com/stretchr/testify/mock"
)

func TestFoo(t *testing.T) {
	_ = mock.Mock{}
}
`
	err := os.WriteFile(filepath.Join(dir, "main_test.go"), []byte(testCode), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// SCAN-001 and SCAN-002 should NOT fire on test files.
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-001" || f.RuleID == "SCAN-002" {
			t.Errorf("should not scan test files for %s: %s:%d", f.RuleID, f.File, f.Line)
		}
	}
}

func TestScanner_JSON(t *testing.T) {
	dir := t.TempDir()

	code := `package example
const Token = "ghp_1234567890abcdefghijklmnopqrstuv1234567890"
`
	err := os.WriteFile(filepath.Join(dir, "secret.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	jsonBytes, err := report.ToJSON()
	if err != nil {
		t.Fatal(err)
	}

	if len(jsonBytes) == 0 {
		t.Error("expected non-empty JSON output")
	}
}

func TestScanner_FormatText(t *testing.T) {
	dir := t.TempDir()

	code := `package example
import "github.com/stretchr/testify/mock"
type S struct{ m mock.Mock }
`
	err := os.WriteFile(filepath.Join(dir, "s.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	text := report.FormatText()
	if len(text) == 0 {
		t.Error("expected non-empty text output")
	}
}

func TestScanner_HasErrors(t *testing.T) {
	dir := t.TempDir()

	code := `package example
import "github.com/stretchr/testify/mock"
type S struct{ m mock.Mock }
`
	err := os.WriteFile(filepath.Join(dir, "s.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if !report.HasErrors() {
		t.Error("expected HasErrors to return true")
	}
}

func TestScanner_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Findings) != 0 {
		t.Errorf("expected 0 findings on empty dir, got %d", len(report.Findings))
	}
}

func TestScanner_ExcludesVerifyDir(t *testing.T) {
	dir := t.TempDir()

	// Create a file inside internal/verify path.
	verifyDir := filepath.Join(dir, "internal", "verify")
	err := os.MkdirAll(verifyDir, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	code := `package verify
import "github.com/stretchr/testify/mock"
type S struct{ m mock.Mock }
`
	err = os.WriteFile(filepath.Join(verifyDir, "helper.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// Should be excluded.
	if report.HasErrors() {
		t.Errorf("expected no errors from excluded verify dir, got:\n%s", report.FormatText())
	}
}

func TestScanSlogUsage_Violation(t *testing.T) {
	dir := t.TempDir()

	code := `package example

func Run() {}
`
	err := os.WriteFile(filepath.Join(dir, "run.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if !hasRule(report, "SCAN-008") {
		t.Error("expected SCAN-008 finding for no slog usage, got none")
	}
}

func TestScanSlogUsage_Compliant(t *testing.T) {
	dir := t.TempDir()

	code := `package example

import (
	"context"
	"log/slog"
)

func Run(ctx context.Context) {
	slog.InfoContext(ctx, "running", "op", "command")
}
`
	err := os.WriteFile(filepath.Join(dir, "run.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if hasRule(report, "SCAN-008") {
		t.Errorf("expected no SCAN-008 finding for slog usage, got: %s", report.FormatText())
	}
}

func TestScanHardcodedDefaults(t *testing.T) {
	dir := t.TempDir()

	violation := `package example

const Prompt = "You are a helpful assistant."
`
	err := os.WriteFile(filepath.Join(dir, "prompt.go"), []byte(violation), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if !hasRule(report, "SCAN-009") {
		t.Error("expected SCAN-009 finding for hardcoded default/prompt, got none")
	}
}

func TestScanHardcodedDefaults_Compliant(t *testing.T) {
	dir := t.TempDir()

	code := `package example

const Prompt = "assistant"
`
	err := os.WriteFile(filepath.Join(dir, "prompt.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if hasRule(report, "SCAN-009") {
		t.Errorf("expected no SCAN-009 finding, got: %s", report.FormatText())
	}
}

func TestScanCommandRouting_Switch(t *testing.T) {
	dir := t.TempDir()

	violation := `package example

func Route(args []string) string {
	switch args[0] {
	case "help":
		return "usage"
	case "version":
		return "v1"
	}
	return ""
}
`
	err := os.WriteFile(filepath.Join(dir, "cmd.go"), []byte(violation), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if !hasRule(report, "SCAN-010") {
		t.Error("expected SCAN-010 finding for switch command routing, got none")
	}
}

func TestScanCommandRouting_RegistryPasses(t *testing.T) {
	dir := t.TempDir()

	code := `package example

import "fmt"

type Command struct {
	Name string
}

var registry = map[string]*Command{}

func Route(args []string) string {
	cmd, ok := registry[args[0]]
	if !ok {
		return fmt.Sprint("unknown")
	}
	return cmd.Name
}
`
	err := os.WriteFile(filepath.Join(dir, "cmd.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if hasRule(report, "SCAN-010") {
		t.Errorf("expected no SCAN-010 finding for registry routing, got: %s", report.FormatText())
	}
}

func TestScanInterfaceDefaultImpl_Violation(t *testing.T) {
	dir := t.TempDir()

	code := `package example

type Service interface {
	Run() error
}
`
	err := os.WriteFile(filepath.Join(dir, "service.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if !hasRule(report, "SCAN-012") {
		t.Error("expected SCAN-012 finding for interface missing default impl, got none")
	}
}

func TestScanInterfaceDefaultImpl_Compliant(t *testing.T) {
	dir := t.TempDir()

	code := `package example

type Service interface {
	Run() error
}

type defaultService struct{}

var _ Service = (*defaultService)(nil)
`
	err := os.WriteFile(filepath.Join(dir, "service.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if hasRule(report, "SCAN-012") {
		t.Errorf("expected no SCAN-012 finding with var _ assertion, got: %s", report.FormatText())
	}
}

func TestScanConcreteInInterface(t *testing.T) {
	dir := t.TempDir()

	violation := `package example

type Service interface {
	Run() error
}

type implService struct{}

func Run(s *implService) error {
	return nil
}
`
	err := os.WriteFile(filepath.Join(dir, "svc.go"), []byte(violation), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if !hasRule(report, "SCAN-013") {
		t.Error("expected SCAN-013 finding for concrete type in interface position, got none")
	}
}

func TestScanConcreteInInterface_Compliant(t *testing.T) {
	dir := t.TempDir()

	code := `package example

import "context"

type Service interface {
	Run(ctx context.Context) error
}

type defaultService struct{}

var _ Service = (*defaultService)(nil)

func New() Service {
	return &defaultService{}
}
`
	err := os.WriteFile(filepath.Join(dir, "svc.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := DefaultScanConfig(dir)
	report, err := Scan(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if hasRule(report, "SCAN-013") {
		t.Errorf("expected no SCAN-013 finding for interface usage, got: %s", report.FormatText())
	}
}

// hasRule reports whether the report contains any finding with the given RuleID.
func hasRule(report *ScanReport, ruleID string) bool {
	for _, f := range report.Findings {
		if f.RuleID == ruleID {
			return true
		}
	}
	return false
}

func TestScanDir(t *testing.T) {
	dir := t.TempDir()

	code := `package example
import "github.com/stretchr/testify/mock"
type S struct{ m mock.Mock }
`
	err := os.WriteFile(filepath.Join(dir, "s.go"), []byte(code), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	report, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !report.HasErrors() {
		t.Error("expected ScanDir to find errors")
	}
}
