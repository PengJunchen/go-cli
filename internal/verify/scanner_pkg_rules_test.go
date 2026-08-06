package verify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// SCAN-006: scanExportOnlyUsedByTest
// ---------------------------------------------------------------------------

func TestScanExportOnlyUsedByTest_Positive(t *testing.T) {
	dir := t.TempDir()
	prodCode := `package example
func ExportedOnlyForTest() int { return 1 }
func UsedInProd() int { return 2 }
`
	testCode := `package example
import "testing"
func TestX(t *testing.T) { ExportedOnlyForTest() }
`

	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(prodCode), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc_test.go"), []byte(testCode), 0o600))

	goFiles := []string{
		filepath.Join(dir, "svc.go"),
		filepath.Join(dir, "svc_test.go"),
	}
	findings := scanExportOnlyUsedByTest(dir, goFiles)

	found := false
	for _, f := range findings {
		if f.RuleID == "SCAN-006" && strings.Contains(f.Message, "ExportedOnlyForTest") {
			found = true
		}
	}
	assert.True(t, found, "should detect ExportedOnlyForTest as only referenced by _test.go")
}

func TestScanExportOnlyUsedByTest_Negative_AlsoUsedInProd(t *testing.T) {
	dir := t.TempDir()
	prodCode := `package example
func UsedInProd() int { return 2 }
`
	otherCode := `package example
func Caller() int { return UsedInProd() }
`

	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(prodCode), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other.go"), []byte(otherCode), 0o600))

	goFiles := []string{
		filepath.Join(dir, "svc.go"),
		filepath.Join(dir, "other.go"),
	}
	findings := scanExportOnlyUsedByTest(dir, goFiles)

	for _, f := range findings {
		assert.NotEqual(t, "SCAN-006", f.RuleID, "should not flag UsedInProd since it's used in another prod file")
	}
}

func TestScanExportOnlyUsedByTest_Negative_NoExports(t *testing.T) {
	dir := t.TempDir()
	code := `package example
func unexported() int { return 1 }
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(code), 0o600))

	goFiles := []string{filepath.Join(dir, "svc.go")}
	findings := scanExportOnlyUsedByTest(dir, goFiles)
	assert.Empty(t, findings, "no exported symbols should yield no findings")
}

func TestScanExportOnlyUsedByTest_ExportedType(t *testing.T) {
	dir := t.TempDir()
	prodCode := `package example
type OnlyTestType struct{ X int }
`
	testCode := `package example
import "testing"
func TestType(t *testing.T) { _ = OnlyTestType{} }
`

	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(prodCode), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc_test.go"), []byte(testCode), 0o600))

	goFiles := []string{
		filepath.Join(dir, "svc.go"),
		filepath.Join(dir, "svc_test.go"),
	}
	findings := scanExportOnlyUsedByTest(dir, goFiles)

	found := false
	for _, f := range findings {
		if f.RuleID == "SCAN-006" && strings.Contains(f.Message, "OnlyTestType") {
			found = true
		}
	}
	assert.True(t, found, "should detect exported type only used in tests")
}

func TestScanExportOnlyUsedByTest_ExportedVar(t *testing.T) {
	dir := t.TempDir()
	prodCode := `package example
var TestOnlyVar = 42
`
	testCode := `package example
import "testing"
func TestVar(t *testing.T) { _ = TestOnlyVar }
`

	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(prodCode), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc_test.go"), []byte(testCode), 0o600))

	goFiles := []string{
		filepath.Join(dir, "svc.go"),
		filepath.Join(dir, "svc_test.go"),
	}
	findings := scanExportOnlyUsedByTest(dir, goFiles)

	found := false
	for _, f := range findings {
		if f.RuleID == "SCAN-006" && strings.Contains(f.Message, "TestOnlyVar") {
			found = true
		}
	}
	assert.True(t, found, "should detect exported var only used in tests")
}

func TestScanExportOnlyUsedByTest_UnparsableFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.go"), []byte("not valid go"), 0o600))

	goFiles := []string{filepath.Join(dir, "bad.go")}
	findings := scanExportOnlyUsedByTest(dir, goFiles)
	assert.Empty(t, findings, "unparsable files should be skipped gracefully")
}

// ---------------------------------------------------------------------------
// SCAN-008: scanSlogUsage
// ---------------------------------------------------------------------------

func TestScanSlogUsage_Positive_NoSlog(t *testing.T) {
	dir := t.TempDir()
	code := `package example
func Run() {}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(code), 0o600))

	findings := scanSlogUsage(dir, []string{filepath.Join(dir, "main.go")})
	assert.Len(t, findings, 1)
	assert.Equal(t, "SCAN-008", findings[0].RuleID)
	assert.Equal(t, SeverityWarn, findings[0].Severity)
}

func TestScanSlogUsage_Negative_WithSlog(t *testing.T) {
	dir := t.TempDir()
	code := `package example
import "log/slog"
func Run() { slog.Info("hello") }
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(code), 0o600))

	findings := scanSlogUsage(dir, []string{filepath.Join(dir, "main.go")})
	assert.Empty(t, findings, "file with slog usage should not trigger SCAN-008")
}

func TestScanSlogUsage_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	code := `package example
func TestRun() {}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main_test.go"), []byte(code), 0o600))

	findings := scanSlogUsage(dir, []string{filepath.Join(dir, "main_test.go")})
	assert.Empty(t, findings, "test files should be skipped")
}

func TestScanSlogUsage_UnparsableFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.go"), []byte("bad code"), 0o600))

	findings := scanSlogUsage(dir, []string{filepath.Join(dir, "bad.go")})
	assert.Empty(t, findings, "unparsable file should be skipped")
}

// ---------------------------------------------------------------------------
// SCAN-009: scanHardcodedDefaults
// ---------------------------------------------------------------------------

func TestScanHardcodedDefaults_Positive(t *testing.T) {
	patterns := []struct {
		name string
		code string
	}{
		{"default timeout", `package example; const T = "default timeout"`},
		{"you are", `package example; const P = "You are a helpful assistant"`},
		{"act as", `package example; const P = "act as a translator"`},
		{"system prompt", `package example; const P = "system prompt for the model"`},
		{"fallback prompt", `package example; const P = "fallback prompt"`},
		{"hardcoded default", `package example; const P = "hardcoded default"`},
		{"default value", `package example; const P = "default value"`},
		{"default values", `package example; const P = "default values"`},
	}

	for _, p := range patterns {
		t.Run(p.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "x.go"), []byte(p.code), 0o600))
			findings := scanHardcodedDefaults(dir, []string{filepath.Join(dir, "x.go")})
			assert.NotEmpty(t, findings, "should detect pattern: %s", p.name)
			assert.Equal(t, "SCAN-009", findings[0].RuleID)
			assert.Equal(t, SeverityError, findings[0].Severity)
		})
	}
}

func TestScanHardcodedDefaults_Negative(t *testing.T) {
	dir := t.TempDir()
	code := `package example
const Normal = "some normal string without patterns"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "clean.go"), []byte(code), 0o600))

	findings := scanHardcodedDefaults(dir, []string{filepath.Join(dir, "clean.go")})
	assert.Empty(t, findings, "clean strings should not trigger SCAN-009")
}

func TestScanHardcodedDefaults_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	code := `package example; const T = "default timeout"`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(code), 0o600))

	findings := scanHardcodedDefaults(dir, []string{filepath.Join(dir, "x_test.go")})
	assert.Empty(t, findings, "test files should be skipped")
}

func TestScanHardcodedDefaults_RawStringLiteral(t *testing.T) {
	dir := t.TempDir()
	// Raw strings with backticks in Go source need special handling.
	src := "package example\nconst T = `default timeout`\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "raw.go"), []byte(src), 0o600))

	findings := scanHardcodedDefaults(dir, []string{filepath.Join(dir, "raw.go")})
	assert.NotEmpty(t, findings, "backtick string literals with patterns should be detected")
}

func TestScanHardcodedDefaults_CaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	code := `package example; const T = "DEFAULT TIMEOUT"`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.go"), []byte(code), 0o600))

	findings := scanHardcodedDefaults(dir, []string{filepath.Join(dir, "x.go")})
	assert.NotEmpty(t, findings, "case-insensitive match should detect 'DEFAULT TIMEOUT'")
}

// ---------------------------------------------------------------------------
// SCAN-010: scanCommandRouting & isCommandStringLiteral
// ---------------------------------------------------------------------------

func TestScanCommandRouting_SwitchPositive(t *testing.T) {
	dir := t.TempDir()
	code := `package example
func Route(args []string) {
	switch args[0] {
	case "help":
		println("help")
	case "version":
		println("v1")
	}
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd.go"), []byte(code), 0o600))
	findings := scanCommandRouting(dir, []string{filepath.Join(dir, "cmd.go")})

	ruleIDs := make(map[string]bool)
	for _, f := range findings {
		ruleIDs[f.RuleID] = true
	}
	assert.True(t, ruleIDs["SCAN-010"], "switch with known command names should trigger SCAN-010")
}

func TestScanCommandRouting_EqualityPositive(t *testing.T) {
	dir := t.TempDir()
	code := `package example
func Route(cmd string) {
	if cmd == "run" {
		println("running")
	}
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd.go"), []byte(code), 0o600))
	findings := scanCommandRouting(dir, []string{filepath.Join(dir, "cmd.go")})

	found := false
	for _, f := range findings {
		if f.RuleID == "SCAN-010" && f.Snippet == "run" {
			found = true
		}
	}
	assert.True(t, found, "equality check with known command should trigger SCAN-010")
}

func TestScanCommandRouting_ReverseEquality(t *testing.T) {
	dir := t.TempDir()
	code := `package example
func Route(cmd string) {
	if "init" == cmd {
		println("initializing")
	}
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd.go"), []byte(code), 0o600))
	findings := scanCommandRouting(dir, []string{filepath.Join(dir, "cmd.go")})

	found := false
	for _, f := range findings {
		if f.RuleID == "SCAN-010" && f.Snippet == "init" {
			found = true
		}
	}
	assert.True(t, found, "reverse equality with known command should trigger SCAN-010")
}

func TestScanCommandRouting_Negative_UnknownCommand(t *testing.T) {
	dir := t.TempDir()
	code := `package example
func Route(cmd string) {
	if cmd == "custom-action" {
		println("custom")
	}
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd.go"), []byte(code), 0o600))
	findings := scanCommandRouting(dir, []string{filepath.Join(dir, "cmd.go")})
	assert.Empty(t, findings, "non-command string comparison should not trigger SCAN-010")
}

func TestScanCommandRouting_Negative_NotEquality(t *testing.T) {
	dir := t.TempDir()
	code := `package example
func Route(cmd string) {
	if cmd != "help" {
		println("not help")
	}
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd.go"), []byte(code), 0o600))
	findings := scanCommandRouting(dir, []string{filepath.Join(dir, "cmd.go")})
	assert.Empty(t, findings, "!= comparison should not trigger SCAN-010")
}

func TestScanCommandRouting_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	code := `package example
func TestRoute() {
	switch "" {
	case "help":
	}
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd_test.go"), []byte(code), 0o600))
	findings := scanCommandRouting(dir, []string{filepath.Join(dir, "cmd_test.go")})
	assert.Empty(t, findings, "test files should be skipped")
}

func TestIsCommandStringLiteral(t *testing.T) {
	fset := token.NewFileSet()
	var findings []Finding

	t.Run("known command", func(t *testing.T) {
		findings = nil
		expr := &ast.BasicLit{Kind: token.STRING, Value: `"help"`}
		result := isCommandStringLiteral(fset, "test.go", expr, &findings)
		assert.True(t, result)
		assert.Len(t, findings, 1)
		assert.Equal(t, "SCAN-010", findings[0].RuleID)
	})

	t.Run("unknown string", func(t *testing.T) {
		findings = nil
		expr := &ast.BasicLit{Kind: token.STRING, Value: `"custom"`}
		result := isCommandStringLiteral(fset, "test.go", expr, &findings)
		assert.False(t, result)
		assert.Empty(t, findings)
	})

	t.Run("non-string literal", func(t *testing.T) {
		findings = nil
		expr := &ast.Ident{Name: "x"}
		result := isCommandStringLiteral(fset, "test.go", expr, &findings)
		assert.False(t, result)
		assert.Empty(t, findings)
	})

	t.Run("all known commands", func(t *testing.T) {
		for _, cmd := range knownCommandNames {
			findings = nil
			expr := &ast.BasicLit{Kind: token.STRING, Value: `"` + cmd + `"`}
			result := isCommandStringLiteral(fset, "test.go", expr, &findings)
			assert.True(t, result, "should detect command: %s", cmd)
		}
	})
}

// ---------------------------------------------------------------------------
// SCAN-011: scanConfigMergePriority & helpers
// ---------------------------------------------------------------------------

func TestScanConfigMergePriority_Positive(t *testing.T) {
	dir := t.TempDir()
	code := `package example
import "os"
type Config struct { Port int }
func Load() Config {
	cfg := Config{}
	cfg.Port = os.Getenv("PORT")
	cfg.Port = 8080
	return cfg
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.go"), []byte(code), 0o600))
	findings := scanConfigMergePriority(dir, []string{filepath.Join(dir, "config.go")})

	assert.NotEmpty(t, findings, "env overwritten by default should be detected")
	assert.Equal(t, "SCAN-011", findings[0].RuleID)
	assert.Equal(t, SeverityWarn, findings[0].Severity)
}

func TestScanConfigMergePriority_Negative_AscendingOrder(t *testing.T) {
	dir := t.TempDir()
	code := `package example
import "os"
type Config struct { Port int }
func Load() Config {
	cfg := Config{}
	cfg.Port = 8080
	cfg.Port = loadFromFile("cfg")
	cfg.Port = os.Getenv("PORT")
	return cfg
}
func loadFromFile(s string) int { return 0 }
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.go"), []byte(code), 0o600))
	findings := scanConfigMergePriority(dir, []string{filepath.Join(dir, "config.go")})
	assert.Empty(t, findings, "ascending priority order should not trigger")
}

func TestScanConfigMergePriority_Negative_SamePriority(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type Config struct { Port int }
func Load() Config {
	cfg := Config{}
	cfg.Port = 8080
	cfg.Port = 9090
	return cfg
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.go"), []byte(code), 0o600))
	findings := scanConfigMergePriority(dir, []string{filepath.Join(dir, "config.go")})
	assert.Empty(t, findings, "same-priority reassignment should not trigger")
}

func TestScanConfigMergePriority_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	code := `package example
import "os"
func TestLoad() {
	cfg.Port = os.Getenv("X")
	cfg.Port = 8080
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config_test.go"), []byte(code), 0o600))
	findings := scanConfigMergePriority(dir, []string{filepath.Join(dir, "config_test.go")})
	assert.Empty(t, findings, "test files should be skipped")
}

func TestScanConfigMergePriority_CLIOverwrittenByEnv(t *testing.T) {
	dir := t.TempDir()
	code := `package example

import (
	"os"
)

type Config struct { Port string }

func Load() Config {
	cfg := Config{}
	cfg.Port = getCLIArg("port")
	cfg.Port = os.Getenv("PORT")
	return cfg
}

func getCLIArg(s string) string { return "" }
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.go"), []byte(code), 0o600))
	findings := scanConfigMergePriority(dir, []string{filepath.Join(dir, "config.go")})
	assert.NotEmpty(t, findings, "CLI (Arg) overwritten by env should trigger SCAN-011")
}

func TestConfigSourcePriority(t *testing.T) {
	fset := token.NewFileSet()

	tests := []struct {
		name     string
		code     string
		expected int
	}{
		{"CLI - Args", `package main; func f() { x.Args }`, 4},
		{"CLI - Flag", `package main; func f() { x.Flag }`, 4},
		{"env - Getenv", `package main; func f() { os.Getenv }`, 3},
		{"env - LookupEnv", `package main; func f() { os.LookupEnv }`, 3},
		{"file - ReadFile", `package main; func f() { os.ReadFile }`, 2},
		{"file - Unmarshal", `package main; func f() { json.Unmarshal }`, 2},
		{"defaults - literal", `package main; func f() { 8080 }`, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node, err := parser.ParseFile(fset, "", tt.code, 0)
			require.NoError(t, err)

			// Find the first expression in the function body.
			var expr ast.Expr
			ast.Inspect(node, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					expr = sel
					return false
				}
				if lit, ok := n.(*ast.BasicLit); ok && expr == nil {
					expr = lit
					return false
				}
				return true
			})
			require.NotNil(t, expr, "could not find expression in code")
			assert.Equal(t, tt.expected, configSourcePriority(expr))
		})
	}
}

func TestSourceName(t *testing.T) {
	assert.Equal(t, "CLI", sourceName(4))
	assert.Equal(t, "env", sourceName(3))
	assert.Equal(t, "file", sourceName(2))
	assert.Equal(t, "defaults", sourceName(1))
	assert.Equal(t, "defaults", sourceName(0))
}

func TestReferencesSource(t *testing.T) {
	fset := token.NewFileSet()

	tests := []struct {
		name    string
		code    string
		markers []string
		want    bool
	}{
		{"matching selector", `package main; func f() { x.Getenv }`, []string{"Getenv"}, true},
		{"matching ident", `package main; var Flag = 1`, []string{"Flag"}, true},
		{"no match", `package main; func f() { x.Something }`, []string{"Getenv"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node, err := parser.ParseFile(fset, "", tt.code, 0)
			require.NoError(t, err)
			var expr ast.Expr
			ast.Inspect(node, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && expr == nil {
					expr = sel
					return false
				}
				return true
			})
			if expr == nil {
				// Try ident
				ast.Inspect(node, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && id.Name != "main" && id.Name != "f" && expr == nil {
						expr = id
						return false
					}
					return true
				})
			}
			require.NotNil(t, expr)
			assert.Equal(t, tt.want, referencesSource(expr, tt.markers...))
		})
	}
}

func TestExprName(t *testing.T) {
	assert.Equal(t, "cfg", exprName(&ast.Ident{Name: "cfg"}))
	assert.Equal(t, "cfg.Port", exprName(&ast.SelectorExpr{
		X:   &ast.Ident{Name: "cfg"},
		Sel: &ast.Ident{Name: "Port"},
	}))
	assert.Equal(t, "?", exprName(&ast.BasicLit{Kind: token.STRING, Value: `"x"`}))
	assert.Equal(t, "cfg.Sub.Port", exprName(&ast.SelectorExpr{
		X: &ast.SelectorExpr{
			X:   &ast.Ident{Name: "cfg"},
			Sel: &ast.Ident{Name: "Sub"},
		},
		Sel: &ast.Ident{Name: "Port"},
	}))
	// SelectorExpr with nil Sel
	assert.Equal(t, "?", exprName(&ast.SelectorExpr{X: &ast.Ident{Name: "cfg"}, Sel: nil}))
}

// ---------------------------------------------------------------------------
// SCAN-012: scanInterfaceDefaultImpl
// ---------------------------------------------------------------------------

func TestScanInterfaceDefaultImpl_Positive(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type Service interface {
	Run() error
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(code), 0o600))
	findings := scanInterfaceDefaultImpl(dir, []string{filepath.Join(dir, "svc.go")})

	assert.NotEmpty(t, findings)
	assert.Equal(t, "SCAN-012", findings[0].RuleID)
	assert.Equal(t, SeverityError, findings[0].Severity)
	assert.Contains(t, findings[0].Message, "Service")
}

func TestScanInterfaceDefaultImpl_Negative_WithAssertion(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type Service interface {
	Run() error
}
type defaultService struct{}
var _ Service = (*defaultService)(nil)
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(code), 0o600))
	findings := scanInterfaceDefaultImpl(dir, []string{filepath.Join(dir, "svc.go")})
	assert.Empty(t, findings, "interface with default implementation assertion should not trigger")
}

func TestScanInterfaceDefaultImpl_Negative_NoInterface(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type Service struct{ X int }
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(code), 0o600))
	findings := scanInterfaceDefaultImpl(dir, []string{filepath.Join(dir, "svc.go")})
	assert.Empty(t, findings, "non-interface type should not trigger")
}

func TestScanInterfaceDefaultImpl_MultipleInterfaces(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type Reader interface { Read() }
type Writer interface { Write() }
var _ Reader = (*fileReader)(nil)
type fileReader struct{}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "io.go"), []byte(code), 0o600))
	findings := scanInterfaceDefaultImpl(dir, []string{filepath.Join(dir, "io.go")})

	// Writer should be flagged, Reader should not.
	found := false
	for _, f := range findings {
		if f.RuleID == "SCAN-012" && strings.Contains(f.Message, "Writer") {
			found = true
		}
	}
	assert.True(t, found, "Writer should be flagged as missing default impl")
	for _, f := range findings {
		assert.NotContains(t, f.Message, "Reader", "Reader has impl assertion and should not be flagged")
	}
}

func TestScanInterfaceDefaultImpl_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type Service interface { Run() error }
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc_test.go"), []byte(code), 0o600))
	findings := scanInterfaceDefaultImpl(dir, []string{filepath.Join(dir, "svc_test.go")})
	assert.Empty(t, findings, "test files should be skipped")
}

// ---------------------------------------------------------------------------
// SCAN-013: scanConcreteInInterface & helpers
// ---------------------------------------------------------------------------

func TestScanConcreteInInterface_Positive(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type IService interface { Run() }
type implService struct{}
func Process(s *implService) {}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(code), 0o600))
	findings := scanConcreteInInterface(dir, []string{filepath.Join(dir, "svc.go")})

	found := false
	for _, f := range findings {
		if f.RuleID == "SCAN-013" && strings.Contains(f.Message, "implService") {
			found = true
		}
	}
	assert.True(t, found, "concrete type implService with corresponding IService should be flagged")
}

func TestScanConcreteInInterface_Positive_InterfaceSuffix(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type HandlerInterface interface { Handle() }
type defaultHandler struct{}
func Do(h *defaultHandler) {}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(code), 0o600))
	findings := scanConcreteInInterface(dir, []string{filepath.Join(dir, "svc.go")})

	// defaultHandler: strip "default" -> Handler; check Handler and IHandler.
	// Neither "Handler" nor "IHandler" is in the interface map (only "HandlerInterface" is).
	// So this should NOT be flagged with the current heuristic.
	found := false
	for _, f := range findings {
		if f.RuleID == "SCAN-013" && strings.Contains(f.Message, "defaultHandler") {
			found = true
		}
	}
	assert.False(t, found, "defaultHandler does not have a direct IHandler/Handler interface match; HandlerInterface does not match by current heuristic")
}

func TestScanConcreteInInterface_Negative_UsesInterface(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type IService interface { Run() }
type implService struct{}
func Process(s IService) {}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(code), 0o600))
	findings := scanConcreteInInterface(dir, []string{filepath.Join(dir, "svc.go")})
	assert.Empty(t, findings, "using the interface directly should not trigger")
}

func TestScanConcreteInInterface_Negative_NoCorrespondingInterface(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type MyStruct struct{}
func Process(s *MyStruct) {}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(code), 0o600))
	findings := scanConcreteInInterface(dir, []string{filepath.Join(dir, "svc.go")})
	assert.Empty(t, findings, "type with no corresponding interface should not trigger")
}

func TestScanConcreteInInterface_Negative_BasicType(t *testing.T) {
	dir := t.TempDir()
	code := `package example
func Process(s string) {}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc.go"), []byte(code), 0o600))
	findings := scanConcreteInInterface(dir, []string{filepath.Join(dir, "svc.go")})
	assert.Empty(t, findings, "basic types should not trigger")
}

func TestScanConcreteInInterface_ReturnType(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type IRepo interface { Find() }
type implRepo struct{}
func NewRepo() *implRepo { return nil }
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "repo.go"), []byte(code), 0o600))
	findings := scanConcreteInInterface(dir, []string{filepath.Join(dir, "repo.go")})

	found := false
	for _, f := range findings {
		if f.RuleID == "SCAN-013" && strings.Contains(f.Message, "implRepo") {
			found = true
		}
	}
	assert.True(t, found, "concrete return type with corresponding interface should be flagged")
}

func TestScanConcreteInInterface_FuncLit(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type IHandler interface { Handle() }
type stdHandler struct{}
func Setup() {
	fn := func(h *stdHandler) {}
	_ = fn
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "handler.go"), []byte(code), 0o600))
	findings := scanConcreteInInterface(dir, []string{filepath.Join(dir, "handler.go")})

	found := false
	for _, f := range findings {
		if f.RuleID == "SCAN-013" && strings.Contains(f.Message, "stdHandler") {
			found = true
		}
	}
	assert.True(t, found, "concrete type in func literal with corresponding interface should be flagged")
}

func TestHasCorrespondingInterface(t *testing.T) {
	ifaceNames := map[string]bool{
		"IService": true,
		"Handler":  true,
		"IHandler": true,
		"Writer":   true,
		"IWriter":  true,
	}

	tests := []struct {
		name string
		want bool
	}{
		{"Service", true},           // IService exists -> I+name
		{"Handler", true},           // IHandler exists -> I+name
		{"implHandler", true},       // strips "impl" -> Handler exists
		{"defaultWriter", true},     // strips "default" -> Writer exists
		{"StdWriter", true},         // strips "Std" -> Writer exists
		{"Unknown", false},          // no match
		{"HandlerInterface", false}, // IHandlerInterface doesn't exist, HandlerInterfaceInterface doesn't exist, no prefix matches
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasCorrespondingInterface(ifaceNames, tt.name))
		})
	}
}

func TestHasCorrespondingInterface_InterfaceSuffix(t *testing.T) {
	ifaceNames := map[string]bool{
		"RepoInterface": true,
	}
	assert.True(t, hasCorrespondingInterface(ifaceNames, "Repo"))
	assert.False(t, hasCorrespondingInterface(ifaceNames, "Other"))
}

// ---------------------------------------------------------------------------
// Helper functions: unwrapTypeName, isBasicTypeName, firstGoFile
// ---------------------------------------------------------------------------

func TestUnwrapTypeName(t *testing.T) {
	tests := []struct {
		name string
		expr ast.Expr
		want string
	}{
		{"ident", &ast.Ident{Name: "MyType"}, "MyType"},
		{"pointer", &ast.StarExpr{X: &ast.Ident{Name: "MyType"}}, "MyType"},
		{"double pointer", &ast.StarExpr{X: &ast.StarExpr{X: &ast.Ident{Name: "MyType"}}}, "MyType"},
		{"slice", &ast.ArrayType{Elt: &ast.Ident{Name: "MyType"}}, "MyType"},
		{"map value", &ast.MapType{Value: &ast.Ident{Name: "MyType"}}, "MyType"},
		{"selector", &ast.SelectorExpr{X: &ast.Ident{Name: "pkg"}, Sel: &ast.Ident{Name: "Type"}}, "pkg.Type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := unwrapTypeName(tt.expr)
			switch r := result.(type) {
			case *ast.Ident:
				assert.Equal(t, tt.want, r.Name)
			case *ast.SelectorExpr:
				xIdent, ok := r.X.(*ast.Ident)
				require.True(t, ok, "expected *ast.Ident for X, got %T", r.X)
				assert.Equal(t, tt.want, xIdent.Name+"."+r.Sel.Name)
			}
		})
	}
}

func TestIsBasicTypeName(t *testing.T) {
	basicTypes := []string{
		"string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"float32", "float64", "complex64", "complex128",
		"bool", "byte", "rune", "error", "any",
	}
	for _, bt := range basicTypes {
		assert.True(t, isBasicTypeName(bt), "%s should be a basic type", bt)
	}

	nonBasic := []string{"MyType", "Context", "Service", "Channel", "map", "chan"}
	for _, nbt := range nonBasic {
		assert.False(t, isBasicTypeName(nbt), "%s should not be a basic type", nbt)
	}
}

func TestFirstGoFile(t *testing.T) {
	tests := []struct {
		name string
		file []string
		want string
	}{
		{"empty", []string{}, ""},
		{"all test", []string{"a_test.go", "b_test.go"}, ""},
		{"first non-test", []string{"a_test.go", "b.go", "c.go"}, "b.go"},
		{"single prod", []string{"main.go"}, "main.go"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, firstGoFile(tt.file))
		})
	}
}

// ---------------------------------------------------------------------------
// Edge cases for scanConfigMergePriority
// ---------------------------------------------------------------------------

func TestScanConfigMergePriority_NilFuncBody(t *testing.T) {
	dir := t.TempDir()
	// Function declaration without body (e.g. cgo)
	code := `package example
func Init()
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.go"), []byte(code), 0o600))
	findings := scanConfigMergePriority(dir, []string{filepath.Join(dir, "x.go")})
	assert.Empty(t, findings, "function with nil body should not crash")
}

func TestScanConfigMergePriority_UnparsableFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.go"), []byte("not valid go"), 0o600))
	findings := scanConfigMergePriority(dir, []string{filepath.Join(dir, "bad.go")})
	assert.Empty(t, findings, "unparsable file should be skipped")
}

func TestScanConfigMergePriority_MultipleRhsSingleAssign(t *testing.T) {
	dir := t.TempDir()
	// Multiple LHS with single RHS: a, b = fn()
	code := `package example
import "os"
type Config struct { Port int; Host int }
func Load() Config {
	cfg := Config{}
	cfg.Port, cfg.Host = os.Getenv("X")
	cfg.Port = 8080
	return cfg
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.go"), []byte(code), 0o600))
	findings := scanConfigMergePriority(dir, []string{filepath.Join(dir, "config.go")})
	// Should detect that cfg.Port was set from env (priority 3) then overwritten by 8080 (priority 1)
	assert.NotEmpty(t, findings, "should detect priority violation even with multi-LHS assignment")
}

func TestScanCommandRouting_TypeSwitchIgnored(t *testing.T) {
	dir := t.TempDir()
	code := `package example
func Route(v interface{}) {
	switch v.(type) {
	case string:
		println("string")
	case int:
		println("int")
	}
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cmd.go"), []byte(code), 0o600))
	findings := scanCommandRouting(dir, []string{filepath.Join(dir, "cmd.go")})
	assert.Empty(t, findings, "type switch should not trigger SCAN-010")
}

func TestScanConcreteInInterface_UnparsableFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.go"), []byte("not valid go"), 0o600))
	findings := scanConcreteInInterface(dir, []string{filepath.Join(dir, "bad.go")})
	assert.Empty(t, findings, "unparsable file should be skipped")
}

func TestScanConcreteInInterface_SkipsTestFiles(t *testing.T) {
	dir := t.TempDir()
	code := `package example
type IService interface { Run() }
type implService struct{}
func Process(s *implService) {}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "svc_test.go"), []byte(code), 0o600))
	findings := scanConcreteInInterface(dir, []string{filepath.Join(dir, "svc_test.go")})
	assert.Empty(t, findings, "test files should be skipped")
}
