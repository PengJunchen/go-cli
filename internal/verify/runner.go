// Package verify — Verification runner that executes all verification rules
// and produces a structured report.
package verify

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// EvidenceKind describes what a verification result actually observed.
type EvidenceKind string

const (
	// EvidenceStatic identifies source or repository structure checks.
	EvidenceStatic EvidenceKind = "static"
	// EvidenceStaticHeuristic identifies heuristic source markers that need behavioral confirmation.
	EvidenceStaticHeuristic EvidenceKind = "static_heuristic"
	// EvidenceNotRun identifies a rule with no executed evidence.
	EvidenceNotRun EvidenceKind = "not_run"
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
	// Status is the outcome: PASS, FAIL, SKIP, or HINT.
	Status string `json:"status"`
	// Evidence identifies whether the result is static, heuristic, or unexecuted.
	Evidence EvidenceKind `json:"evidence"`
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
	// Hints is the count of static heuristics that do not prove behavior.
	Hints int `json:"hints"`
}

// AllRules returns the complete list of verification rules.
func AllRules() []VerifyRule {
	return []VerifyRule{
		// ── CLI 命令路由架构 (VQ) ──
		{ID: "VQ-001", Name: "命令注册后可路由执行", Category: "cli", Description: "Register subcommand and route execution"},
		{ID: "VQ-002", Name: "未知命令返回明确错误", Category: "cli", Description: "Unknown command returns error"},
		{ID: "VQ-003", Name: "version 输出正确", Category: "cli", Description: "Version flag and command output"},
		{ID: "VQ-004", Name: "帮助信息完整", Category: "cli", Description: "Help flag shows complete usage"},

		// ── 命令执行循环 (VT) ──
		{ID: "VT-001", Name: "参数解析→配置加载→命令执行循环", Category: "loop", Description: "Parse args → load config → execute command cycle"},
		{ID: "VT-002", Name: "参数优先级合并正确", Category: "loop", Description: "Multi-source config merge priority"},
		{ID: "VT-003", Name: "正常退出终止条件", Category: "loop", Description: "Normal exit: exit_code=0"},
		{ID: "VT-004", Name: "参数错误退出终止条件", Category: "loop", Description: "Args error exit: exit_code=2"},
		{ID: "VT-005", Name: "子命令直接执行", Category: "loop", Description: "Subcommand direct execution mode"},
		{ID: "VT-006", Name: "命令注册可枚举", Category: "loop", Description: "Registered commands are enumerable"},

		// ── 状态管理 (VS) ──
		{ID: "VS-001", Name: "并发安全", Category: "state", Description: "Race test — no data races"},
		{ID: "VS-002", Name: "状态变更可观测", Category: "state", Description: "Every state change emits structured log"},
		{ID: "VS-003", Name: "命令状态正确转换", Category: "state", Description: "State machine: Idle→Running→Completed"},

		// ── 配置管理 (VC) ──
		{ID: "VC-001", Name: "默认值不覆盖文件值", Category: "config", Description: "File values override defaults"},
		{ID: "VC-002", Name: "环境变量覆盖文件值", Category: "config", Description: "Env vars override file values"},
		{ID: "VC-003", Name: "命令行参数覆盖环境变量", Category: "config", Description: "CLI flags override env vars"},

		// ── 信号处理与优雅退出 (VH) ──
		{ID: "VH-001", Name: "SIGINT 触发优雅退出", Category: "signal", Description: "SIGINT triggers graceful shutdown"},
		{ID: "VH-002", Name: "中断取消运行中命令", Category: "signal", Description: "Interrupt cancels running command"},
		{ID: "VH-003", Name: "SIGTERM 同等处理", Category: "signal", Description: "SIGTERM handled like SIGINT"},

		// ── 持久化 (VP) ──
		{ID: "VP-001", Name: "配置文件正确读取", Category: "persistence", Description: "Config file read from JSON"},
		{ID: "VP-002", Name: "配置文件不存在时降级", Category: "persistence", Description: "Fallback to defaults when config missing"},

		// ── Go 语言特性 (VG) ──
		{ID: "VG-001", Name: "context 取消传播", Category: "context", Description: "Context cancellation propagation"},
		{ID: "VG-002", Name: "goroutine 泄漏检测", Category: "context", Description: "Goroutine leak detection (CLI level)"},

		// ── 错误恢复 (VE) ──
		{ID: "VE-001", Name: "错误包装保留链", Category: "error", Description: "Error wrapping preserves chain (%w)"},
		{ID: "VE-002", Name: "非空错误检测", Category: "error", Description: "Non-nil error detection"},
		{ID: "VE-003", Name: "参数类型错误处理", Category: "error", Description: "Type mismatch error handling"},

		// ── 链路追踪完整性 (VT tracing) ──
		{ID: "VT-010", Name: "LLM 请求/响应有追踪span", Category: "tracing", Description: "LLM request/response has trace span"},
		{ID: "VT-011", Name: "工具调用有追踪span", Category: "tracing", Description: "Tool call has trace span with parent"},
		{ID: "VT-012", Name: "命令执行有追踪span", Category: "tracing", Description: "Command execution has trace span"},
		{ID: "VT-013", Name: "追踪链路完整无断裂", Category: "tracing", Description: "All parent_span_id resolvable"},
		{ID: "VT-014", Name: "追踪导出器可替换", Category: "tracing", Description: "Custom TraceExporter receives spans"},
		{ID: "VT-015", Name: "超长对话追踪完整", Category: "tracing", Description: "100+ Turn trace without span loss"},

		// ── 合约驱动测试 (VT contract) ──
		{ID: "VT-016", Name: "Mock Server 先于实现", Category: "contract", Description: "Test files created before implementation"},
		{ID: "VT-017", Name: "多轮场景覆盖", Category: "contract", Description: "Each module has ≥3 multi-turn scenarios"},
		{ID: "VT-018", Name: "超长对话稳定性", Category: "contract", Description: "100+ Turn test: no leak, no OOM"},
		{ID: "VT-019", Name: "合约一致性", Category: "contract", Description: "Mock vs Real behavior identical"},
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
			ID:       "SCAN",
			Name:     "AST Scanner",
			Status:   "FAIL",
			Evidence: EvidenceStatic,
			Message:  fmt.Sprintf("scanner error: %v", err),
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
			ID:       "SCAN",
			Name:     "AST Scanner (mock/hardcoded bypass detection)",
			Status:   status,
			Evidence: EvidenceStatic,
			Message:  message,
		})
	}

	// Phase 2: Check for Go-specific issues via file scanning.
	report.Results = append(report.Results, checkGoFiles(dir)...)

	// Phase 3: Run the VQ-*/VG-* AST heuristics as hints and include the
	// remaining V* rules as SKIP (pending behavioral verification).
	vqvgByID := make(map[string]VerifyResult, 6)
	for _, r := range checkVQVG(dir) {
		vqvgByID[r.ID] = r
	}
	for _, rule := range AllRules() {
		if r, ok := vqvgByID[rule.ID]; ok {
			report.Results = append(report.Results, r)
			continue
		}
		report.Results = append(report.Results, VerifyResult{
			ID:       rule.ID,
			Name:     rule.Name,
			Status:   "SKIP",
			Evidence: EvidenceNotRun,
			Message:  fmt.Sprintf("category=%s — no behavioral check has been run", rule.Category),
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
		case "HINT":
			report.Summary.Hints++
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
			ID:       "GOMOD",
			Name:     "go.mod exists",
			Status:   "PASS",
			Evidence: EvidenceStatic,
			Message:  goModPath,
		})
	} else {
		results = append(results, VerifyResult{
			ID:       "GOMOD",
			Name:     "go.mod exists",
			Status:   "FAIL",
			Evidence: EvidenceStatic,
			Message:  "go.mod not found",
		})
	}

	// Check for .golangci.yml.
	lintPath := filepath.Join(dir, ".golangci.yml")
	if _, err := os.Stat(lintPath); err == nil {
		results = append(results, VerifyResult{
			ID:       "LINT",
			Name:     "golangci-lint config exists",
			Status:   "PASS",
			Evidence: EvidenceStatic,
			Message:  lintPath,
		})
	} else {
		results = append(results, VerifyResult{
			ID:       "LINT",
			Name:     "golangci-lint config exists",
			Status:   "FAIL",
			Evidence: EvidenceStatic,
			Message:  ".golangci.yml not found",
		})
	}

	// Count Go files.
	goFileCount := 0
	testFileCount := 0
	behaviorTestCount := 0
	var testParseErrors []string
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			testFileCount++
			count, parseErr := countBehaviorTestFunctions(path)
			if parseErr != nil {
				testParseErrors = append(testParseErrors, fmt.Sprintf("%s: %v", path, parseErr))
			} else {
				behaviorTestCount += count
			}
		} else {
			goFileCount++
		}
		return nil
	})
	if walkErr != nil {
		results = append(results, VerifyResult{
			ID:       "GOFILES",
			Name:     "Go source files scan",
			Status:   "FAIL",
			Evidence: EvidenceStatic,
			Message:  fmt.Sprintf("filepath.Walk error: %v", walkErr),
		})
		return results
	}

	goFilesResult := VerifyResult{
		ID:       "GOFILES",
		Name:     "Go source files present",
		Status:   "PASS",
		Evidence: EvidenceStatic,
		Message:  fmt.Sprintf("%d source files, %d test files", goFileCount, testFileCount),
	}
	if goFileCount == 0 {
		goFilesResult.Status = "FAIL"
		goFilesResult.Message = "no source files found"
	}
	results = append(results, goFilesResult)

	if len(testParseErrors) > 0 {
		results = append(results, VerifyResult{
			ID:       "TESTCOV",
			Name:     "Behavioral test functions declared",
			Status:   "FAIL",
			Evidence: EvidenceStatic,
			Message:  "could not inspect test files",
			Details:  testParseErrors,
		})
	} else if behaviorTestCount == 0 {
		message := fmt.Sprintf("no behavioral test functions found (%d test files inspected)", testFileCount)
		if testFileCount == 0 {
			message = "no test files found"
		}
		results = append(results, VerifyResult{
			ID:       "TESTCOV",
			Name:     "Behavioral test functions declared",
			Status:   "FAIL",
			Evidence: EvidenceStatic,
			Message:  message,
		})
	} else {
		results = append(results, VerifyResult{
			ID:       "TESTCOV",
			Name:     "Behavioral test functions declared",
			Status:   "PASS",
			Evidence: EvidenceStatic,
			Message:  fmt.Sprintf("%d behavioral test functions in %d test files (not executed by RunVerify)", behaviorTestCount, testFileCount),
		})
	}

	return results
}

func countBehaviorTestFunctions(path string) (int, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return 0, fmt.Errorf("parse test file: %w", err)
	}
	testingAliases := make(map[string]bool)
	for _, imported := range file.Imports {
		importPath, err := strconv.Unquote(imported.Path.Value)
		if err != nil || importPath != "testing" {
			continue
		}
		localName := "testing"
		if imported.Name != nil {
			localName = imported.Name.Name
		}
		if localName != "_" && localName != "." {
			testingAliases[localName] = true
		}
	}
	count := 0
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && isBehaviorTestFunction(function, testingAliases) {
			count++
		}
	}
	return count, nil
}

func isBehaviorTestFunction(function *ast.FuncDecl, testingAliases map[string]bool) bool {
	if function == nil || function.Recv != nil || function.Type == nil || function.Type.Results != nil {
		return false
	}
	if !strings.HasPrefix(function.Name.Name, "Test") {
		return false
	}
	suffix := strings.TrimPrefix(function.Name.Name, "Test")
	if suffix == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(suffix)
	if unicode.IsLower(r) {
		return false
	}
	params := function.Type.Params
	if params == nil || len(params.List) != 1 || len(params.List[0].Names) > 1 {
		return false
	}
	pointer, ok := params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	testingT, ok := pointer.X.(*ast.SelectorExpr)
	if !ok || testingT.Sel == nil || testingT.Sel.Name != "T" {
		return false
	}
	packageName, ok := testingT.X.(*ast.Ident)
	return ok && testingAliases[packageName.Name]
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
	sb.WriteString(fmt.Sprintf("Summary: %d total, %d passed, %d failed, %d skipped, %d hints\n\n",
		report.Summary.Total, report.Summary.Passed, report.Summary.Failed, report.Summary.Skipped, report.Summary.Hints))

	sb.WriteString("───────────────────────────────────────────────\n")
	sb.WriteString("RESULTS\n")
	sb.WriteString("───────────────────────────────────────────────\n")
	for _, r := range report.Results {
		status := r.Status
		if r.Evidence != "" {
			sb.WriteString(fmt.Sprintf("[%s] %s — %s (%dms) [evidence=%s]\n", status, r.ID, r.Name, r.DurationMs, r.Evidence))
		} else {
			sb.WriteString(fmt.Sprintf("[%s] %s — %s (%dms)\n", status, r.ID, r.Name, r.DurationMs))
		}
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
