package skill_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/skill"
)

func TestCatalogSearchPreservesMetadataMatchingAndOrder(t *testing.T) {
	sources := []skill.SkillSourceFile{
		{RelativePath: "z/SKILL.md", Content: []byte("---\nname: Alpha\ndescription: CAFÉ ΣΧΕΔΙΟ 世界\ncategory: Développeur\n---\nalpha body")},
		{RelativePath: "m/SKILL.md", Content: []byte("---\nname: beta\ndescription: Alpha helper\ncategory: Tools\n---\nbeta body")},
		{RelativePath: "a/SKILL.md", Content: []byte("---\nname: zeta\ndescription: Boundary END\ncategory: Ops\n---\nzeta body")},
	}
	for _, mode := range []string{"captured", "filesystem"} {
		t.Run(mode, func(t *testing.T) {
			catalog := buildContractCatalog(t, mode, sources, skill.CatalogOptions{})
			for _, testCase := range []struct {
				query string
				limit int
				want  []string
			}{
				{query: "", want: []string{"Alpha", "beta", "zeta"}},
				{query: "  ALPHA  ", want: []string{"Alpha", "beta"}},
				{query: "alpha", limit: 1, want: []string{"Alpha"}},
				{query: "café", want: []string{"Alpha"}},
				{query: "σχεδιο", want: []string{"Alpha"}},
				{query: "世界", want: []string{"Alpha"}},
				{query: "DÉVELOPPEUR", want: []string{"Alpha"}},
				{query: "alpha café", want: []string{"Alpha"}},
				{query: "end ops", want: []string{"zeta"}},
				{query: "missing-query"},
			} {
				t.Run(fmt.Sprintf("%q/%d", testCase.query, testCase.limit), func(t *testing.T) {
					require.Equal(t, testCase.want, catalogDescriptorNames(catalog.Search(testCase.query, testCase.limit)))
				})
			}
			entries := catalog.Entries()
			require.Equal(t, []string{"Alpha", "beta", "zeta"}, catalogDescriptorNames(entries))
			entries[0].Name = "changed"
			entries[0].Description = "changed"
			require.Equal(t, []string{"Alpha"}, catalogDescriptorNames(catalog.Search("café", 50)))
			require.Nil(t, catalog.Search("changed", 50))
		})
	}
	var absent *skill.Catalog
	require.Nil(t, absent.Search("", 1))
	require.Nil(t, absent.Entries())
}

func TestCatalogSearchKeepsResultLimits(t *testing.T) {
	catalog, err := skill.CatalogFromSources(t.Context(), catalogPerformanceSources(128), skill.CatalogOptions{})
	require.NoError(t, err)
	for _, testCase := range []struct{ limit, want int }{{-1, 100}, {0, 100}, {1, 1}, {50, 50}, {100, 100}, {101, 100}} {
		result := catalog.Search("developer", testCase.limit)
		require.Len(t, result, testCase.want)
		require.Equal(t, "skill-0000", result[0].Name)
	}
	require.Equal(t, []string{"skill-0127"}, catalogDescriptorNames(catalog.Search("skill-0127", 50)))
}

func TestCatalogConstructionPreservesValidationPrecedence(t *testing.T) {
	valid := skill.SkillSourceFile{RelativePath: "a.md", Content: []byte("---\nname: duplicate\n---\nbody")}
	for _, mode := range []string{"captured", "filesystem"} {
		t.Run(mode, func(t *testing.T) {
			for _, testCase := range []struct {
				name    string
				content string
				options skill.CatalogOptions
				want    error
			}{
				{name: "duplicate", content: "---\nname: duplicate\n---\nbody", want: skill.ErrCatalogDuplicate},
				{name: "limit before duplicate", content: "---\nname: duplicate\n---\nbody", options: skill.CatalogOptions{MaxSkills: 1}, want: skill.ErrSkillResourceLimit},
				{name: "limit before malformed YAML", content: "---\nname: [\n---\nbody", options: skill.CatalogOptions{MaxSkills: 1}, want: skill.ErrSkillResourceLimit},
				{name: "description before duplicate", content: "---\nname: duplicate\ndescription: " + strings.Repeat("x", 4097) + "\n---\nbody", want: skill.ErrSkillResourceLimit},
			} {
				t.Run(testCase.name, func(t *testing.T) {
					_, err := constructContractCatalog(t, mode, []skill.SkillSourceFile{valid, {RelativePath: "z.md", Content: []byte(testCase.content)}}, testCase.options)
					require.ErrorIs(t, err, testCase.want)
				})
			}
		})
	}
}

func TestCatalogSearchAcceptsMetadataAtConfiguredBoundary(t *testing.T) {
	metadata := "---\nname: bounds\ncategory: " + strings.Repeat("CATEGORY", 1024) + "\n---\n"
	sources := []skill.SkillSourceFile{{RelativePath: "bounds/SKILL.md", Content: []byte(metadata + "body")}}
	for _, mode := range []string{"captured", "filesystem"} {
		t.Run(mode, func(t *testing.T) {
			catalog := buildContractCatalog(t, mode, sources, skill.CatalogOptions{MaxMetadataBytes: len(metadata)})
			require.Equal(t, []string{"bounds"}, catalogDescriptorNames(catalog.Search("categorycategory", 50)))
			_, err := constructContractCatalog(t, mode, sources, skill.CatalogOptions{MaxMetadataBytes: len(metadata) - 1})
			require.ErrorIs(t, err, skill.ErrSkillResourceLimit)
		})
	}
}

func TestCatalogRefreshKeepsPriorViewAndMaterialization(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "writer.md")
	require.NoError(t, os.WriteFile(path, []byte("---\nname: writer\ndescription: Original metadata\n---\nOriginal body"), 0o600))
	previous, err := skill.DiscoverCatalog(t.Context(), root, skill.CatalogOptions{})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("---\nname: writer\ndescription: Original metadata\n---\nUpdated body"), 0o600))
	definition, err := previous.Load(t.Context(), "writer")
	require.NoError(t, err)
	require.Equal(t, "Updated body", definition.Prompt())
	require.NoError(t, os.WriteFile(path, []byte("---\nname: writer\ndescription: Refreshed metadata\n---\nFresh body"), 0o600))
	fresh, err := skill.DiscoverCatalog(t.Context(), root, skill.CatalogOptions{})
	require.NoError(t, err)
	require.Nil(t, previous.Search("refreshed", 50))
	require.Equal(t, []string{"writer"}, catalogDescriptorNames(previous.Search("original", 50)))
	require.Equal(t, []string{"writer"}, catalogDescriptorNames(fresh.Search("refreshed", 50)))
	cached, err := previous.Load(t.Context(), "writer")
	require.NoError(t, err)
	require.Equal(t, "Updated body", cached.Prompt())
	definition, err = fresh.Load(t.Context(), "writer")
	require.NoError(t, err)
	require.Equal(t, "Fresh body", definition.Prompt())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = skill.DiscoverCatalog(ctx, root, skill.CatalogOptions{})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, []string{"writer"}, catalogDescriptorNames(fresh.Search("refreshed", 50)))
}

func TestCatalogConcurrentSearchEntriesAndLoad(t *testing.T) {
	sources := catalogPerformanceSources(128)
	catalog, err := skill.CatalogFromSources(t.Context(), sources, skill.CatalogOptions{})
	require.NoError(t, err)
	sources[127].Content[0] = 'x'
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 20 {
				result := catalog.Search("skill-0127", 50)
				if len(result) != 1 || result[0].Name != "skill-0127" {
					t.Errorf("unexpected search result: %v", result)
					return
				}
				entries := catalog.Entries()
				entries[127].Name = "detached mutation"
				definition, loadErr := catalog.Load(t.Context(), "skill-0127")
				if loadErr != nil || definition.Prompt() != "Useful instructions." {
					t.Errorf("captured materialization changed: definition=%v error=%v", definition, loadErr)
					return
				}
			}
		}()
	}
	workers.Wait()
}

func catalogDescriptorNames(entries []skill.Descriptor) []string {
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}

func buildContractCatalog(t *testing.T, mode string, sources []skill.SkillSourceFile, options skill.CatalogOptions) *skill.Catalog {
	t.Helper()
	catalog, err := constructContractCatalog(t, mode, sources, options)
	require.NoError(t, err)
	return catalog
}

func constructContractCatalog(t *testing.T, mode string, sources []skill.SkillSourceFile, options skill.CatalogOptions) (*skill.Catalog, error) {
	t.Helper()
	if mode == "captured" {
		return skill.CatalogFromSources(t.Context(), sources, options)
	}
	root := t.TempDir()
	for _, source := range sources {
		path := filepath.Join(root, source.RelativePath)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, source.Content, 0o600))
	}
	return skill.DiscoverCatalog(t.Context(), root, options)
}
