// Package verify — AST scanner for mock/hardcoded bypass detection.
// This scanner parses Go source files and detects patterns that could
// bypass verification: mock imports in production code, hardcoded test
// variables, build tags, and testing package references.
package verify

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Severity represents the severity level of a scan finding.
type Severity string

const (
	// SeverityError indicates an error-level finding that should fail the scan.
	SeverityError Severity = "ERROR"
	// SeverityWarn indicates a warning-level finding.
	SeverityWarn Severity = "WARN"
	// SeverityInfo indicates an informational finding.
	SeverityInfo Severity = "INFO"
)

// Finding represents a single issue found by the scanner.
type Finding struct {
	// RuleID is the identifier of the rule that triggered the finding (e.g. SCAN-001).
	RuleID string `json:"rule_id"`
	// File is the path of the file where the finding was detected.
	File string `json:"file"`
	// Line is the 1-based line number where the finding was detected.
	Line int `json:"line"`
	// Severity is the severity level of the finding.
	Severity Severity `json:"severity"`
	// Message describes the finding in human-readable terms.
	Message string `json:"message"`
	// Snippet holds an optional code snippet for context.
	Snippet string `json:"snippet,omitempty"`
}

// ScanReport is the complete output of a scan run.
type ScanReport struct {
	// Dir is the directory that was scanned.
	Dir string `json:"dir"`
	// Findings is the list of all findings discovered during the scan.
	Findings []Finding `json:"findings"`
	// Summary aggregates counts of findings by severity.
	Summary ScanSummary `json:"summary"`
}

// ScanSummary summarizes scan results.
type ScanSummary struct {
	// TotalFiles is the number of Go files scanned (excluding test files).
	TotalFiles int `json:"total_files"`
	// TotalFindings is the total number of findings across all severities.
	TotalFindings int `json:"total_findings"`
	// Errors is the count of ERROR-level findings.
	Errors int `json:"errors"`
	// Warnings is the count of WARN-level findings.
	Warnings int `json:"warnings"`
	// Infos is the count of INFO-level findings.
	Infos int `json:"infos"`
}

// ScanConfig configures the scanner behavior.
type ScanConfig struct {
	// Dir is the directory to scan.
	Dir string
	// ExcludeDirs lists directories to skip during the scan.
	ExcludeDirs []string
	// ExcludeFiles lists file glob patterns to skip (e.g. "*_test.go").
	ExcludeFiles []string
	// CheckMockImport enables SCAN-001 (mock package imports in production code).
	CheckMockImport bool
	// CheckTesting enables SCAN-002 (testing package references in production code).
	CheckTesting bool
	// CheckHardcoded enables SCAN-003 (hardcoded API keys/tokens).
	CheckHardcoded bool
	// CheckTestURL enables SCAN-004 (hardcoded test URLs like localhost).
	CheckTestURL bool
	// CheckBuildTag enables SCAN-005 (build tags in production files).
	CheckBuildTag bool
	// CheckNolint enables SCAN-007 (nolint directive threshold).
	CheckNolint bool
	// NolintThreshold is the maximum allowed //nolint directives per file.
	NolintThreshold int
}

// DefaultScanConfig returns a config with all checks enabled and sensible defaults.
func DefaultScanConfig(dir string) ScanConfig {
	return ScanConfig{
		Dir:             dir,
		ExcludeDirs:     []string{"vendor", "internal/verify", ".git"},
		ExcludeFiles:    []string{"*_test.go"},
		CheckMockImport: true,
		CheckTesting:    true,
		CheckHardcoded:  true,
		CheckTestURL:    true,
		CheckBuildTag:   true,
		CheckNolint:     true,
		NolintThreshold: 3,
	}
}

// mockImportPatterns detects mock-related import paths.
var mockImportPatterns = []string{
	"mock", "testify", "gomock", "mockery", "moq",
}

// hardcodedSecretPatterns detects hardcoded API keys/tokens.
var hardcodedSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|passwd|pwd)\s*[:=]\s*["'][^"']{8,}["']`),
	regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`), // OpenAI API key pattern
	regexp.MustCompile(`ghp_[a-zA-Z0-9]{36}`), // GitHub token pattern
}

// testURLPatterns detects hardcoded test URLs/ports.
var testURLPatterns = []string{
	"localhost", "127.0.0.1", "0.0.0.0", ":test", ":9999", ":8080",
}

// ScanDir scans a directory using the default configuration and returns all findings.
func ScanDir(dir string) (*ScanReport, error) {
	return Scan(DefaultScanConfig(dir))
}

// Scan scans a directory and returns all findings according to the given config.
func Scan(cfg ScanConfig) (*ScanReport, error) {
	report := &ScanReport{
		Dir:      cfg.Dir,
		Findings: []Finding{},
	}

	fset := token.NewFileSet()

	err := filepath.Walk(cfg.Dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip excluded directories.
		if info.IsDir() {
			if isExcludedDir(path, cfg.ExcludeDirs) {
				return filepath.SkipDir
			}
			return nil
		}

		// Only scan .go files.
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		// Skip excluded file patterns.
		for _, pattern := range cfg.ExcludeFiles {
			matched, _ := filepath.Match(pattern, filepath.Base(path)) //nolint:errcheck // patterns come from validated config
			if matched {
				// Still scan test files for hardcoded secrets (SCAN-003).
				if cfg.CheckHardcoded {
					findings := scanHardcodedSecrets(path)
					report.Findings = append(report.Findings, findings...)
				}
				return nil
			}
		}

		report.Summary.TotalFiles++

		// Parse the file.
		node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			report.Findings = append(report.Findings, Finding{
				RuleID:   "SCAN-PARSE",
				Severity: SeverityError,
				File:     path,
				Line:     0,
				Message:  fmt.Sprintf("parse error: %v", err),
			})
			return nil
		}

		// Run all enabled checks.
		if cfg.CheckMockImport {
			report.Findings = append(report.Findings, scanMockImports(path, node, fset)...)
		}
		if cfg.CheckTesting {
			report.Findings = append(report.Findings, scanTestingUsage(path, node, fset)...)
		}
		if cfg.CheckHardcoded {
			report.Findings = append(report.Findings, scanHardcodedSecrets(path)...)
		}
		if cfg.CheckTestURL {
			report.Findings = append(report.Findings, scanTestURLs(path, node, fset)...)
		}
		if cfg.CheckBuildTag {
			report.Findings = append(report.Findings, scanBuildTags(path, node, fset)...)
		}
		if cfg.CheckNolint {
			report.Findings = append(report.Findings, scanNolint(path, node, fset, cfg.NolintThreshold)...)
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("scan walk error: %w", err)
	}

	// Compute summary.
	for _, f := range report.Findings {
		switch f.Severity {
		case SeverityError:
			report.Summary.Errors++
		case SeverityWarn:
			report.Summary.Warnings++
		case SeverityInfo:
			report.Summary.Infos++
		}
	}
	report.Summary.TotalFindings = len(report.Findings)

	return report, nil
}

// scanMockImports detects mock-related imports in production code (SCAN-001).
func scanMockImports(file string, node *ast.File, fset *token.FileSet) []Finding {
	var findings []Finding

	for _, imp := range node.Imports {
		impPath := strings.Trim(imp.Path.Value, `"`)
		for _, pattern := range mockImportPatterns {
			if strings.Contains(strings.ToLower(impPath), pattern) {
				findings = append(findings, Finding{
					RuleID:   "SCAN-001",
					Severity: SeverityError,
					File:     file,
					Line:     fset.Position(imp.Pos()).Line,
					Message:  fmt.Sprintf("production code imports mock package: %s", impPath),
					Snippet:  impPath,
				})
				break
			}
		}
	}

	return findings
}

// scanTestingUsage detects testing package usage in production code (SCAN-002).
func scanTestingUsage(file string, node *ast.File, fset *token.FileSet) []Finding {
	var findings []Finding

	ast.Inspect(node, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}

		if ident.Name == "testing" {
			findings = append(findings, Finding{
				RuleID:   "SCAN-002",
				Severity: SeverityError,
				File:     file,
				Line:     fset.Position(n.Pos()).Line,
				Message:  fmt.Sprintf("production code references testing package: testing.%s", sel.Sel.Name),
				Snippet:  fmt.Sprintf("testing.%s", sel.Sel.Name),
			})
		}

		return true
	})

	return findings
}

// scanHardcodedSecrets detects hardcoded API keys/tokens (SCAN-003).
func scanHardcodedSecrets(file string) []Finding {
	var findings []Finding

	data, err := os.ReadFile(file)
	if err != nil {
		return findings
	}

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		for _, pattern := range hardcodedSecretPatterns {
			if pattern.MatchString(line) {
				findings = append(findings, Finding{
					RuleID:   "SCAN-003",
					Severity: SeverityError,
					File:     file,
					Line:     i + 1,
					Message:  "hardcoded secret/token detected",
					Snippet:  truncate(line, 80),
				})
			}
		}
	}

	return findings
}

// scanTestURLs detects hardcoded test URLs/ports in production code (SCAN-004).
func scanTestURLs(file string, node *ast.File, fset *token.FileSet) []Finding {
	var findings []Finding

	ast.Inspect(node, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}

		value := strings.Trim(lit.Value, "\"`")
		for _, pattern := range testURLPatterns {
			if strings.Contains(value, pattern) {
				findings = append(findings, Finding{
					RuleID:   "SCAN-004",
					Severity: SeverityWarn,
					File:     file,
					Line:     fset.Position(lit.Pos()).Line,
					Message:  fmt.Sprintf("hardcoded test URL/port: %s", value),
					Snippet:  value,
				})
				break
			}
		}

		return true
	})

	return findings
}

// scanBuildTags detects build tags in production files (SCAN-005).
func scanBuildTags(file string, node *ast.File, fset *token.FileSet) []Finding {
	var findings []Finding

	for _, group := range node.Comments {
		for _, comment := range group.List {
			text := comment.Text
			if strings.HasPrefix(text, "//go:build") || strings.HasPrefix(text, "// +build") {
				if strings.Contains(text, "test") || strings.Contains(text, "integration") {
					findings = append(findings, Finding{
						RuleID:   "SCAN-005",
						Severity: SeverityError,
						File:     file,
						Line:     fset.Position(comment.Pos()).Line,
						Message:  fmt.Sprintf("build tag in production file: %s", text),
						Snippet:  text,
					})
				}
			}
		}
	}

	return findings
}

// scanNolint counts //nolint directives and flags excess (SCAN-007).
func scanNolint(file string, _ *ast.File, _ *token.FileSet, threshold int) []Finding {
	var findings []Finding

	data, err := os.ReadFile(file)
	if err != nil {
		return findings
	}

	count := strings.Count(string(data), "nolint")

	if count > threshold {
		findings = append(findings, Finding{
			RuleID:   "SCAN-007",
			Severity: SeverityWarn,
			File:     file,
			Line:     1,
			Message:  fmt.Sprintf("//nolint count %d exceeds threshold %d", count, threshold),
			Snippet:  fmt.Sprintf("%d nolint directives", count),
		})
	}

	return findings
}

// HasErrors returns true if the report contains any ERROR-level findings.
func (r *ScanReport) HasErrors() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// ToJSON marshals the report to indented JSON.
func (r *ScanReport) ToJSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// FormatText returns a human-readable text summary of the report.
func (r *ScanReport) FormatText() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Scan Report: %s\n", r.Dir))
	sb.WriteString(fmt.Sprintf("Files scanned: %d\n", r.Summary.TotalFiles))
	sb.WriteString(fmt.Sprintf("Findings: %d (ERROR: %d, WARN: %d, INFO: %d)\n\n",
		r.Summary.TotalFindings, r.Summary.Errors, r.Summary.Warnings, r.Summary.Infos))

	for _, f := range r.Findings {
		sb.WriteString(fmt.Sprintf("[%s] %s %s:%d — %s\n",
			f.Severity, f.RuleID, f.File, f.Line, f.Message))
	}

	return sb.String()
}

func truncate(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// isExcludedDir checks whether the path matches any of the exclude patterns.
// Single-segment patterns (e.g. "vendor", ".git") are matched against the
// directory basename. Multi-segment patterns (e.g. "internal/verify") are
// matched against the path suffix with a path-separator boundary check.
func isExcludedDir(path string, excludeDirs []string) bool {
	for _, excl := range excludeDirs {
		if excl == "" {
			continue
		}
		// Single segment: match basename exactly.
		if !strings.ContainsRune(excl, '/') && !strings.ContainsRune(excl, filepath.Separator) {
			if filepath.Base(path) == excl {
				return true
			}
			continue
		}
		// Multi-segment: match path suffix with proper boundary.
		slashExcl := "/" + filepath.ToSlash(excl)
		pathSlash := filepath.ToSlash(path)
		if strings.HasSuffix(pathSlash, slashExcl) || pathSlash == filepath.ToSlash(excl) {
			return true
		}
	}
	return false
}
