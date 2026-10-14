package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSkillLoadReportMixedDirectoryIsStable(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "b-schema.md"), []byte("---\nname: bad-schema\nparameter_schema: |\n  []\n---\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "c-valid.md"), []byte("---\nname: valid\nunknown_field: tolerated\n---\nbody canary\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a-frontmatter.md"), []byte("missing opening delimiter\n"), 0o600))

	loader := NewYAMLSkillLoader()
	reporter, ok := loader.(SkillLoadReporter)
	require.True(t, ok, "the built-in YAML loader should expose directory reports")
	result, err := reporter.LoadDirWithReport(context.Background(), root)
	require.NoError(t, err)
	require.Len(t, result.Definitions, 1)
	assert.Equal(t, "valid", (*result.Definitions[0]).Name())
	assert.Equal(t, SkillLoadSummary{
		Loaded:  1,
		Skipped: 2,
		ErrorCounts: []SkillLoadErrorCount{
			{Class: SkillLoadErrorClassFrontmatter, Count: 1},
			{Class: SkillLoadErrorClassParameterSchema, Count: 1},
		},
	}, result.Report.Summary)
	assert.Equal(t, []SkillLoadDiagnostic{
		{SourceID: `skill:"a-frontmatter.md"`, Class: SkillLoadErrorClassFrontmatter},
		{SourceID: `skill:"b-schema.md"`, Class: SkillLoadErrorClassParameterSchema},
	}, result.Report.Diagnostics)

	repeated, err := reporter.LoadDirWithReport(context.Background(), root)
	require.NoError(t, err)
	assert.Equal(t, result.Report, repeated.Report, "reports should be deterministic across repeated directory walks")

	legacy, err := loader.LoadDir(context.Background(), root)
	require.NoError(t, err)
	assert.Equal(t, result.Definitions, legacy)
}

func TestSkillLoadReportCapturedSourcesUsesProvidedBytesAndBoundsDiagnostics(t *testing.T) {
	root := t.TempDir()
	capturedContent := []byte("---\nname: captured\n---\nCAPTURED_PROMPT_CANARY\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, "valid.md"), capturedContent, 0o600))
	changedContent := []byte("---\nname: changed\n---\nCHANGED_PROMPT_CANARY\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, "valid.md"), changedContent, 0o600))

	sources := make([]SkillSourceFile, 0, maxSkillCandidateFiles)
	sources = append(sources, SkillSourceFile{RelativePath: "valid.md", Content: capturedContent})
	sources = append(sources, SkillSourceFile{RelativePath: "nested/bad\nlabel.md", Content: []byte("RAW_ERROR_CANARY")})
	for i := 2; i < maxSkillCandidateFiles-1; i++ {
		sources = append(sources, SkillSourceFile{RelativePath: fmt.Sprintf("nested/bad-%03d.md", i), Content: []byte("RAW_ERROR_CANARY")})
	}
	longPath := strings.Repeat("q", 300) + ".md"
	sources = append(sources, SkillSourceFile{RelativePath: longPath, Content: []byte("RAW_ERROR_CANARY")})

	loader := NewYAMLSkillLoader()
	reporter, ok := loader.(SkillSourceLoadReporter)
	require.True(t, ok, "the built-in YAML loader should expose captured-source reports")
	result, err := reporter.LoadSourcesWithReport(context.Background(), sources)
	require.NoError(t, err)
	require.Len(t, result.Definitions, 1)
	assert.Equal(t, "CAPTURED_PROMPT_CANARY", (*result.Definitions[0]).Prompt())
	assert.Equal(t, maxSkillCandidateFiles-1, result.Report.Summary.Skipped)
	assert.Equal(t, []SkillLoadErrorCount{{Class: SkillLoadErrorClassFrontmatter, Count: maxSkillCandidateFiles - 1}}, result.Report.Summary.ErrorCounts)
	require.Len(t, result.Report.Diagnostics, maxSkillCandidateFiles-1)
	assert.True(t, sortDiagnostics(result.Report.Diagnostics))

	var controlLabel string
	var longLabel string
	for _, diagnostic := range result.Report.Diagnostics {
		assert.LessOrEqual(t, len(diagnostic.SourceID), 256)
		for _, b := range []byte(diagnostic.SourceID) {
			assert.Less(t, b, byte(0x80), "source labels should be ASCII")
		}
		assert.NotContains(t, diagnostic.SourceID, "\n")
		if strings.Contains(diagnostic.SourceID, "bad\\nlabel") {
			controlLabel = diagnostic.SourceID
		}
		if strings.Contains(diagnostic.SourceID, "~") {
			longLabel = diagnostic.SourceID
		}
	}
	assert.Contains(t, controlLabel, `skill:"nested/bad\nlabel.md"`)
	longDigest := sha256.Sum256([]byte(longPath))
	assert.Contains(t, longLabel, hex.EncodeToString(longDigest[:]))
	assert.Contains(t, longLabel, longPath[:20])
	assert.Contains(t, longLabel, longPath[len(longPath)-12:])

	encoded, err := json.Marshal(result.Report)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "RAW_ERROR_CANARY")
	assert.NotContains(t, string(encoded), "CAPTURED_PROMPT_CANARY")
	assert.NotContains(t, string(encoded), "CHANGED_PROMPT_CANARY")
	assert.NotContains(t, string(encoded), root)
	assert.NotContains(t, string(encoded), longPath)

	legacySourceLoader := loader.(SkillSourceLoader)
	legacy, err := legacySourceLoader.LoadSources(context.Background(), sources)
	require.NoError(t, err)
	assert.Equal(t, result.Definitions, legacy)
}

func TestSkillLoadReporterPreservesLegacyMethodSets(t *testing.T) {
	var loader SkillLoader = legacyDirectorySkillLoader{}
	definitions, err := loader.LoadDir(context.Background(), "unused")
	require.NoError(t, err)
	assert.Empty(t, definitions)

	var sourceLoader SkillSourceLoader = legacySourceSkillLoader{}
	sourceDefinitions, err := sourceLoader.LoadSources(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, sourceDefinitions)

	assert.Equal(t, 2, reflect.TypeOf((*SkillLoader)(nil)).Elem().NumMethod())
	assert.Equal(t, 1, reflect.TypeOf((*SkillSourceLoader)(nil)).Elem().NumMethod())
	assert.Equal(t, 1, reflect.TypeOf((*SkillLoadReporter)(nil)).Elem().NumMethod())
	assert.Equal(t, 1, reflect.TypeOf((*SkillSourceLoadReporter)(nil)).Elem().NumMethod())
}

func TestSkillLoadReportFatalAndCanceledLoadsReturnZeroReport(t *testing.T) {
	loader := NewYAMLSkillLoader()
	reporter := loader.(SkillLoadReporter)

	t.Run("pre-canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := reporter.LoadDirWithReport(ctx, t.TempDir())
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, SkillLoadResult{}, result)
	})

	t.Run("fatal candidate after a skipped file", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "a-malformed.md"), []byte("bad"), 0o600))
		require.NoError(t, os.Mkdir(filepath.Join(root, "z-fatal.md"), 0o700))
		result, err := reporter.LoadDirWithReport(context.Background(), root)
		require.ErrorIs(t, err, ErrSkillUnsafeSource)
		assert.Equal(t, SkillLoadResult{}, result)
	})

	t.Run("resource limit", func(t *testing.T) {
		root := t.TempDir()
		for i := 0; i <= maxSkillCandidateFiles; i++ {
			require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("%03d.md", i)), []byte("bad"), 0o600))
		}
		result, err := reporter.LoadDirWithReport(context.Background(), root)
		require.ErrorIs(t, err, ErrSkillResourceLimit)
		assert.Equal(t, SkillLoadResult{}, result)
	})

	t.Run("unsafe captured source", func(t *testing.T) {
		sourceReporter := loader.(SkillSourceLoadReporter)
		result, err := sourceReporter.LoadSourcesWithReport(context.Background(), []SkillSourceFile{
			{RelativePath: "a-malformed.md", Content: []byte("bad")},
			{RelativePath: "../unsafe.md", Content: []byte("bad")},
		})
		require.ErrorIs(t, err, ErrSkillUnsafeSource)
		assert.Equal(t, SkillLoadResult{}, result)
	})
}

func TestSkillLoadReportClassifiesOnlyKnownSkipErrors(t *testing.T) {
	assert.Equal(t, SkillLoadErrorClassFrontmatter, classifySkillLoadError(fmt.Errorf("wrapped: %w", errParse)))
	assert.Equal(t, SkillLoadErrorClassParameterSchema, classifySkillLoadError(fmt.Errorf("wrapped: %w", ErrSkillParameterSchemaInvalid)))
	assert.Equal(t, SkillLoadErrorClassOther, classifySkillLoadError(errors.New("other")))
	assert.Equal(t, SkillLoadErrorClassFrontmatter, classifySkillLoadError(fmt.Errorf("schema chain: %w", errParse)))
}

func TestSkillLoadReportDiagnosticRejectsUnsafeRelativeIdentities(t *testing.T) {
	for _, path := range []string{
		"",
		".",
		"..",
		"../escape.md",
		"nested/../escape.md",
		"/absolute.md",
		"C:/volume.md",
		`C:\\volume.md`,
		`\\\\server\\share.md`,
	} {
		t.Run(fmt.Sprintf("%q", path), func(t *testing.T) {
			_, ok := skillLoadDiagnosticSourceID(path)
			assert.False(t, ok, "unsafe identities must not appear as diagnostic source ids")
		})
	}
}

func sortDiagnostics(diagnostics []SkillLoadDiagnostic) bool {
	for i := 1; i < len(diagnostics); i++ {
		if diagnostics[i-1].SourceID > diagnostics[i].SourceID {
			return false
		}
	}
	return true
}

type legacyDirectorySkillLoader struct{}

func (legacyDirectorySkillLoader) Load(context.Context, string) (*SkillDefinition, error) {
	return nil, nil
}

func (legacyDirectorySkillLoader) LoadDir(context.Context, string) ([]*SkillDefinition, error) {
	return nil, nil
}

type legacySourceSkillLoader struct{}

func (legacySourceSkillLoader) LoadSources(context.Context, []SkillSourceFile) ([]*SkillDefinition, error) {
	return nil, nil
}
