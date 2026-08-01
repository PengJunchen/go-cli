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
