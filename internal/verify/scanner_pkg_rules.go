// Package verify — package-level scan rules (SCAN-008 through SCAN-013).
// These rules operate on a whole package (all .go files in one directory) and
// use go/ast inspection to detect log, routing, and interface-conformance issues.
package verify

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

// hardcodedDefaultPatterns are keyword markers for string literals that look
// like hardcoded defaults or embedded LLM system prompts (SCAN-009).
var hardcodedDefaultPatterns = []string{
	"default timeout",
	"default value",
	"default values",
	"you are",
	"system prompt",
	"act as",
	"fallback prompt",
	"hardcoded default",
}

// knownCommandNames are command names that should be routed through a command
// registry rather than by string comparison (SCAN-010).
var knownCommandNames = []string{
	"help", "version", "prompt", "run", "start", "stop", "init",
	"config", "status", "execute", "list",
}

// scanExportOnlyUsedByTest detects exported symbols that are only referenced
// from _test.go files within the same package (SCAN-006).
func scanExportOnlyUsedByTest(dir string, goFiles []string) []Finding {
	var findings []Finding

	fset := token.NewFileSet()

	// Phase 1: Collect exported function/method names from production files.
	type exportInfo struct {
		name string
		file string
		line int
	}
	var exports []exportInfo
	prodFiles := []string{}
	testFiles := []string{}

	for _, file := range goFiles {
		if strings.HasSuffix(file, "_test.go") {
			testFiles = append(testFiles, file)
		} else {
			prodFiles = append(prodFiles, file)
		}
	}

	for _, file := range prodFiles {
		node, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			continue
		}
		for _, decl := range node.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				// Only exported functions/methods.
				if d.Name == nil || !ast.IsExported(d.Name.Name) {
					continue
				}
				exports = append(exports, exportInfo{
					name: d.Name.Name,
					file: file,
					line: fset.Position(d.Pos()).Line,
				})
			case *ast.GenDecl:
				// Exported type/var/const declarations.
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if ast.IsExported(s.Name.Name) {
							exports = append(exports, exportInfo{
								name: s.Name.Name,
								file: file,
								line: fset.Position(s.Pos()).Line,
							})
						}
					case *ast.ValueSpec:
						for _, name := range s.Names {
							if ast.IsExported(name.Name) {
								exports = append(exports, exportInfo{
									name: name.Name,
									file: file,
									line: fset.Position(name.Pos()).Line,
								})
							}
						}
					}
				}
			}
		}
	}

	if len(exports) == 0 {
		return findings
	}

	// Phase 2: Build reference sets from production and test files.
	prodRefs := map[string]bool{}
	testRefs := map[string]bool{}

	collectRefs := func(files []string, refSet map[string]bool) {
		for _, file := range files {
			node, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
			if err != nil {
				continue
			}
			ast.Inspect(node, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.Ident:
					if ast.IsExported(x.Name) {
						refSet[x.Name] = true
					}
				case *ast.SelectorExpr:
					if x.Sel != nil && ast.IsExported(x.Sel.Name) {
						refSet[x.Sel.Name] = true
					}
				}
				return true
			})
		}
	}

	collectRefs(prodFiles, prodRefs)
	collectRefs(testFiles, testRefs)

	// Phase 3: Flag exported symbols not referenced in any production file.
	for _, exp := range exports {
		// Skip the declaration itself (self-reference in the same file).
		// The reference set includes the declaration file, so we check if
		// the symbol is used in ANY production file OTHER than where it's declared.
		// We do this by checking if prodRefs contains it from a file other than
		// the declaration file.
		usedInProd := false
		for _, pf := range prodFiles {
			if pf == exp.file {
				continue // Skip declaration file.
			}
			node, err := parser.ParseFile(fset, pf, nil, parser.ParseComments)
			if err != nil {
				continue
			}
			found := false
			ast.Inspect(node, func(n ast.Node) bool {
				if found {
					return false
				}
				switch x := n.(type) {
				case *ast.Ident:
					if x.Name == exp.name {
						found = true
						return false
					}
				case *ast.SelectorExpr:
					if x.Sel != nil && x.Sel.Name == exp.name {
						found = true
						return false
					}
				}
				return true
			})
			if found {
				usedInProd = true
				break
			}
		}

		if !usedInProd {
			// Check if it's at least referenced in test files.
			if testRefs[exp.name] {
				findings = append(findings, Finding{
					RuleID:   "SCAN-006",
					Severity: SeverityWarn,
					File:     exp.file,
					Line:     exp.line,
					Message:  fmt.Sprintf("exported symbol %s only referenced by _test.go files", exp.name),
					Snippet:  exp.name,
				})
			}
		}
	}

	return findings
}
func scanSlogUsage(dir string, goFiles []string) []Finding {
	var findings []Finding

	fset := token.NewFileSet()
	for _, file := range goFiles {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}

		node, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			continue
		}

		// Check for nolint:scan008 directive in comments.
		if hasNolintDirective(node, "scan008") {
			continue
		}

		hasSlog := false
		ast.Inspect(node, func(n ast.Node) bool {
			if hasSlog {
				return false
			}
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "slog" {
				hasSlog = true
				return false
			}
			return true
		})

		if !hasSlog {
			findings = append(findings, Finding{
				RuleID:   "SCAN-008",
				Severity: SeverityWarn,
				File:     file,
				Line:     1,
				Message:  "no slog usage in production code",
				Snippet:  filepath.Base(file),
			})
		}
	}

	return findings
}

// scanHardcodedDefaults detects hardcoded default values/prompts (SCAN-009).
func scanHardcodedDefaults(dir string, goFiles []string) []Finding {
	var findings []Finding

	fset := token.NewFileSet()
	for _, file := range goFiles {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}

		node, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			continue
		}

		ast.Inspect(node, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}

			value := strings.ToLower(strings.Trim(lit.Value, "`\""))

			for _, pattern := range hardcodedDefaultPatterns {
				if strings.Contains(value, pattern) {
					findings = append(findings, Finding{
						RuleID:   "SCAN-009",
						Severity: SeverityError,
						File:     file,
						Line:     fset.Position(lit.Pos()).Line,
						Message:  "hardcoded default/prompt string literal detected",
						Snippet:  truncate(lit.Value, 80),
					})
					break
				}
			}
			return true
		})
	}

	return findings
}

// scanCommandRouting detects hardcoded command routing (SCAN-010).
// It flags string-comparison dispatch (switch on string literal case, or
// `if args[0] == "help"`) when the compared value is a known command name.
func scanCommandRouting(dir string, goFiles []string) []Finding {
	var findings []Finding

	fset := token.NewFileSet()
	for _, file := range goFiles {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}

		node, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			continue
		}

		ast.Inspect(node, func(n ast.Node) bool {
			switch stmt := n.(type) {
			case *ast.SwitchStmt:
				// Check each case clause for a string literal that matches a command name.
				for _, st := range stmt.Body.List {
					if stmtCase, ok := st.(*ast.CaseClause); ok {
						for _, expr := range stmtCase.List {
							if isCommandStringLiteral(fset, file, expr, &findings) {
								break
							}
						}
					}
				}
			case *ast.TypeSwitchStmt:
				// Skip type switches.
				return true
			case *ast.BinaryExpr:
				// Detect `x == "help"` or `"help" == x` string comparisons.
				if stmt.Op.String() == "==" {
					if isCommandStringLiteral(fset, file, stmt.X, &findings) ||
						isCommandStringLiteral(fset, file, stmt.Y, &findings) {
						return true
					}
				}
			}
			return true
		})
	}

	return findings
}

// isCommandStringLiteral reports a finding when expr is a string literal that
// matches a known command name, and returns true if it did.
func isCommandStringLiteral(fset *token.FileSet, file string, expr ast.Expr, findings *[]Finding) bool {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	value := strings.Trim(lit.Value, "`\"")
	for _, cmd := range knownCommandNames {
		if value == cmd {
			*findings = append(*findings, Finding{
				RuleID:   "SCAN-010",
				Severity: SeverityError,
				File:     file,
				Line:     fset.Position(lit.Pos()).Line,
				Message:  "hardcoded command routing via string literal",
				Snippet:  value,
			})
			return true
		}
	}
	return false
}

// scanConfigMergePriority detects configuration merge-priority violations
// (SCAN-011). Config sources must merge in ascending priority order:
// defaults → file → env → CLI, so that a higher-priority source wins.
//
// The heuristic is deterministic and conservative: within each function body,
// assignments to the same config struct field (e.g. cfg.Port = <rhs>) are
// examined in source order. If a later assignment to the field loads a value
// from a LOWER-priority source than an earlier assignment, the higher-priority
// value has been overwritten by a lower-priority one — a priority inversion.
// Compliant code assigns fields in ascending priority, which never triggers.
func scanConfigMergePriority(dir string, goFiles []string) []Finding {
	var findings []Finding

	fset := token.NewFileSet()
	for _, file := range goFiles {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}

		node, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			continue
		}

		for _, decl := range node.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}

			// highestSeen tracks, per config struct field, the highest-priority
			// source that has been assigned to it so far within this function.
			highestSeen := map[string]int{}

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}

				for i, lhs := range as.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel == nil {
						continue
					}

					var rhs ast.Expr
					switch {
					case i < len(as.Rhs):
						rhs = as.Rhs[i]
					case len(as.Rhs) == 1:
						rhs = as.Rhs[0]
					default:
						continue
					}

					fieldKey := exprName(sel.X) + "." + sel.Sel.Name
					priority := configSourcePriority(rhs)
					if prev, seen := highestSeen[fieldKey]; seen && priority < prev {
						findings = append(findings, Finding{
							RuleID:   "SCAN-011",
							Severity: SeverityWarn,
							File:     file,
							Line:     fset.Position(as.Pos()).Line,
							Message: fmt.Sprintf(
								"config merge priority violation: %s assigns a lower-priority source (%s) after a higher-priority source",
								fieldKey, sourceName(priority)),
							Snippet: fieldKey,
						})
					}
					if priority > highestSeen[fieldKey] {
						highestSeen[fieldKey] = priority
					}
				}
				return true
			})
		}
	}

	return findings
}

// configSourcePriority ranks a config-source expression by merge priority.
// Higher values win. Unknown/literal values are treated as defaults (lowest).
func configSourcePriority(expr ast.Expr) int {
	switch {
	case referencesSource(expr, "Args", "Arg", "Flag", "flag"):
		return 4 // CLI
	case referencesSource(expr, "Getenv", "LookupEnv", "Env"):
		return 3 // env
	case referencesSource(expr, "ReadFile", "Unmarshal", "Load", "fromFile", "File"):
		return 2 // file
	default:
		return 1 // defaults
	}
}

// sourceName returns a human-readable name for a config-source priority.
func sourceName(priority int) string {
	switch priority {
	case 4:
		return "CLI"
	case 3:
		return "env"
	case 2:
		return "file"
	default:
		return "defaults"
	}
}

// referencesSource reports whether expr references any of the given markers in
// its identifiers or selector names (e.g. os.Getenv matches "Getenv").
func referencesSource(expr ast.Expr, markers ...string) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		name := ""
		switch t := n.(type) {
		case *ast.Ident:
			name = t.Name
		case *ast.SelectorExpr:
			if t.Sel != nil {
				name = t.Sel.Name
			}
		}
		if name != "" {
			for _, m := range markers {
				if strings.Contains(name, m) {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// exprName returns a stable textual name for an expression, used as part of the
// config-field key. Identifiers return their name; other expressions fall back
// to a source-position-based token so distinct receivers stay distinct.
func exprName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		// e.g. cfg.Sub → "cfg.Sub", using the last identifier segment.
		if t.Sel != nil {
			return exprName(t.X) + "." + t.Sel.Name
		}
	}
	return "?"
}

// scanInterfaceDefaultImpl detects interfaces missing a default implementation
// assertion (var _ Xxx = ...) in the same package (SCAN-012).
func scanInterfaceDefaultImpl(dir string, goFiles []string) []Finding {
	var findings []Finding

	// Collect interfaces declared in the package and the set of interface names
	// referenced by `var _ Xxx = ...` compile-time default-implementation assertions.
	interfaceNames := map[string]bool{}
	implAssertions := map[string]bool{}

	fset := token.NewFileSet()
	for _, file := range goFiles {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}

		node, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			continue
		}

		for _, decl := range node.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}

			for _, spec := range genDecl.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if _, ok := s.Type.(*ast.InterfaceType); ok {
						interfaceNames[s.Name.Name] = true
					}
				case *ast.ValueSpec:
					if len(s.Names) > 0 && s.Names[0].Name == "_" {
						if id, ok := s.Type.(*ast.Ident); ok {
							implAssertions[id.Name] = true
						}
					}
				}
			}
		}
	}

	// Report interfaces that lack any compile-time default implementation assertion.
	for name := range interfaceNames {
		if !implAssertions[name] {
			findings = append(findings, Finding{
				RuleID:   "SCAN-012",
				Severity: SeverityError,
				File:     firstGoFile(goFiles),
				Line:     1,
				Message:  "interface " + name + " missing default implementation",
				Snippet:  name,
			})
		}
	}

	return findings
}

// scanConcreteInInterface detects concrete types used in function/method
// signatures where a matching interface exists in the same package (SCAN-013).
// It uses a conservative heuristic: a non-basic concrete type name T used in a
// parameter or return position is flagged when the package defines an interface
// named IT or TInterface.
func scanConcreteInInterface(dir string, goFiles []string) []Finding {
	var findings []Finding

	interfaceNames := map[string]bool{}
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}

	for _, file := range goFiles {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		node, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			continue
		}
		parsed[file] = node
		for _, decl := range node.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok {
				for _, spec := range gd.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						if _, ok := ts.Type.(*ast.InterfaceType); ok {
							interfaceNames[ts.Name.Name] = true
						}
					}
				}
			}
		}
	}

	for _, node := range parsed {
		ast.Inspect(node, func(n ast.Node) bool {
			switch fn := n.(type) {
			case *ast.FuncDecl:
				if fn.Type != nil {
					checkFieldList(fn.Type.Params, fset, interfaceNames, &findings)
					checkFieldList(fn.Type.Results, fset, interfaceNames, &findings)
				}
			case *ast.FuncLit:
				if fn.Type != nil {
					checkFieldList(fn.Type.Params, fset, interfaceNames, &findings)
					checkFieldList(fn.Type.Results, fset, interfaceNames, &findings)
				}
			}
			return true
		})
	}

	return findings
}

// checkFieldList inspects a parameter/result field list and flags concrete
// types that correspond to a package interface.
func checkFieldList(fields *ast.FieldList, fset *token.FileSet, interfaceNames map[string]bool, findings *[]Finding) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		typ := field.Type
		// Unwrap pointer / slice / map wrappers to reach the base type.
		t := unwrapTypeName(typ)
		concreteType, ok := t.(*ast.Ident)
		if !ok {
			continue
		}
		name := concreteType.Name
		if isBasicTypeName(name) {
			continue
		}
		// Skip if the type itself is an interface — that is the compliant case.
		if interfaceNames[name] {
			continue
		}
		// Look for a corresponding interface name: IT, TInterface, or the
		// concrete type with an implementation prefix stripped (e.g. implService
		// corresponds to the Service interface).
		if hasCorrespondingInterface(interfaceNames, name) {
			*findings = append(*findings, Finding{
				RuleID:   "SCAN-013",
				Severity: SeverityWarn,
				File:     fset.Position(field.Pos()).Filename,
				Line:     fset.Position(field.Pos()).Line,
				Message:  "concrete type " + name + " used in interface position",
				Snippet:  name,
			})
		}
	}
}

// hasCorrespondingInterface reports whether the package defines an interface
// that a concrete type `name` is likely meant to satisfy. It matches direct
// name forms (IT, TInterface) as well as implementation-style prefixes
// (implX, defaultX) against a shared base name.
func hasCorrespondingInterface(interfaceNames map[string]bool, name string) bool {
	if interfaceNames["I"+name] || interfaceNames[name+"Interface"] {
		return true
	}
	// Strip common implementation prefixes and re-check.
	for _, prefix := range []string{"impl", "default", "Default", "std", "Std"} {
		base := strings.TrimPrefix(name, prefix)
		if base == "" || base == name {
			continue
		}
		if interfaceNames[base] || interfaceNames["I"+base] {
			return true
		}
	}
	return false
}

// unwrapTypeName strips pointer/slice/map/array wrappers to return the base type expression.
func unwrapTypeName(expr ast.Expr) ast.Expr {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return unwrapTypeName(t.X)
	case *ast.ArrayType:
		return unwrapTypeName(t.Elt)
	case *ast.MapType:
		return unwrapTypeName(t.Value)
	case *ast.SelectorExpr:
		return t
	default:
		return expr
	}
}

// isBasicTypeName reports whether s is a built-in Go type name.
func isBasicTypeName(s string) bool {
	switch s {
	case "string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"float32", "float64", "complex64", "complex128",
		"bool", "byte", "rune", "error", "any":
		return true
	}
	return false
}

// firstGoFile returns the first non-test Go file in the list, or "" if none.
func firstGoFile(goFiles []string) string {
	for _, f := range goFiles {
		if !strings.HasSuffix(f, "_test.go") {
			return f
		}
	}
	return ""
}

// hasNolintDirective checks whether the file contains a //nolint:<rule>
// directive in its comments.
func hasNolintDirective(node *ast.File, rule string) bool {
	for _, cg := range node.Comments {
		for _, c := range cg.List {
			text := c.Text
			if strings.Contains(text, "nolint:"+rule) || strings.Contains(text, "nolint:all") {
				return true
			}
		}
	}
	return false
}
