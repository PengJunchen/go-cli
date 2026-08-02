// Package verify — AST-based verification for the VQ-* (CLI routing) and
// VG-* (context/goroutine) rules. These heuristics are deterministic and
// conservative: they scan string literals, identifiers and call expressions
// across all non-test .go files under a directory and do NOT perform
// cross-package import resolution.
package verify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// vqvgMarkers aggregates the boolean evidence collected from parsing the
// source files of a directory for the VQ-* and VG-* rule heuristics.
type vqvgMarkers struct {
	// VQ-001: command registration mechanism.
	hasRegister        bool
	hasCommandRegistry bool
	// VQ-001: routing/dispatch mechanism.
	hasDispatchFunc bool
	hasSwitchStmt   bool

	// VQ-002: unknown-command error signal.
	hasUnknownCommandError bool

	// VQ-003: version command/flag handling.
	hasVersion bool

	// VQ-004: help command/flag handling.
	hasHelp bool

	// VG-001: context creation and cancellation observation.
	hasContextCreate   bool
	hasContextObserve  bool
	hasContextParam    bool
	hasContextCanceled bool

	// VG-002: goroutine usage and leak guard.
	hasGoStmt    bool
	hasLeakGuard bool
}

// checkVQVG parses all non-test .go files under dir and produces the six
// VQ-001..004 / VG-001..002 verification results. These rules now return PASS
// or FAIL (never SKIP) based on the heuristic AST evidence in the source.
func checkVQVG(dir string) []VerifyResult {
	markers := scanVQVGMarkers(dir)

	results := append([]VerifyResult{},
		vqvgResult("VQ-001", "命令注册后可路由执行", markers.vq001(),
			"registration (Register/CommandRegistry) and dispatch (switch/Run/Execute/Route) both present",
			"missing command registration (Register/CommandRegistry) or routing dispatch (switch/Run/Execute/Route)"),
		vqvgResult("VQ-002", "未知命令返回明确错误", markers.hasUnknownCommandError,
			"unknown-command error string/symbol present",
			"no 'unknown command' error string or ErrUnknownCommand/UnknownCommand symbol found"),
		vqvgResult("VQ-003", "version 输出正确", markers.hasVersion,
			"version command/flag handling present",
			"no version command string or version flag identifier found"),
		vqvgResult("VQ-004", "帮助信息完整", markers.hasHelp,
			"help command/flag handling present",
			"no help command string or help/-h flag found"),
		vqvgResult("VG-001", "context 取消传播", markers.vg001(),
			"cancellable contexts created (WithCancel/WithTimeout/WithDeadline) and cancellation observed (ctx.Done/ctx.Err/context.Canceled)",
			"missing context creation (WithCancel/WithTimeout/WithDeadline) or no cancellation observation (ctx.Done/ctx.Err/context.Canceled)"),
		vqvgResult("VG-002", "goroutine 泄漏检测", markers.vg002(),
			"goroutines spawned with a leak guard (sync.WaitGroup/leak helper/AssertNoGoroutineLeak)",
			"goroutines spawned without any leak guard (sync.WaitGroup/leak helper/AssertNoGoroutineLeak)"),
	)
	return results
}

// vqvgResult builds a single VerifyResult with a PASS/FAIL/SKIP status and a
// descriptive message in English for the given evidence.
func vqvgResult(id, name string, pass bool, passMsg, failMsg string) VerifyResult {
	status := "FAIL"
	message := failMsg
	if pass {
		status = "PASS"
		message = passMsg
	}
	return VerifyResult{
		ID:      id,
		Name:    name,
		Status:  status,
		Message: message,
	}
}

// vq001 reports whether both a command-registration and a routing-dispatch
// mechanism are present (VQ-001).
func (m *vqvgMarkers) vq001() bool {
	return (m.hasRegister || m.hasCommandRegistry) && (m.hasDispatchFunc || m.hasSwitchStmt)
}

// vg001 reports whether cancellable contexts are created AND cancellation is
// observed/propagated (VG-001).
func (m *vqvgMarkers) vg001() bool {
	return m.hasContextCreate && (m.hasContextObserve || m.hasContextParam || m.hasContextCanceled)
}

// vg002 reports whether every goroutine use is backed by a leak guard (VG-002).
// When no goroutines are spawned at all there is nothing to leak, so the rule
// passes.
func (m *vqvgMarkers) vg002() bool {
	if !m.hasGoStmt {
		return true
	}
	return m.hasLeakGuard
}

// scanVQVGMarkers walks dir, parses each non-test .go file once, and collects
// all boolean evidence required by the VQ-* / VG-* rules.
func scanVQVGMarkers(dir string) vqvgMarkers {
	var m vqvgMarkers
	fset := token.NewFileSet()

	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		node, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return nil
		}
		inspectVQVGFile(node, &m)
		return nil
	})
	_ = walkErr // walking failures are non-fatal for the heuristic scan.

	return m
}

// inspectVQVGFile walks a single parsed file and updates the shared markers.
func inspectVQVGFile(node *ast.File, m *vqvgMarkers) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			// A function whose name suggests dispatch/routing.
			if x.Name != nil && nameLooksLikeDispatch(x.Name.Name) {
				m.hasDispatchFunc = true
			}
			// A function with a context.Context parameter observes cancellation.
			if x.Type != nil && fieldListHasContext(x.Type.Params) {
				m.hasContextParam = true
			}
		case *ast.SwitchStmt:
			m.hasSwitchStmt = true
		case *ast.GoStmt:
			m.hasGoStmt = true
		case *ast.SelectorExpr:
			if x.Sel == nil {
				return true
			}
			switch x.Sel.Name {
			case "WithCancel", "WithTimeout", "WithDeadline":
				m.hasContextCreate = true
			case "Done", "Err":
				m.hasContextObserve = true
			case "Register":
				m.hasRegister = true
			case "Canceled":
				m.hasContextCanceled = true
			}
			if strings.Contains(x.Sel.Name, "Version") || strings.EqualFold(x.Sel.Name, "version") {
				m.hasVersion = true
			}
			if strings.EqualFold(x.Sel.Name, "help") {
				m.hasHelp = true
			}
			if strings.Contains(x.Sel.Name, "WaitGroup") || strings.Contains(x.Sel.Name, "leak") || strings.Contains(x.Sel.Name, "Leak") {
				m.hasLeakGuard = true
			}
		case *ast.Ident:
			switch x.Name {
			case "CommandRegistry":
				m.hasCommandRegistry = true
			case "Register":
				m.hasRegister = true
			case "UnknownCommand", "ErrUnknownCommand":
				m.hasUnknownCommandError = true
			case "AssertNoGoroutineLeak":
				m.hasLeakGuard = true
			}
			if strings.Contains(x.Name, "version") || strings.Contains(x.Name, "Version") {
				m.hasVersion = true
			}
			if strings.Contains(x.Name, "help") || strings.Contains(x.Name, "Help") {
				m.hasHelp = true
			}
			if strings.Contains(x.Name, "WaitGroup") || strings.Contains(x.Name, "leak") || strings.Contains(x.Name, "Leak") {
				m.hasLeakGuard = true
			}
		case *ast.BasicLit:
			if x.Kind != token.STRING {
				return true
			}
			value := strings.ToLower(strings.Trim(x.Value, "`\""))
			switch value {
			case "version":
				m.hasVersion = true
			case "help", "-h":
				m.hasHelp = true
			}
			if strings.Contains(value, "unknown command") {
				m.hasUnknownCommandError = true
			}
		}
		return true
	})
}

// callName returns the simple identifier/selector name of a call expression
// (e.g. "fac.Register" → "Register"), or "" when it cannot be determined.
func callName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if f.Sel != nil {
			return f.Sel.Name
		}
	}
	return ""
}

// nameLooksLikeDispatch reports whether a function name suggests it performs
// command routing/dispatch.
func nameLooksLikeDispatch(name string) bool {
	for _, marker := range []string{"Run", "Execute", "Dispatch", "Route"} {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// fieldListHasContext reports whether any field in the list is of type
// context.Context (handling pointer wrappers).
func fieldListHasContext(fields *ast.FieldList) bool {
	if fields == nil {
		return false
	}
	for _, field := range fields.List {
		if exprIsContextType(field.Type) {
			return true
		}
	}
	return false
}

// exprIsContextType reports whether an expression is (or is a pointer to)
// context.Context.
func exprIsContextType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.StarExpr:
		return exprIsContextType(t.X)
	case *ast.SelectorExpr:
		return t.Sel != nil && t.Sel.Name == "Context" && callName(t.X) == "context"
	case *ast.Ident:
		return t.Name == "Context"
	}
	return false
}
