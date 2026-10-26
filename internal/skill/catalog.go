package skill

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// Descriptor is the bounded metadata advertised before instructions are loaded.
// Path identifies a package entry point, not permission to execute its scripts.
type Descriptor struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category,omitempty"`
	Path        string `json:"path"`
	Digest      string `json:"digest"`
}

// Catalog indexes metadata separately from instruction materialization. It is
// immutable after construction. Load caches one read-only materialization until
// a new Catalog is discovered; callers cannot mutate definition fields.
type Catalog struct {
	mu      sync.Mutex
	loaded  map[string]SkillDefinition
	entries []catalogEntry
	root    string
}

type catalogEntry struct {
	descriptor Descriptor
	metadata   []byte
	captured   []byte
}

// CatalogOptions bounds discovery without depending on the number of references
// or scripts inside standard skill packages. Zero values select safe defaults.
type CatalogOptions struct {
	MaxSkills        int
	MaxMetadataBytes int
	BaseDirectory    string
}

var (
	// ErrCatalogChanged means metadata changed after discovery; refresh first.
	ErrCatalogChanged = errors.New("skill: catalog entry changed; refresh required")
	// ErrCatalogDuplicate reports a conflicting skill name.
	ErrCatalogDuplicate = errors.New("skill: duplicate catalog name")
)

const defaultCatalogSkills = 1024
const defaultMetadataBytes = 64 << 10

// DiscoverCatalog reads only frontmatter from standard SKILL.md packages and
// legacy flat files. A package's references/scripts/assets are never candidates.
func DiscoverCatalog(ctx context.Context, dir string, opts CatalogOptions) (*Catalog, error) {
	span, ctx := tracing.SpanFromContext(ctx, "skill.catalog.discover", tracing.SpanKindInternal)
	defer span.End()
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("skill: catalog root: %w", err)
	}
	handle, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("skill: catalog root: %w", err)
	}
	defer handle.Close() //nolint:errcheck
	catalog := &Catalog{root: root}
	var paths []string
	entries := 0
	err = fs.WalkDir(handle.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > maxSkillWalkEntries {
			return ErrSkillResourceLimit
		}
		if skillPathDepth(path) > maxSkillDirectoryDepth {
			return ErrSkillResourceLimit
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: catalog symlink", ErrSkillUnsafeSource)
		}
		if d.IsDir() {
			if path != "." {
				if _, statErr := handle.Stat(filepath.ToSlash(filepath.Join(path, "SKILL.md"))); statErr == nil {
					paths = append(paths, filepath.ToSlash(filepath.Join(path, "SKILL.md")))
					return fs.SkipDir
				}
			}
			return nil
		}
		if isSkillFileName(d.Name()) {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("skill: discover: %w", err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		f, openErr := handle.Open(path)
		if openErr != nil {
			return nil, fmt.Errorf("skill: metadata open: %w", openErr)
		}
		metadata, readErr := readCatalogMetadata(ctx, f, opts)
		closeErr := f.Close()
		if readErr != nil {
			if errors.Is(readErr, errParse) {
				continue
			}
			return nil, fmt.Errorf("skill: metadata %s: %w", path, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("skill: metadata close: %w", closeErr)
		}
		if err := catalog.add(path, metadata, nil, opts); err != nil {
			return nil, err
		}
	}
	return catalog, nil
}

// CatalogFromSources indexes already verified project bytes without reopening
// mutable paths. The caller retains responsibility for the trust binding.
func CatalogFromSources(ctx context.Context, sources []SkillSourceFile, opts CatalogOptions) (*Catalog, error) {
	catalog := &Catalog{root: opts.BaseDirectory}
	standard := map[string]bool{}
	for _, source := range sources {
		if filepath.Base(source.RelativePath) == "SKILL.md" {
			standard[filepath.ToSlash(filepath.Dir(source.RelativePath))] = true
		}
	}
	sorted := append([]SkillSourceFile(nil), sources...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RelativePath < sorted[j].RelativePath })
	for _, source := range sorted {
		path, err := validateSkillSourcePath(source.RelativePath)
		if err != nil {
			return nil, err
		}
		if !isSkillFileName(filepath.Base(path)) {
			continue
		}
		plainReadme, err := isPlainReadme(ctx, path, source.Content, opts)
		if err != nil {
			return nil, err
		}
		if plainReadme {
			continue
		}
		inside := false
		for dir := range standard {
			if strings.HasPrefix(path, dir+"/") && path != dir+"/SKILL.md" {
				inside = true
				break
			}
		}
		if inside {
			continue
		}
		metadata, err := readCatalogMetadata(ctx, bytes.NewReader(source.Content), opts)
		if err != nil {
			return nil, err
		}
		if err := catalog.add(path, metadata, append([]byte(nil), source.Content...), opts); err != nil {
			return nil, err
		}
	}
	return catalog, nil
}

func isPlainReadme(ctx context.Context, path string, content []byte, opts CatalogOptions) (bool, error) {
	if !strings.EqualFold(filepath.Base(path), "README.md") {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	limit := opts.MaxMetadataBytes
	if limit <= 0 {
		limit = defaultMetadataBytes
	}
	probeLen := len(content)
	if probeLen > limit {
		probeLen = limit
	}
	firstLine := content[:probeLen]
	if newline := bytes.IndexByte(firstLine, '\n'); newline >= 0 {
		firstLine = firstLine[:newline]
	} else if len(content) > limit {
		return false, ErrSkillResourceLimit
	}
	return !bytes.Equal(bytes.TrimSpace(firstLine), []byte(frontmatterDelimiter)), nil
}

func readCatalogMetadata(ctx context.Context, r io.Reader, opts CatalogOptions) ([]byte, error) {
	limit := opts.MaxMetadataBytes
	if limit <= 0 {
		limit = defaultMetadataBytes
	}
	reader := bufio.NewReaderSize(io.LimitReader(r, int64(limit+1)), 1024)
	var metadata bytes.Buffer
	for lineNo := 0; ; lineNo++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, err := reader.ReadString('\n')
		metadata.WriteString(line)
		if metadata.Len() > limit {
			return nil, ErrSkillResourceLimit
		}
		trimmed := strings.TrimSpace(line)
		if lineNo == 0 && trimmed != frontmatterDelimiter {
			return nil, errParse
		}
		if lineNo > 0 && trimmed == frontmatterDelimiter {
			return metadata.Bytes(), nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errParse
			}
			return nil, err
		}
	}
}

func (c *Catalog) add(path string, metadata, captured []byte, opts CatalogOptions) error {
	limit := opts.MaxSkills
	if limit <= 0 {
		limit = defaultCatalogSkills
	}
	if len(c.entries) >= limit {
		return ErrSkillResourceLimit
	}
	var fields struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Category    string `yaml:"category"`
	}
	lines := strings.Split(strings.TrimSpace(string(metadata)), "\n")
	if len(lines) < 3 {
		return errParse
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:len(lines)-1], "\n")), &fields); err != nil {
		return fmt.Errorf("skill: metadata: %w", err)
	}
	if fields.Name == "" {
		fields.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if filepath.Base(path) == "SKILL.md" {
			fields.Name = filepath.Base(filepath.Dir(path))
		}
	}
	if fields.Name == "" || len(fields.Name) > 128 || strings.ContainsAny(fields.Name, "/\\ \t\r\n") {
		return fmt.Errorf("skill: invalid catalog name")
	}
	if len(fields.Description) > 4096 {
		return ErrSkillResourceLimit
	}
	for _, existing := range c.entries {
		if existing.descriptor.Name == fields.Name {
			return fmt.Errorf("%w: %s", ErrCatalogDuplicate, fields.Name)
		}
	}
	digest := sha256.Sum256(metadata)
	c.entries = append(c.entries, catalogEntry{descriptor: Descriptor{Name: fields.Name, Description: fields.Description, Category: fields.Category, Path: path, Digest: hex.EncodeToString(digest[:])}, metadata: append([]byte(nil), metadata...), captured: captured})
	sort.Slice(c.entries, func(i, j int) bool { return c.entries[i].descriptor.Name < c.entries[j].descriptor.Name })
	return nil
}

// Search returns deterministic, bounded metadata matching name or description.
func (c *Catalog) Search(query string, limit int) []Descriptor {
	if c == nil {
		return nil
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	query = strings.ToLower(strings.TrimSpace(query))
	var result []Descriptor
	for _, entry := range c.entries {
		if query == "" || strings.Contains(strings.ToLower(entry.descriptor.Name+" "+entry.descriptor.Description+" "+entry.descriptor.Category), query) {
			result = append(result, entry.descriptor)
			if len(result) == limit {
				break
			}
		}
	}
	return result
}

// Load materializes one package after rechecking its metadata. Project captures
// never touch disk; filesystem catalogs use an os.Root to confine reads.
func (c *Catalog) Load(ctx context.Context, name string) (SkillDefinition, error) {
	if c == nil {
		return nil, ErrSkillNotFound
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if definition := c.loaded[name]; definition != nil {
		return definition, nil
	}
	span, ctx := tracing.SpanFromContext(ctx, "skill.catalog.load", tracing.SpanKindInternal)
	defer span.End()
	for _, entry := range c.entries {
		if entry.descriptor.Name != name {
			continue
		}
		content := entry.captured
		if content == nil {
			root, err := os.OpenRoot(c.root)
			if err != nil {
				return nil, err
			}
			defer root.Close() //nolint:errcheck
			info, err := root.Lstat(entry.descriptor.Path)
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, ErrSkillUnsafeSource
			}
			file, err := root.Open(entry.descriptor.Path)
			if err != nil {
				return nil, err
			}
			defer file.Close() //nolint:errcheck
			content, err = io.ReadAll(io.LimitReader(file, maxSkillFileBytes+1))
			if err != nil {
				return nil, err
			}
			if int64(len(content)) > maxSkillFileBytes {
				return nil, ErrSkillResourceLimit
			}
		}
		metadata, err := readCatalogMetadata(ctx, bytes.NewReader(content), CatalogOptions{})
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(entry.metadata, metadata) {
			return nil, ErrCatalogChanged
		}
		lines, _, err := readSkillLines(ctx, bytes.NewReader(content))
		if err != nil {
			return nil, err
		}
		def, err := parseFrontmatter(lines)
		if err != nil {
			return nil, err
		}
		concrete, ok := def.(*DefaultSkillDefinition)
		if !ok {
			return nil, errors.New("skill: unexpected definition implementation")
		}
		concrete.name = entry.descriptor.Name
		concrete.description = entry.descriptor.Description
		concrete.category = entry.descriptor.Category
		if c.loaded == nil {
			c.loaded = map[string]SkillDefinition{}
		}
		c.loaded[name] = def
		return def, nil
	}
	return nil, ErrSkillNotFound
}

// Entries returns all discovered metadata as a detached slice, for compiling
// generation identities without widening the bounded search result surface.
func (c *Catalog) Entries() []Descriptor {
	if c == nil {
		return nil
	}
	result := make([]Descriptor, len(c.entries))
	for index, entry := range c.entries {
		result[index] = entry.descriptor
	}
	return result
}

// EntryPath resolves a package entry point for instruction-relative references.
// It grants no read/execute authority; ordinary tools retain their policy.
func (c *Catalog) EntryPath(name string) string {
	if c == nil {
		return ""
	}
	for _, entry := range c.entries {
		if entry.descriptor.Name == name {
			if c.root != "" {
				return filepath.Join(c.root, filepath.FromSlash(entry.descriptor.Path))
			}
			return entry.descriptor.Path
		}
	}
	return ""
}
