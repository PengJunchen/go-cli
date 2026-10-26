package skill

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStandardCatalogDiscoversOnlyEntryPointAndLoadsLazily(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "writer", "references"), 0700))
	entry := filepath.Join(root, "writer", "SKILL.md")
	require.NoError(t, os.WriteFile(entry, []byte("---\nname: writer\ndescription: |\n  Write a clear article\n---\nORIGINAL BODY"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "writer", "references", "not-a-skill.md"), []byte("---\nname: accidental\n---\nreference"), 0600))
	catalog, err := DiscoverCatalog(context.Background(), root, CatalogOptions{})
	require.NoError(t, err)
	require.Len(t, catalog.Search("", 100), 1)
	require.Equal(t, "Write a clear article", catalog.Search("writer", 1)[0].Description)
	// Keeping metadata unchanged while replacing body proves discovery didn't
	// materialize and cache the body. Each activation returns its current bytes.
	require.NoError(t, os.WriteFile(entry, []byte("---\nname: writer\ndescription: |\n  Write a clear article\n---\nREPLACED BODY"), 0600))
	def, err := catalog.Load(context.Background(), "writer")
	require.NoError(t, err)
	require.Equal(t, "REPLACED BODY", def.Prompt())
	require.NoError(t, os.WriteFile(entry, []byte("---\nname: writer\ndescription: changed\n---\nREPLACED BODY"), 0600))
	cached, err := catalog.Load(context.Background(), "writer")
	require.NoError(t, err)
	require.Equal(t, "REPLACED BODY", cached.Prompt())
	fresh, err := DiscoverCatalog(context.Background(), root, CatalogOptions{})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(entry, []byte("---\nname: writer\ndescription: changed again\n---\nbody"), 0600))
	_, err = fresh.Load(context.Background(), "writer")
	require.ErrorIs(t, err, ErrCatalogChanged)
}

func TestCapturedStandardCatalogOwnsBytesAndRejectsDuplicates(t *testing.T) {
	sources := []SkillSourceFile{{RelativePath: "writer/SKILL.md", Content: []byte("---\nname: writer\ndescription: write\n---\nCaptured body")}, {RelativePath: "writer/references/info.md", Content: []byte("reference")}}
	catalog, err := CatalogFromSources(context.Background(), sources, CatalogOptions{})
	require.NoError(t, err)
	sources[0].Content[0] = 'x'
	def, err := catalog.Load(context.Background(), "writer")
	require.NoError(t, err)
	require.Equal(t, "Captured body", def.Prompt())
	_, err = CatalogFromSources(context.Background(), []SkillSourceFile{{RelativePath: "a.md", Content: []byte("---\nname: duplicate\n---\na")}, {RelativePath: "b.md", Content: []byte("---\nname: duplicate\n---\nb")}}, CatalogOptions{})
	require.True(t, errors.Is(err, ErrCatalogDuplicate))
}

func TestCapturedCatalogSkipsPlainREADMEAndKeepsNestedSkill(t *testing.T) {
	catalog, err := CatalogFromSources(context.Background(), []SkillSourceFile{
		{RelativePath: "README.md", Content: []byte("# Project skills\n\nThis directory contains project skills.\n")},
		{RelativePath: "writer/SKILL.md", Content: []byte("---\nname: writer\ndescription: Write clearly\n---\nWriter instructions")},
	}, CatalogOptions{})
	require.NoError(t, err)

	entries := catalog.Search("", 100)
	require.Len(t, entries, 1)
	require.Equal(t, "writer", entries[0].Name)
}

func TestCapturedCatalogPreservesREADMEFlatSkillCompatibility(t *testing.T) {
	catalog, err := CatalogFromSources(context.Background(), []SkillSourceFile{{
		RelativePath: "README.md",
		Content:      []byte("---\nname: readme-skill\ndescription: A documented skill\n---\nSkill instructions"),
	}}, CatalogOptions{})
	require.NoError(t, err)

	entries := catalog.Search("", 100)
	require.Len(t, entries, 1)
	require.Equal(t, "readme-skill", entries[0].Name)
}

func TestCapturedCatalogREADMEHandlingDoesNotMaskInvalidEntries(t *testing.T) {
	validSkill := []byte("---\nname: writer\ndescription: Write clearly\n---\nWriter instructions")
	for _, testCase := range []struct {
		name    string
		sources []SkillSourceFile
		wantErr error
		wantMsg string
	}{
		{
			name:    "malformed README frontmatter",
			sources: []SkillSourceFile{{RelativePath: "README.md", Content: []byte("---\nname: incomplete\n")}},
			wantErr: errParse,
		},
		{
			name:    "malformed standard skill",
			sources: []SkillSourceFile{{RelativePath: "writer/SKILL.md", Content: []byte("not frontmatter\n")}},
			wantErr: errParse,
		},
		{
			name:    "malformed legacy flat skill",
			sources: []SkillSourceFile{{RelativePath: "writer.md", Content: []byte("not frontmatter\n")}},
			wantErr: errParse,
		},
		{
			name:    "invalid skill name",
			sources: []SkillSourceFile{{RelativePath: "writer/SKILL.md", Content: []byte("---\nname: live/check\ndescription: Invalid\n---\n")}},
			wantMsg: "invalid catalog name",
		},
		{
			name: "duplicate name between README and nested skill",
			sources: []SkillSourceFile{
				{RelativePath: "README.md", Content: validSkill},
				{RelativePath: "writer/SKILL.md", Content: validSkill},
			},
			wantErr: ErrCatalogDuplicate,
		},
		{
			name:    "unclean README path",
			sources: []SkillSourceFile{{RelativePath: "./README.md", Content: []byte("# docs\n")}},
			wantErr: ErrSkillUnsafeSource,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := CatalogFromSources(context.Background(), testCase.sources, CatalogOptions{})
			require.Error(t, err)
			if testCase.wantErr != nil {
				require.ErrorIs(t, err, testCase.wantErr)
			}
			if testCase.wantMsg != "" {
				require.ErrorContains(t, err, testCase.wantMsg)
			}
		})
	}
}

func TestCapturedCatalogPlainREADMEHonorsMetadataLimitAndCancellation(t *testing.T) {
	t.Run("metadata limit", func(t *testing.T) {
		_, err := CatalogFromSources(context.Background(), []SkillSourceFile{{
			RelativePath: "README.md",
			Content:      []byte("# documentation that is too long\n"),
		}}, CatalogOptions{MaxMetadataBytes: 8})
		require.ErrorIs(t, err, ErrSkillResourceLimit)
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := CatalogFromSources(ctx, []SkillSourceFile{{
			RelativePath: "README.md",
			Content:      []byte("# documentation\n"),
		}}, CatalogOptions{})
		require.ErrorIs(t, err, context.Canceled)
	})
}
