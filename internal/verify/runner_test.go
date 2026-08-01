package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllRules(t *testing.T) {
	rules := AllRules()
	require.NotEmpty(t, rules, "AllRules should return non-empty slice")

	expectedIDs := map[string]bool{
		"VQ-001": false, "VQ-002": false, "VQ-003": false, "VQ-004": false,
		"VS-001": false, "VS-002": false,
		"VG-001": false, "VG-002": false,
		"VE-001": false, "VE-002": false,
	}
	for _, rule := range rules {
		assert.NotEmpty(t, rule.ID, "rule should have non-empty ID")
		assert.NotEmpty(t, rule.Name, "rule %s should have non-empty Name", rule.ID)
		assert.NotEmpty(t, rule.Category, "rule %s should have non-empty Category", rule.ID)
		assert.NotEmpty(t, rule.Description, "rule %s should have non-empty Description", rule.ID)
		if _, ok := expectedIDs[rule.ID]; ok {
			expectedIDs[rule.ID] = true
		}
	}
	for id, found := range expectedIDs {
		assert.True(t, found, "expected rule %s to be present in AllRules", id)
	}
}

func TestCheckGoFiles_CompleteProject(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".golangci.yml"), []byte("linters:\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package example\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main_test.go"), []byte("package example\n"), 0o600))

	results := checkGoFiles(dir)
	require.Len(t, results, 4, "expected 4 results from checkGoFiles")

	resultMap := make(map[string]VerifyResult)
	for _, r := range results {
		resultMap[r.ID] = r
	}

	assert.Equal(t, "PASS", resultMap["GOMOD"].Status, "go.mod should be found")
	assert.Equal(t, "PASS", resultMap["LINT"].Status, ".golangci.yml should be found")
	assert.Equal(t, "PASS", resultMap["GOFILES"].Status, "Go files should be found")
	assert.Contains(t, resultMap["GOFILES"].Message, "1 source files, 1 test files")
	assert.Equal(t, "PASS", resultMap["TESTCOV"].Status, "test files should be found")
	assert.Contains(t, resultMap["TESTCOV"].Message, "1 test files")
}

func TestCheckGoFiles_MissingGoModAndLintConfig(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package example\n"), 0o600))

	results := checkGoFiles(dir)
	resultMap := make(map[string]VerifyResult)
	for _, r := range results {
		resultMap[r.ID] = r
	}

	assert.Equal(t, "FAIL", resultMap["GOMOD"].Status, "go.mod should be missing")
	assert.Equal(t, "go.mod not found", resultMap["GOMOD"].Message)
	assert.Equal(t, "FAIL", resultMap["LINT"].Status, ".golangci.yml should be missing")
	assert.Equal(t, ".golangci.yml not found", resultMap["LINT"].Message)
}

func TestCheckGoFiles_NoTestFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package example\n"), 0o600))

	results := checkGoFiles(dir)
	resultMap := make(map[string]VerifyResult)
	for _, r := range results {
		resultMap[r.ID] = r
	}

	assert.Equal(t, "FAIL", resultMap["TESTCOV"].Status, "no test files should fail TESTCOV")
	assert.Equal(t, "no test files found", resultMap["TESTCOV"].Message)
}

func TestCheckGoFiles_EmptyDir(t *testing.T) {
	dir := t.TempDir()

	results := checkGoFiles(dir)
	resultMap := make(map[string]VerifyResult)
	for _, r := range results {
		resultMap[r.ID] = r
	}

	assert.Equal(t, "FAIL", resultMap["GOMOD"].Status)
	assert.Equal(t, "FAIL", resultMap["LINT"].Status)
	assert.Equal(t, "PASS", resultMap["GOFILES"].Status)
	assert.Contains(t, resultMap["GOFILES"].Message, "0 source files, 0 test files")
	// goFileCount == 0 so the "no test files" condition (testFileCount == 0 && goFileCount > 0) is false.
	assert.Equal(t, "PASS", resultMap["TESTCOV"].Status)
}

func TestCheckGoFiles_Subdirectories(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "pkg")
	require.NoError(t, os.MkdirAll(subDir, 0o750))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package example\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "handler.go"), []byte("package pkg\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(subDir, "handler_test.go"), []byte("package pkg\n"), 0o600))

	results := checkGoFiles(dir)
	resultMap := make(map[string]VerifyResult)
	for _, r := range results {
		resultMap[r.ID] = r
	}

	assert.Equal(t, "PASS", resultMap["GOFILES"].Status)
	assert.Contains(t, resultMap["GOFILES"].Message, "2 source files, 1 test files")
}

func TestRunVerify(t *testing.T) {
	defer AssertNoGoroutineLeak(t)()

	dir := t.TempDir()

	cleanCode := `package example

import "fmt"

func Hello() { fmt.Println("hello") }
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(cleanCode), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0o600))

	report := RunVerify(dir)
	require.NotNil(t, report)

	assert.NotEmpty(t, report.Timestamp, "report should have a timestamp")
	assert.Equal(t, dir, report.Dir, "report.Dir should match input dir")
	assert.NotEmpty(t, report.Results, "report should have results")

	// Verify timestamp is parseable as RFC3339.
	_, err := time.Parse(time.RFC3339, report.Timestamp)
	assert.NoError(t, err, "timestamp should be valid RFC3339")

	// Verify summary adds up.
	assert.Equal(t, report.Summary.Total,
		report.Summary.Passed+report.Summary.Failed+report.Summary.Skipped,
		"summary total should equal passed+failed+skipped")

	// Verify SCAN result is present.
	scanResult := findResultByID(report, "SCAN")
	require.NotNil(t, scanResult, "SCAN result should be present")

	// Verify GOMOD result is present.
	goModResult := findResultByID(report, "GOMOD")
	require.NotNil(t, goModResult, "GOMOD result should be present")
	assert.Equal(t, "PASS", goModResult.Status, "go.mod should be found")

	// Verify all V* rules are SKIP.
	for _, rule := range AllRules() {
		r := findResultByID(report, rule.ID)
		require.NotNil(t, r, "rule %s should be in results", rule.ID)
		assert.Equal(t, "SKIP", r.Status, "rule %s should be SKIP", rule.ID)
	}
}

func TestRunVerify_ScanError(t *testing.T) {
	defer AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	nonExistentDir := filepath.Join(dir, "nonexistent")

	report := RunVerify(nonExistentDir)
	require.NotNil(t, report)

	scanResult := findResultByID(report, "SCAN")
	require.NotNil(t, scanResult)
	assert.Equal(t, "FAIL", scanResult.Status)
	assert.Contains(t, scanResult.Message, "scanner error")
}

func TestRunVerify_WithScanFindings(t *testing.T) {
	defer AssertNoGoroutineLeak(t)()

	dir := t.TempDir()

	mockCode := `package example

import "github.com/stretchr/testify/mock"

type S struct{ m mock.Mock }
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "service.go"), []byte(mockCode), 0o600))

	report := RunVerify(dir)
	require.NotNil(t, report)

	scanResult := findResultByID(report, "SCAN")
	require.NotNil(t, scanResult)
	assert.Equal(t, "FAIL", scanResult.Status)

	require.NotNil(t, report.Scan, "scan report should be present")
	assert.NotEmpty(t, report.Scan.Findings, "scan should have findings")
}

func TestSaveReport(t *testing.T) {
	report := &VerifyReport{
		Timestamp: time.Now().Format(time.RFC3339),
		Dir:       "/test/dir",
		Summary: VerifySummary{
			Total:   2,
			Passed:  1,
			Failed:  1,
			Skipped: 0,
		},
		Results: []VerifyResult{
			{ID: "TEST-001", Name: "Test rule", Status: "PASS", DurationMs: 100},
			{ID: "TEST-002", Name: "Failing rule", Status: "FAIL", DurationMs: 200, Message: "something broke"},
		},
	}

	path := filepath.Join(t.TempDir(), "report.json")
	require.NoError(t, SaveReport(report, path))

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var loaded VerifyReport
	require.NoError(t, json.Unmarshal(data, &loaded))

	assert.Equal(t, report.Dir, loaded.Dir)
	assert.Equal(t, report.Timestamp, loaded.Timestamp)
	assert.Equal(t, report.Summary.Total, loaded.Summary.Total)
	assert.Equal(t, report.Summary.Passed, loaded.Summary.Passed)
	assert.Equal(t, report.Summary.Failed, loaded.Summary.Failed)
	require.Len(t, loaded.Results, 2)
	assert.Equal(t, "TEST-001", loaded.Results[0].ID)
	assert.Equal(t, "PASS", loaded.Results[0].Status)
	assert.Equal(t, "TEST-002", loaded.Results[1].ID)
	assert.Equal(t, "FAIL", loaded.Results[1].Status)
	assert.Equal(t, "something broke", loaded.Results[1].Message)
}

func TestSaveReport_InvalidPath(t *testing.T) {
	dir := t.TempDir()
	// Create a regular file where a directory would be needed.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blocked"), []byte("x"), 0o600))

	report := &VerifyReport{
		Timestamp: time.Now().Format(time.RFC3339),
		Dir:       "/test",
		Summary:   VerifySummary{Total: 0},
		Results:   []VerifyResult{},
	}

	err := SaveReport(report, filepath.Join(dir, "blocked", "sub", "report.json"))
	assert.Error(t, err, "SaveReport should fail when path is blocked by a file")
}

func TestFormatReportText(t *testing.T) {
	report := &VerifyReport{
		Timestamp: "2025-01-01T00:00:00Z",
		Dir:       "/project",
		Summary: VerifySummary{
			Total:   3,
			Passed:  1,
			Failed:  1,
			Skipped: 1,
		},
		Results: []VerifyResult{
			{ID: "R001", Name: "Rule One", Status: "PASS", DurationMs: 10, Message: "all good"},
			{ID: "R002", Name: "Rule Two", Status: "FAIL", DurationMs: 20, Message: "broken", Details: []string{"detail1", "detail2"}},
			{ID: "R003", Name: "Rule Three", Status: "SKIP", DurationMs: 0, Message: "pending"},
		},
	}

	text := FormatReportText(report)
	assert.Contains(t, text, "VERIFICATION REPORT")
	assert.Contains(t, text, "2025-01-01T00:00:00Z")
	assert.Contains(t, text, "/project")
	assert.Contains(t, text, "3 total, 1 passed, 1 failed, 1 skipped")
	assert.Contains(t, text, "[PASS] R001")
	assert.Contains(t, text, "[FAIL] R002")
	assert.Contains(t, text, "[SKIP] R003")
	assert.Contains(t, text, "all good")
	assert.Contains(t, text, "broken")
	assert.Contains(t, text, "detail1")
	assert.Contains(t, text, "detail2")
}

func TestFormatReportText_WithScanFindings(t *testing.T) {
	report := &VerifyReport{
		Timestamp: "2025-01-01T00:00:00Z",
		Dir:       "/project",
		Summary:   VerifySummary{Total: 1, Passed: 1},
		Results: []VerifyResult{
			{ID: "SCAN", Name: "AST Scanner", Status: "PASS", DurationMs: 50},
		},
		Scan: &ScanReport{
			Dir: "/project",
			Findings: []Finding{
				{RuleID: "SCAN-001", File: "service.go", Line: 5, Severity: SeverityError, Message: "mock import"},
			},
		},
	}

	text := FormatReportText(report)
	assert.Contains(t, text, "SCAN FINDINGS")
	assert.Contains(t, text, "SCAN-001")
	assert.Contains(t, text, "service.go")
	assert.Contains(t, text, "mock import")
}

func TestFormatReportText_EmptyReport(t *testing.T) {
	report := &VerifyReport{
		Timestamp: "2025-01-01T00:00:00Z",
		Dir:       "/empty",
		Summary:   VerifySummary{},
		Results:   []VerifyResult{},
	}

	text := FormatReportText(report)
	assert.Contains(t, text, "VERIFICATION REPORT")
	assert.Contains(t, text, "0 total, 0 passed, 0 failed, 0 skipped")
}

// findResultByID finds a VerifyResult by ID in a report.
func findResultByID(report *VerifyReport, id string) *VerifyResult {
	for i := range report.Results {
		if report.Results[i].ID == id {
			return &report.Results[i]
		}
	}
	return nil
}
