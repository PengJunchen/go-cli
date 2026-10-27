package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScanExportOnlyUsedByTest_ReferenceSemantics(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller string
		warn   bool
	}{
		{"test only", "package example\nfunc caller() {}", true},
		{"production identifier", "package example\nfunc caller() { Shared() }", false},
		{"named import selector", "package example\nimport other \"example.org/other\"\nfunc caller() { other.Shared() }", false},
		{"default import selector", "package example\nimport \"example.org/other\"\nfunc caller() { other.Shared() }", false},
		{"shadowed selector", "package example\nfunc caller() { other := struct { Shared func() }{}; other.Shared() }", false},
		{"same name declaration", "package example\ntype holder struct{}\nfunc (holder) Shared() {}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			files := []string{filepath.Join(dir, "service.go"), filepath.Join(dir, "caller.go"), filepath.Join(dir, "service_test.go")}
			for i, source := range []string{
				"package example\nfunc Shared() {}\nfunc localCaller() { Shared() }",
				tc.caller,
				"package example_test\nimport alias \"example.org/example\"\nfunc testUse() { alias.Shared() }",
			} {
				require.NoError(t, os.WriteFile(files[i], []byte(source), 0o600))
			}
			findings := scanExportOnlyUsedByTest(dir, files)
			if tc.warn {
				require.Len(t, findings, 1)
				assert.Equal(t, Finding{RuleID: "SCAN-006", Severity: SeverityWarn, File: files[0], Line: 2,
					Message: "exported symbol Shared only referenced by _test.go files", Snippet: "Shared"}, findings[0])
			} else {
				assert.Empty(t, findings)
			}
		})
	}
}

func TestScanner_ExportReferencesStayWithinPackage(t *testing.T) {
	dir := t.TempDir()
	for name, source := range map[string]string{
		"first/service.go":      "package first\nfunc Shared() {}",
		"first/service_test.go": "package first\nfunc testUse() { Shared() }",
		"second/service.go":     "package second\nfunc Shared() {}\nfunc caller() { Shared() }",
		"second/caller.go":      "package second\nfunc callerAgain() { Shared() }",
	} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(source), 0o600))
	}
	report, err := Scan(ScanConfig{Dir: dir, ExcludeFiles: []string{"*_test.go"}, CheckExportOnlyUsedByTest: true})
	require.NoError(t, err)
	require.Len(t, report.Findings, 1)
	assert.Equal(t, filepath.Join(dir, "first/service.go"), report.Findings[0].File)
	assert.Equal(t, "Shared", report.Findings[0].Snippet)
}

func TestScanner_ExportReferencesRefreshOnEveryScan(t *testing.T) {
	dir := t.TempDir()
	service := filepath.Join(dir, "service.go")
	caller := filepath.Join(dir, "caller.go")
	testFile := filepath.Join(dir, "service_test.go")
	require.NoError(t, os.WriteFile(service, []byte("package example\nfunc Shared() {}"), 0o600))
	require.NoError(t, os.WriteFile(testFile, []byte("package example\nfunc testUse() { Shared() }"), 0o600))
	cfg := ScanConfig{Dir: dir, ExcludeFiles: []string{"*_test.go"}, CheckExportOnlyUsedByTest: true}
	for _, tc := range []struct {
		source string
		rule   string
	}{
		{"package example\nfunc caller() {}", "SCAN-006"},
		{"package example\nfunc caller() { Shared() }", ""},
		{"package example\nfunc caller() {}", "SCAN-006"},
		{"package example\nfunc caller( {", "SCAN-PARSE"},
	} {
		require.NoError(t, os.WriteFile(caller, []byte(tc.source), 0o600))
		report, err := Scan(cfg)
		require.NoError(t, err)
		var rules []string
		for _, finding := range report.Findings {
			rules = append(rules, finding.RuleID)
		}
		if tc.rule == "" {
			assert.Empty(t, rules)
		} else {
			assert.Contains(t, rules, tc.rule)
		}
	}
	// Changes to excluded test files must also be visible on the next scan.
	require.NoError(t, os.Remove(caller))
	require.NoError(t, os.WriteFile(testFile, []byte("package example\nfunc testUse() {}"), 0o600))
	report, err := Scan(cfg)
	require.NoError(t, err)
	assert.Empty(t, report.Findings)
}

func BenchmarkScanExportOnlyUsedByTest(b *testing.B) {
	for _, count := range []int{32, 128} {
		b.Run(fmt.Sprintf("exports=%d", count), func(b *testing.B) {
			dir := b.TempDir()
			var declarations, references strings.Builder
			declarations.WriteString("package example\n")
			references.WriteString("package example\nfunc testUse() {\n")
			for i := range count {
				fmt.Fprintf(&declarations, "func Exported%d() {}\n", i)
				fmt.Fprintf(&references, "Exported%d()\n", i)
			}
			references.WriteString("}\n")
			files := []string{filepath.Join(dir, "service.go"), filepath.Join(dir, "service_test.go")}
			for i, source := range []string{declarations.String(), references.String()} {
				if err := os.WriteFile(files[i], []byte(source), 0o600); err != nil {
					b.Fatal(err)
				}
			}
			for i := range 8 {
				file := filepath.Join(dir, fmt.Sprintf("caller%d.go", i))
				source := fmt.Sprintf("package example\nfunc caller%d() {}\n", i)
				if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
					b.Fatal(err)
				}
				files = append(files, file)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				findings := scanExportOnlyUsedByTest(dir, files)
				if len(findings) != count {
					b.Fatalf("got %d findings, want %d", len(findings), count)
				}
			}
		})
	}
}
