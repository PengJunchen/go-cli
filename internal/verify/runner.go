// Package verify — Verification runner that executes all verification rules
// and produces a structured report.
package verify

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// VerifyRule represents a single verification rule.
type VerifyRule struct {
	// ID is the rule identifier (e.g. VQ-001).
	ID string `json:"id"`
	// Name is the human-readable rule name.
	Name string `json:"name"`
	// Category groups rules by domain (e.g. queue, turn, state, error).
	Category string `json:"category"`
	// Description explains what the rule verifies.
	Description string `json:"description"`
}

// VerifyResult represents the result of running a verification rule.
type VerifyResult struct {
	// ID is the rule identifier this result corresponds to.
	ID string `json:"id"`
	// Name is the human-readable rule name.
	Name string `json:"name"`
	// Status is the outcome: PASS, FAIL, or SKIP.
	Status string `json:"status"`
	// DurationMs is the execution duration in milliseconds.
	DurationMs int64 `json:"duration_ms"`
	// Message provides additional context about the result.
	Message string `json:"message,omitempty"`
	// Details lists additional detail strings.
	Details []string `json:"details,omitempty"`
}

// VerifyReport is the complete verification report.
type VerifyReport struct {
	// Timestamp is when the verification was run (RFC3339).
	Timestamp string `json:"timestamp"`
	// Dir is the directory that was verified.
	Dir string `json:"dir"`
	// Summary aggregates result counts.
	Summary VerifySummary `json:"summary"`
	// Results is the list of individual verification results.
	Results []VerifyResult `json:"results"`
	// Scan holds the AST scan report, if available.
	Scan *ScanReport `json:"scan,omitempty"`
}

// VerifySummary summarizes verification results.
type VerifySummary struct {
	// Total is the total number of verification results.
	Total int `json:"total"`
	// Passed is the count of PASS results.
	Passed int `json:"passed"`
	// Failed is the count of FAIL results.
	Failed int `json:"failed"`
	// Skipped is the count of SKIP results.
	Skipped int `json:"skipped"`
}

// AllRules returns the complete list of verification rules.
func AllRules() []VerifyRule {
	return []VerifyRule{
		// CLI execution.
		{ID: "VQ-001", Name: "命令解析与执行", Category: "cli", Description: "Parse flags and execute subcommands"},
		{ID: "VQ-002", Name: "未知命令返回错误", Category: "cli", Description: "Unknown command returns error"},
		{ID: "VQ-003", Name: "version 输出正确", Category: "cli", Description: "Version flag and command output"},
		// Config system.
		{ID: "VQ-004", Name: "配置加载从环境变量", Category: "config", Description: "Load config from env vars"},
		// State management.
		{ID: "VS-001", Name: "并发安全", Category: "state", Description: "Race test"},
		{ID: "VS-002", Name: "状态变更可观测", Category: "state", Description: "Log capture"},
		// Context management (VG prefix = Go context/goroutine rules).
		{ID: "VG-001", Name: "context 取消传播", Category: "context", Description: "Context cancellation propagation"},
		{ID: "VG-002", Name: "goroutine 泄漏检测", Category: "context", Description: "Goroutine leak detection (CLI level)"},
		// Error recovery.
		{ID: "VE-001", Name: "错误包装保留链", Category: "error", Description: "Error wrapping preserves chain"},
		{ID: "VE-002", Name: "非空错误检测", Category: "error", Description: "Non-nil error detection"},
	}
}

// RunVerify executes the full verification suite against a directory.
// It runs the AST scanner and checks for project structure issues.
// Individual rule verification (VQ-001 etc.) is done via Go test files
// that use the verify package utilities.
func RunVerify(dir string) *VerifyReport {
	report := &VerifyReport{
		Timestamp: time.Now().Format(time.RFC3339),
		Dir:       dir,
		Results:   []VerifyResult{},
	}

	// Phase 1: AST Scan.
	scanCfg := DefaultScanConfig(dir)
	scanReport, err := Scan(scanCfg)
	if err != nil {
		report.Results = append(report.Results, VerifyResult{
			ID:      "SCAN",
			Name:    "AST Scanner",
			Status:  "FAIL",
			Message: fmt.Sprintf("scanner error: %v", err),
		})
	} else {
		report.Scan = scanReport
		status := "PASS"
		message := fmt.Sprintf("Scanned %d files, %d findings (%d errors, %d warnings)",
			scanReport.Summary.TotalFiles, scanReport.Summary.TotalFindings,
			scanReport.Summary.Errors, scanReport.Summary.Warnings)
		if scanReport.HasErrors() {
			status = "FAIL"
		}
		report.Results = append(report.Results, VerifyResult{
			ID:      "SCAN",
			Name:    "AST Scanner (mock/hardcoded bypass detection)",
			Status:  status,
			Message: message,
		})
	}

	// Phase 2: Check for Go-specific issues via file scanning.
	report.Results = append(report.Results, checkGoFiles(dir)...)

	// Phase 3: Include all V* rules as SKIP (pending package-level test implementation).
	for _, rule := range AllRules() {
		report.Results = append(report.Results, VerifyResult{
			ID:      rule.ID,
			Name:    rule.Name,
			Status:  "SKIP",
			Message: fmt.Sprintf("category=%s — pending in package tests", rule.Category),
		})
	}

	// Compute summary.
	for _, r := range report.Results {
		report.Summary.Total++
		switch r.Status {
		case "PASS":
			report.Summary.Passed++
		case "FAIL":
			report.Summary.Failed++
		case "SKIP":
			report.Summary.Skipped++
		}
	}

	return report
}

// checkGoFiles performs Go-specific file checks.
func checkGoFiles(dir string) []VerifyResult {
	var results []VerifyResult

	// Check for go.mod.
	goModPath := filepath.Join(dir, "go.mod")
	if _, err := os.Stat(goModPath); err == nil {
		results = append(results, VerifyResult{
			ID:      "GOMOD",
			Name:    "go.mod exists",
			Status:  "PASS",
			Message: goModPath,
		})
	} else {
		results = append(results, VerifyResult{
			ID:      "GOMOD",
			Name:    "go.mod exists",
			Status:  "FAIL",
			Message: "go.mod not found",
		})
	}

	// Check for .golangci.yml.
	lintPath := filepath.Join(dir, ".golangci.yml")
	if _, err := os.Stat(lintPath); err == nil {
		results = append(results, VerifyResult{
			ID:      "LINT",
			Name:    "golangci-lint config exists",
			Status:  "PASS",
			Message: lintPath,
		})
	} else {
		results = append(results, VerifyResult{
			ID:      "LINT",
			Name:    "golangci-lint config exists",
			Status:  "FAIL",
			Message: ".golangci.yml not found",
		})
	}

	// Count Go files.
	goFileCount := 0
	testFileCount := 0
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			testFileCount++
		} else {
			goFileCount++
		}
		return nil
	})
	if walkErr != nil {
		results = append(results, VerifyResult{
			ID:      "GOFILES",
			Name:    "Go source files scan",
			Status:  "FAIL",
			Message: fmt.Sprintf("filepath.Walk error: %v", walkErr),
		})
		return results
	}

	results = append(results, VerifyResult{
		ID:      "GOFILES",
		Name:    "Go source files present",
		Status:  "PASS",
		Message: fmt.Sprintf("%d source files, %d test files", goFileCount, testFileCount),
	})

	if testFileCount == 0 && goFileCount > 0 {
		results = append(results, VerifyResult{
			ID:      "TESTCOV",
			Name:    "Test files exist for source",
			Status:  "FAIL",
			Message: "no test files found",
		})
	} else {
		results = append(results, VerifyResult{
			ID:      "TESTCOV",
			Name:    "Test files exist for source",
			Status:  "PASS",
			Message: fmt.Sprintf("%d test files", testFileCount),
		})
	}

	return results
}

// SaveReport writes a verification report to a JSON file.
func SaveReport(report *VerifyReport, path string) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// FormatReportText returns a human-readable text version of the report.
func FormatReportText(report *VerifyReport) string {
	var sb strings.Builder
	sb.WriteString("═══════════════════════════════════════════════\n")
	sb.WriteString("           VERIFICATION REPORT\n")
	sb.WriteString("═══════════════════════════════════════════════\n")
	sb.WriteString(fmt.Sprintf("Timestamp: %s\n", report.Timestamp))
	sb.WriteString(fmt.Sprintf("Directory: %s\n", report.Dir))
	sb.WriteString(fmt.Sprintf("Summary: %d total, %d passed, %d failed, %d skipped\n\n",
		report.Summary.Total, report.Summary.Passed, report.Summary.Failed, report.Summary.Skipped))

	sb.WriteString("───────────────────────────────────────────────\n")
	sb.WriteString("RESULTS\n")
	sb.WriteString("───────────────────────────────────────────────\n")
	for _, r := range report.Results {
		status := "PASS"
		if r.Status == "FAIL" {
			status = "FAIL"
		} else if r.Status == "SKIP" {
			status = "SKIP"
		}
		sb.WriteString(fmt.Sprintf("[%s] %s — %s (%dms)\n", status, r.ID, r.Name, r.DurationMs))
		if r.Message != "" {
			sb.WriteString(fmt.Sprintf("       %s\n", r.Message))
		}
		for _, d := range r.Details {
			sb.WriteString(fmt.Sprintf("       - %s\n", d))
		}
	}

	if report.Scan != nil && len(report.Scan.Findings) > 0 {
		sb.WriteString("\n───────────────────────────────────────────────\n")
		sb.WriteString("SCAN FINDINGS\n")
		sb.WriteString("───────────────────────────────────────────────\n")
		for _, f := range report.Scan.Findings {
			sb.WriteString(fmt.Sprintf("[%s] %s %s:%d — %s\n",
				f.Severity, f.RuleID, f.File, f.Line, f.Message))
		}
	}

	return sb.String()
}
