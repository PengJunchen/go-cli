package skill_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/skill"
)

func catalogPerformanceSources(count int) []skill.SkillSourceFile {
	sources := make([]skill.SkillSourceFile, count)
	for index := range sources {
		name := fmt.Sprintf("skill-%04d", index)
		sources[index] = skill.SkillSourceFile{
			RelativePath: name + "/SKILL.md",
			Content: []byte("---\nname: " + name +
				"\ndescription: MixedCase DOCUMENTATION Review Tool Workflow " + strings.Repeat("details ", 24) +
				"\ncategory: Developer\n---\nUseful instructions.\n"),
		}
	}
	return sources
}

func TestCatalogMissingSearchDoesNotAllocatePerEntry(t *testing.T) {
	catalog, err := skill.CatalogFromSources(t.Context(), catalogPerformanceSources(1024), skill.CatalogOptions{})
	require.NoError(t, err)
	var result []skill.Descriptor
	allocations := testing.AllocsPerRun(20, func() {
		result = catalog.Search("missing-query", 50)
	})
	require.Nil(t, result)
	require.Zero(t, allocations, "immutable metadata searches must not allocate for every examined entry")
}

var catalogBenchmarkResults []skill.Descriptor
var catalogBenchmarkResult *skill.Catalog
var catalogBenchmarkDefinition skill.SkillDefinition

func BenchmarkCatalogSearch(b *testing.B) {
	for _, count := range []int{128, 1024} {
		catalog, err := skill.CatalogFromSources(context.Background(), catalogPerformanceSources(count), skill.CatalogOptions{})
		if err != nil {
			b.Fatal(err)
		}
		for _, query := range []string{"missing-query", fmt.Sprintf("skill-%04d", count-1), "developer", ""} {
			b.Run(fmt.Sprintf("%d/%q", count, query), func(b *testing.B) {
				b.ReportAllocs()
				for index := 0; index < b.N; index++ {
					catalogBenchmarkResults = catalog.Search(query, 50)
				}
			})
		}
	}
}

func BenchmarkCatalogConstruction(b *testing.B) {
	for _, count := range []int{128, 1024} {
		sources := catalogPerformanceSources(count)
		b.Run(fmt.Sprintf("captured/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				catalog, err := skill.CatalogFromSources(context.Background(), sources, skill.CatalogOptions{})
				if err != nil {
					b.Fatal(err)
				}
				catalogBenchmarkResult = catalog
			}
		})
		root := b.TempDir()
		for _, source := range sources {
			path := filepath.Join(root, source.RelativePath)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(path, source.Content, 0o600); err != nil {
				b.Fatal(err)
			}
		}
		b.Run(fmt.Sprintf("filesystem/%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				catalog, err := skill.DiscoverCatalog(context.Background(), root, skill.CatalogOptions{})
				if err != nil {
					b.Fatal(err)
				}
				catalogBenchmarkResult = catalog
			}
		})
	}
}

func BenchmarkCatalogLoad(b *testing.B) {
	catalog, err := skill.CatalogFromSources(context.Background(), catalogPerformanceSources(1024), skill.CatalogOptions{})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := catalog.Load(context.Background(), "skill-1023"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		definition, err := catalog.Load(context.Background(), "skill-1023")
		if err != nil {
			b.Fatal(err)
		}
		catalogBenchmarkDefinition = definition
	}
}
