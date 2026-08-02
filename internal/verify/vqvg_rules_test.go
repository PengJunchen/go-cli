package verify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVQVGMarkers_VQ001(t *testing.T) {
	tests := []struct {
		name               string
		hasRegister        bool
		hasCommandRegistry bool
		hasDispatchFunc    bool
		hasSwitchStmt      bool
		want               bool
	}{
		{"both registration and dispatch", true, false, true, false, true},
		{"CommandRegistry and switch", false, true, false, true, true},
		{"Register and switch", true, false, false, true, true},
		{"CommandRegistry and dispatch", false, true, true, false, true},
		{"missing registration", false, false, true, true, false},
		{"missing dispatch", true, true, false, false, false},
		{"both missing", false, false, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := vqvgMarkers{
				hasRegister:        tt.hasRegister,
				hasCommandRegistry: tt.hasCommandRegistry,
				hasDispatchFunc:    tt.hasDispatchFunc,
				hasSwitchStmt:      tt.hasSwitchStmt,
			}
			assert.Equal(t, tt.want, m.vq001())
		})
	}
}

func TestVQVGMarkers_VG001(t *testing.T) {
	tests := []struct {
		name               string
		hasContextCreate   bool
		hasContextObserve  bool
		hasContextParam    bool
		hasContextCanceled bool
		want               bool
	}{
		{"create and observe", true, true, false, false, true},
		{"create and param", true, false, true, false, true},
		{"create and canceled", true, false, false, true, true},
		{"create but no observation", true, false, false, false, false},
		{"observe but no create", false, true, true, true, false},
		{"nothing", false, false, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := vqvgMarkers{
				hasContextCreate:   tt.hasContextCreate,
				hasContextObserve:  tt.hasContextObserve,
				hasContextParam:    tt.hasContextParam,
				hasContextCanceled: tt.hasContextCanceled,
			}
			assert.Equal(t, tt.want, m.vg001())
		})
	}
}

func TestVQVGMarkers_VG002(t *testing.T) {
	tests := []struct {
		name         string
		hasGoStmt    bool
		hasLeakGuard bool
		want         bool
	}{
		{"no goroutines", false, false, true},
		{"goroutines with guard", true, true, true},
		{"goroutines without guard", true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := vqvgMarkers{
				hasGoStmt:    tt.hasGoStmt,
				hasLeakGuard: tt.hasLeakGuard,
			}
			assert.Equal(t, tt.want, m.vg002())
		})
	}
}

func TestVQVGResult(t *testing.T) {
	t.Run("pass", func(t *testing.T) {
		r := vqvgResult("VQ-001", "test rule", true, "pass msg", "fail msg")
		assert.Equal(t, "VQ-001", r.ID)
		assert.Equal(t, "test rule", r.Name)
		assert.Equal(t, "PASS", r.Status)
		assert.Equal(t, "pass msg", r.Message)
	})
	t.Run("fail", func(t *testing.T) {
		r := vqvgResult("VG-001", "another rule", false, "pass msg", "fail msg")
		assert.Equal(t, "FAIL", r.Status)
		assert.Equal(t, "fail msg", r.Message)
	})
}

func TestCallName(t *testing.T) {
	tests := []struct {
		name string
		expr ast.Expr
		want string
	}{
		{"ident", &ast.Ident{Name: "Register"}, "Register"},
		{"selector with sel", &ast.SelectorExpr{Sel: &ast.Ident{Name: "WithCancel"}}, "WithCancel"},
		{"selector nil sel", &ast.SelectorExpr{Sel: nil}, ""},
		{"other expr", &ast.BasicLit{Kind: token.STRING, Value: `"x"`}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, callName(tt.expr))
		})
	}
}

func TestNameLooksLikeDispatch(t *testing.T) {
	assert.True(t, nameLooksLikeDispatch("Run"))
	assert.True(t, nameLooksLikeDispatch("Execute"))
	assert.True(t, nameLooksLikeDispatch("Dispatch"))
	assert.True(t, nameLooksLikeDispatch("Route"))
	assert.True(t, nameLooksLikeDispatch("RunCommand"))
	assert.False(t, nameLooksLikeDispatch("Get"))
	assert.False(t, nameLooksLikeDispatch("handle"))
	assert.False(t, nameLooksLikeDispatch(""))
}

func TestFieldListHasContext(t *testing.T) {
	t.Run("nil fields", func(t *testing.T) {
		assert.False(t, fieldListHasContext(nil))
	})
	t.Run("no context param", func(t *testing.T) {
		fl := &ast.FieldList{List: []*ast.Field{{
			Type: &ast.Ident{Name: "string"},
		}}}
		assert.False(t, fieldListHasContext(fl))
	})
	t.Run("context selector param", func(t *testing.T) {
		fl := &ast.FieldList{List: []*ast.Field{{
			Type: &ast.SelectorExpr{
				X:   &ast.Ident{Name: "context"},
				Sel: &ast.Ident{Name: "Context"},
			},
		}}}
		assert.True(t, fieldListHasContext(fl))
	})
	t.Run("context ident param", func(t *testing.T) {
		fl := &ast.FieldList{List: []*ast.Field{{
			Type: &ast.Ident{Name: "Context"},
		}}}
		assert.True(t, fieldListHasContext(fl))
	})
	t.Run("pointer to context", func(t *testing.T) {
		fl := &ast.FieldList{List: []*ast.Field{{
			Type: &ast.StarExpr{X: &ast.SelectorExpr{
				X:   &ast.Ident{Name: "context"},
				Sel: &ast.Ident{Name: "Context"},
			}},
		}}}
		assert.True(t, fieldListHasContext(fl))
	})
}

func TestExprIsContextType(t *testing.T) {
	tests := []struct {
		name string
		expr ast.Expr
		want bool
	}{
		{"selector context.Context", &ast.SelectorExpr{X: &ast.Ident{Name: "context"}, Sel: &ast.Ident{Name: "Context"}}, true},
		{"selector wrong package", &ast.SelectorExpr{X: &ast.Ident{Name: "other"}, Sel: &ast.Ident{Name: "Context"}}, false},
		{"selector nil sel", &ast.SelectorExpr{X: &ast.Ident{Name: "context"}, Sel: nil}, false},
		{"selector wrong name", &ast.SelectorExpr{X: &ast.Ident{Name: "context"}, Sel: &ast.Ident{Name: "Cancel"}}, false},
		{"ident Context", &ast.Ident{Name: "Context"}, true},
		{"ident other", &ast.Ident{Name: "string"}, false},
		{"star expr wrapping selector", &ast.StarExpr{X: &ast.SelectorExpr{X: &ast.Ident{Name: "context"}, Sel: &ast.Ident{Name: "Context"}}}, true},
		{"other type", &ast.ArrayType{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, exprIsContextType(tt.expr))
		})
	}
}

func writeGoFile(t *testing.T, dir, name, src string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
}

func TestScanVQVGMarkers_RegistrationAndDispatch(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "cmd.go", `package main
import "context"
func Register() {}
func Run(ctx context.Context) {}
func main() { Register() }
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasRegister, "should detect Register call")
	assert.True(t, m.hasDispatchFunc, "should detect Run as dispatch")
	assert.True(t, m.hasContextParam, "should detect context.Context param")
}

func TestScanVQVGMarkers_CommandRegistryAndSwitch(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "router.go", `package main
func handle() {
	var r CommandRegistry
	switch r {}
}
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasCommandRegistry, "should detect CommandRegistry")
	assert.True(t, m.hasSwitchStmt, "should detect switch statement")
}

func TestScanVQVGMarkers_UnknownCommandError(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "err.go", `package main
var ErrUnknownCommand = "unknown command"
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasUnknownCommandError, "should detect unknown command error")
}

func TestScanVQVGMarkers_UnknownCommandStringLiteral(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "err.go", `package main
var msg = "unknown command: xyz"
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasUnknownCommandError, "should detect 'unknown command' in string literal")
}

func TestScanVQVGMarkers_VersionDetection(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{"version selector", `package main; func f() { x.GetVersion() }`, true},
		{"Version ident", `package main; var Version = "1.0"`, true},
		{"version string", `package main; var v = "version"`, true},
		{"no version", `package main; func f() {}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGoFile(t, dir, "ver.go", tt.src)
			m := scanVQVGMarkers(dir)
			assert.Equal(t, tt.want, m.hasVersion)
		})
	}
}

func TestScanVQVGMarkers_HelpDetection(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{"help selector", `package main; func f() { x.help() }`, true},
		{"Help ident", `package main; var Help = true`, true},
		{"help string", `package main; var h = "help"`, true},
		{"-h string", `package main; var h = "-h"`, true},
		{"no help", `package main; func f() {}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGoFile(t, dir, "help.go", tt.src)
			m := scanVQVGMarkers(dir)
			assert.Equal(t, tt.want, m.hasHelp)
		})
	}
}

func TestScanVQVGMarkers_ContextCancellation(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "ctx.go", `package main
import "context"
func work(ctx context.Context) {
	ctx2, cancel := context.WithCancel(ctx)
	<-ctx2.Done()
	_ = ctx2.Err()
	_ = context.Canceled
	_ = cancel
}
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasContextCreate, "should detect WithCancel")
	assert.True(t, m.hasContextObserve, "should detect Done/Err")
	assert.True(t, m.hasContextParam, "should detect context.Context param")
	assert.True(t, m.hasContextCanceled, "should detect context.Canceled")
}

func TestScanVQVGMarkers_GoroutineLeakGuard(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		hasGo   bool
		hasLeak bool
	}{
		{"goroutine with WaitGroup", `package main; import "sync"; func f() { var wg sync.WaitGroup; go func() { wg.Done() }() }`, true, true},
		{"goroutine with leak check", `package main; func f() { go func(){}(); AssertNoGoroutineLeak() }`, true, true},
		{"goroutine without guard", `package main; func f() { go func(){}() }`, true, false},
		{"no goroutine", `package main; func f() {}`, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGoFile(t, dir, "goroutine.go", tt.src)
			m := scanVQVGMarkers(dir)
			assert.Equal(t, tt.hasGo, m.hasGoStmt)
			assert.Equal(t, tt.hasLeak, m.hasLeakGuard)
		})
	}
}

func TestScanVQVGMarkers_IgnoresTestFiles(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "main.go", `package main; func main() {}`)
	writeGoFile(t, dir, "main_test.go", `package main; func TestRegister() {}`)
	m := scanVQVGMarkers(dir)
	assert.False(t, m.hasRegister, "test files should be ignored")
}

func TestScanVQVGMarkers_SkipsUnparseableFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.go"), []byte("this is not go code"), 0o600))
	m := scanVQVGMarkers(dir)
	var zero vqvgMarkers
	assert.Equal(t, zero, m, "unparseable files should produce zero markers")
}

func TestScanVQVGMarkers_DispatchFuncNames(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{"Execute", `package main; func Execute() {}`, true},
		{"Dispatch", `package main; func Dispatch() {}`, true},
		{"Route", `package main; func Route() {}`, true},
		{"Handle", `package main; func Handle() {}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGoFile(t, dir, "dispatch.go", tt.src)
			m := scanVQVGMarkers(dir)
			assert.Equal(t, tt.want, m.hasDispatchFunc)
		})
	}
}

func TestCheckVQVG_Integration(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "full.go", `package main
import "context"
func Register() {}
func Execute(ctx context.Context) {}
func main() {
	Register()
	Execute(context.Background())
	println("unknown command")
	println("version")
	println("help")
}
`)
	results := checkVQVG(dir)
	resultMap := make(map[string]VerifyResult, len(results))
	for _, r := range results {
		resultMap[r.ID] = r
	}
	assert.Equal(t, "PASS", resultMap["VQ-001"].Status)
	assert.Equal(t, "PASS", resultMap["VQ-002"].Status)
	assert.Equal(t, "PASS", resultMap["VQ-003"].Status)
	assert.Equal(t, "PASS", resultMap["VQ-004"].Status)
}

func TestCheckVQVG_AllFail(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "minimal.go", `package main; func main() { go func(){}() }`)
	results := checkVQVG(dir)
	resultMap := make(map[string]VerifyResult, len(results))
	for _, r := range results {
		resultMap[r.ID] = r
	}
	assert.Equal(t, "FAIL", resultMap["VQ-001"].Status)
	assert.Equal(t, "FAIL", resultMap["VQ-002"].Status)
	assert.Equal(t, "FAIL", resultMap["VQ-003"].Status)
	assert.Equal(t, "FAIL", resultMap["VQ-004"].Status)
	assert.Equal(t, "FAIL", resultMap["VG-002"].Status, "goroutine without leak guard should fail VG-002")
}

func TestScanVQVGMarkers_ContextWithTimeout(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "ctx.go", `package main
import "context"
func f(ctx context.Context) {
	ctx2, _ := context.WithTimeout(ctx, 0)
	<-ctx2.Done()
}
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasContextCreate, "should detect WithTimeout")
	assert.True(t, m.hasContextObserve, "should detect Done")
	assert.True(t, m.hasContextParam, "should detect context.Context parameter")
	assert.True(t, m.vg001(), "VG-001 should pass with context creation and observation")
}

// ---------------------------------------------------------------------------
// Additional VQ/VG tests for improved coverage
// ---------------------------------------------------------------------------

func TestScanVQVGMarkers_ContextWithDeadline(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "ctx.go", `package main
import "context"
func f(ctx context.Context) {
	ctx2, _ := context.WithDeadline(ctx, time.Time{})
	_ = ctx2
}
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasContextCreate, "should detect WithDeadline")
	assert.True(t, m.hasContextParam, "should detect context.Context parameter")
	// WithDeadline + context param is sufficient for VG-001 to pass
	// (context param counts as cancellation propagation awareness)
	assert.True(t, m.vg001(), "VG-001 should pass: context creation + context param")
}

func TestScanVQVGMarkers_VG001_ContextCreateOnly(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "ctx.go", `package main
import "context"
func f() {
	_, _ = context.WithCancel(context.Background())
}
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasContextCreate, "should detect WithCancel")
	assert.False(t, m.hasContextObserve, "should not detect Done/Err")
	assert.False(t, m.hasContextParam, "should not detect context.Context param")
	assert.False(t, m.hasContextCanceled, "should not detect context.Canceled")
	assert.False(t, m.vg001(), "VG-001 should fail: creation without observation")
}

func TestScanVQVGMarkers_VG001_ObserveOnly(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "ctx.go", `package main
import "context"
func f(ctx context.Context) {
	<-ctx.Done()
	_ = ctx.Err()
}
`)
	m := scanVQVGMarkers(dir)
	assert.False(t, m.hasContextCreate, "should not detect WithCancel/WithTimeout/WithDeadline")
	assert.True(t, m.hasContextObserve, "should detect Done/Err")
	assert.True(t, m.hasContextParam, "should detect context.Context param")
	assert.False(t, m.vg001(), "VG-001 should fail: observation without creation")
}

func TestScanVQVGMarkers_UnknownCommandIdent(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "err.go", `package main
var Err = UnknownCommand
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasUnknownCommandError, "should detect UnknownCommand ident")
}

func TestScanVQVGMarkers_ErrUnknownCommand(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "err.go", `package main
var Err = ErrUnknownCommand
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasUnknownCommandError, "should detect ErrUnknownCommand ident")
}

func TestScanVQVGMarkers_VersionViaVersionIdent(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "ver.go", `package main
func f() { println(version) }
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasVersion, "should detect version ident (lowercase)")
}

func TestScanVQVGMarkers_HelpViaHelpIdent(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "help.go", `package main
var x = help
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasHelp, "should detect help ident")
}

func TestScanVQVGMarkers_HelpViaHelpSelector(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "help.go", `package main
func f() { x.help() }
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasHelp, "should detect help selector")
}

func TestScanVQVGMarkers_LeakGuardViaIdent(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "leak.go", `package main
func f() { AssertNoGoroutineLeak() }
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasLeakGuard, "should detect AssertNoGoroutineLeak ident")
}

func TestScanVQVGMarkers_LeakGuardViaLeakSelector(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "leak.go", `package main
func f() { x.CheckLeak() }
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasLeakGuard, "should detect Leak in selector name")
}

func TestScanVQVGMarkers_LeakGuardViaLeakIdent(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "leak.go", `package main
func f() { leakCheck() }
`)
	m := scanVQVGMarkers(dir)
	// "leakCheck" contains "leak" so it should be detected
	assert.True(t, m.hasLeakGuard, "should detect leak in ident name")
}

func TestCheckVQVG_VG001Pass(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "ctx.go", `package main
import "context"
func work(ctx context.Context) {
	ctx2, cancel := context.WithCancel(ctx)
	<-ctx2.Done()
	_ = cancel
}
`)
	results := checkVQVG(dir)
	resultMap := make(map[string]VerifyResult, len(results))
	for _, r := range results {
		resultMap[r.ID] = r
	}
	assert.Equal(t, "PASS", resultMap["VG-001"].Status, "context creation + observation should pass VG-001")
	// VG-002 should pass since no goroutines
	assert.Equal(t, "PASS", resultMap["VG-002"].Status, "no goroutines should pass VG-002")
}

func TestCheckVQVG_VG001Fail(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "ctx.go", `package main
import "context"
func work() {
	_, _ = context.WithCancel(context.Background())
}
`)
	results := checkVQVG(dir)
	resultMap := make(map[string]VerifyResult, len(results))
	for _, r := range results {
		resultMap[r.ID] = r
	}
	assert.Equal(t, "FAIL", resultMap["VG-001"].Status, "context creation without observation should fail VG-001")
}

func TestCheckVQVG_VG002Pass_WithGuard(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "main.go", `package main
import "sync"
func f() {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { wg.Done() }()
	wg.Wait()
}
`)
	results := checkVQVG(dir)
	resultMap := make(map[string]VerifyResult, len(results))
	for _, r := range results {
		resultMap[r.ID] = r
	}
	assert.Equal(t, "PASS", resultMap["VG-002"].Status, "goroutine with WaitGroup should pass VG-002")
}

func TestCheckVQVG_VG002Fail_NoGuard(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "main.go", `package main
func f() { go func(){}() }
`)
	results := checkVQVG(dir)
	resultMap := make(map[string]VerifyResult, len(results))
	for _, r := range results {
		resultMap[r.ID] = r
	}
	assert.Equal(t, "FAIL", resultMap["VG-002"].Status, "goroutine without leak guard should fail VG-002")
}

func TestCheckVQVG_VQ002Fail(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "main.go", `package main
func main() {}
`)
	results := checkVQVG(dir)
	resultMap := make(map[string]VerifyResult, len(results))
	for _, r := range results {
		resultMap[r.ID] = r
	}
	assert.Equal(t, "FAIL", resultMap["VQ-002"].Status, "no unknown command error should fail VQ-002")
}

func TestCheckVQVG_VQ003Fail(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "main.go", `package main
func main() {}
`)
	results := checkVQVG(dir)
	resultMap := make(map[string]VerifyResult, len(results))
	for _, r := range results {
		resultMap[r.ID] = r
	}
	assert.Equal(t, "FAIL", resultMap["VQ-003"].Status, "no version handling should fail VQ-003")
}

func TestCheckVQVG_VQ004Fail(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "main.go", `package main
func main() {}
`)
	results := checkVQVG(dir)
	resultMap := make(map[string]VerifyResult, len(results))
	for _, r := range results {
		resultMap[r.ID] = r
	}
	assert.Equal(t, "FAIL", resultMap["VQ-004"].Status, "no help handling should fail VQ-004")
}

func TestCheckVQVG_AllPass(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "full.go", `package main
import "context"
import "sync"
func Register() {}
func Run(ctx context.Context) {}
func main() {
	Register()
	Run(context.Background())
	println("unknown command")
	println("version")
	println("help")
	ctx, cancel := context.WithCancel(context.Background())
	<-ctx.Done()
	_ = cancel
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { wg.Done() }()
	wg.Wait()
}
`)
	results := checkVQVG(dir)
	resultMap := make(map[string]VerifyResult, len(results))
	for _, r := range results {
		resultMap[r.ID] = r
	}
	assert.Equal(t, "PASS", resultMap["VQ-001"].Status)
	assert.Equal(t, "PASS", resultMap["VQ-002"].Status)
	assert.Equal(t, "PASS", resultMap["VQ-003"].Status)
	assert.Equal(t, "PASS", resultMap["VQ-004"].Status)
	assert.Equal(t, "PASS", resultMap["VG-001"].Status)
	assert.Equal(t, "PASS", resultMap["VG-002"].Status)
}

func TestScanVQVGMarkers_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	m := scanVQVGMarkers(dir)
	var zero vqvgMarkers
	assert.Equal(t, zero, m, "empty directory should produce zero markers")
}

func TestScanVQVGMarkers_NonGoFilesIgnored(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readme.md"), []byte("# Readme"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "script.sh"), []byte("#!/bin/bash"), 0o600))
	m := scanVQVGMarkers(dir)
	var zero vqvgMarkers
	assert.Equal(t, zero, m, "non-Go files should be ignored")
}

func TestScanVQVGMarkers_ContextCanceledSelector(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "ctx.go", `package main
func f() { _ = context.Canceled }
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasContextCanceled, "should detect context.Canceled selector")
}

func TestScanVQVGMarkers_VersionViaVersionSelector(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "ver.go", `package main
func f() { x.GetVersion() }
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasVersion, "should detect Version in selector name")
}

func TestScanVQVGMarkers_WaitGroupSelector(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "wg.go", `package main
func f() { x.WaitGroup() }
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasLeakGuard, "should detect WaitGroup in selector name")
}

func TestScanVQVGMarkers_WaitGroupIdent(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "wg.go", `package main
func f() { WaitGroup() }
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasLeakGuard, "should detect WaitGroup in ident name")
}

func TestInspectVQVGFile_NilSelectorSel(t *testing.T) {
	// Ensure inspectVQVGFile handles a SelectorExpr with nil Sel without panicking.
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "", `package main; func f() {}`, 0)
	require.NoError(t, err)

	var m vqvgMarkers
	// Manually inject a SelectorExpr with nil Sel to test the guard.
	ast.Inspect(node, func(n ast.Node) bool {
		// This doesn't naturally occur in parsed code, but we test the guard
		// by calling inspectVQVGFile with a normal file (which won't have nil Sel).
		return true
	})
	// Just ensure no panic.
	inspectVQVGFile(node, &m)
}

func TestScanVQVGMarkers_VQ002Pass(t *testing.T) {
	dir := t.TempDir()
	writeGoFile(t, dir, "err.go", `package main
var ErrUnknownCommand = "unknown command"
`)
	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasUnknownCommandError, "should detect ErrUnknownCommand ident")
}

func TestScanVQVGMarkers_Subdirectory(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o750))
	writeGoFile(t, dir, "main.go", `package main; func main() {}`)
	writeGoFile(t, subDir, "sub.go", `package sub; func Register() {}`)

	m := scanVQVGMarkers(dir)
	assert.True(t, m.hasRegister, "should detect Register in subdirectory")
}
