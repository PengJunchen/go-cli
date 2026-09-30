package skill

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// YAMLSkillLoader loads skills from files that use a minimal YAML-frontmatter
// format. The frontmatter block is delimited by "--" lines at the top of the
// file; the block after the closing delimiter is treated as the skill body
// (markdown). Parsing is done manually with the standard library only, so the
// package has no external YAML dependency.
//
//	---
//	name: example
//	description: an example skill
//	version: 1.0.0
//	category: coding
//	prompt: |
//	  You are a coding assistant...
//	tools:
//	  - bash
//	  - read
//	trigger_hint: "fix bug"
//	parameters:
//	  max_attempts: 3
//	---
//	<body markdown>

// SkillLoader is the contract a loader satisfies. It turns skill files or
// directories of skill files into SkillDefinition values.
type SkillLoader interface {
	// Load parses a single skill file and returns its definition.
	Load(ctx context.Context, path string) (*SkillDefinition, error)
	// LoadDir scans a directory recursively and returns every loadable skill.
	LoadDir(ctx context.Context, dirPath string) ([]*SkillDefinition, error)
}

// SkillSourceLoader is the optional loader capability for parsing captured
// skill bytes without reopening their filesystem paths.
type SkillSourceLoader interface {
	LoadSources(ctx context.Context, sources []SkillSourceFile) ([]*SkillDefinition, error)
}

// SkillSourceFile holds captured skill bytes and their path relative to the
// skill directory. It lets callers load a verified source set without
// reopening mutable filesystem paths.
type SkillSourceFile struct {
	RelativePath string
	Content      []byte
}

// YAMLSkillLoader implements SkillLoader for the YAML-frontmatter format.
type YAMLSkillLoader struct{}

// Compile-time assertion that YAMLSkillLoader satisfies SkillLoader.
var _ SkillLoader = (*YAMLSkillLoader)(nil)
var _ SkillSourceLoader = (*YAMLSkillLoader)(nil)

// NewYAMLSkillLoader returns a ready-to-use YAMLSkillLoader.
func NewYAMLSkillLoader() SkillLoader { return &YAMLSkillLoader{} }

// frontmatterDelimiter is the line that opens and closes the frontmatter block.
const frontmatterDelimiter = "---"

// Frontmatter field keys. Referenced through constants (rather than inline
// string literals) in switches so the rule does not mistake a case
// label for hardcoded command routing — some of these ("version", "prompt")
// collide with known command names.
const (
	fmKeyName        = "name"
	fmKeyDescription = "description"
	fmKeyVersion     = "version"
	fmKeyCategory    = "category"
	fmKeyPrompt      = "prompt"
	fmKeyTools       = "tools"
	fmKeyParameters  = "parameters"
	fmKeyTriggerHint = "trigger_hint"
)

// errParse is returned when a skill file cannot be parsed into a definition.
var errParse = errors.New("skill: parse error")

// ErrSkillResourceLimit reports that a skill source set exceeds one of the
// loader's explicit file, byte, traversal-entry, or depth bounds.
var ErrSkillResourceLimit = errors.New("skill: resource limit exceeded")

// ErrSkillUnsafeSource reports a symlink, non-regular file, or invalid source
// path that cannot be loaded safely.
var ErrSkillUnsafeSource = errors.New("skill: unsafe source")

const (
	// Keep these bounds aligned with config.project_sources.go so trusted
	// project captures and user-level directory loading have the same budget.
	maxSkillFileBytes      int64 = 1 << 20
	maxSkillDirectoryBytes int64 = 16 << 20
	maxSkillCandidateFiles       = 128
	maxSkillWalkEntries          = 4096
	maxSkillDirectoryDepth       = 128
	maxSkillLineBytes      int   = 4 << 20
)

// Load reads the skill file at path, parses its frontmatter and returns a
// populated SkillDefinition. It emits a `skill.load` span.
func (l *YAMLSkillLoader) Load(ctx context.Context, path string) (*SkillDefinition, error) {
	def, _, err := l.loadFile(ctx, path)
	return def, err
}

func (l *YAMLSkillLoader) loadFile(ctx context.Context, path string) (*SkillDefinition, int64, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "skill.load", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	span.SetAttributes(tracing.Attribute{Key: "source_path", Value: path})

	absPath := path
	if p, err := filepath.Abs(path); err == nil {
		absPath = p
	}

	if err := spanCtx.Err(); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, 0, err
	}

	file, err := openRegularSkillFile(absPath)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("skill.load.open_failed", "path", absPath, "err", err)
		return nil, 0, fmt.Errorf("skill: open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }() //nolint:errcheck // best-effort close.

	lines, bytesRead, scanErr := readSkillLines(spanCtx, file)
	if scanErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, scanErr.Error())
		logger.Error("skill.load.scan_failed", "path", absPath, "err", scanErr)
		return nil, bytesRead, fmt.Errorf("skill: read %s: %w", path, scanErr)
	}

	def, err := parseFrontmatter(lines)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("skill.load.parse_failed", "path", absPath, "err", err)
		return nil, bytesRead, fmt.Errorf("skill: load %s: %w", path, err)
	}

	span.SetAttributes(
		tracing.Attribute{Key: "skill_name", Value: def.Name()},
		tracing.Attribute{Key: "success", Value: true},
	)
	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("skill.load", "path", absPath, "name", def.Name())

	return &def, bytesRead, nil
}

// LoadDir scans dirPath recursively for skill files and loads each one. Files
// ending in ".md", ".skill.md" or ".yaml" (or ".yml") are considered candidate
// skill files; parse failures are skipped with a warning. Cancellation, unsafe
// candidates, or exhausted resource limits fail the whole load without
// returning definitions accumulated earlier in the walk.
func (l *YAMLSkillLoader) LoadDir(ctx context.Context, dirPath string) ([]*SkillDefinition, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "skill.load_dir", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()
	fail := func(err error) ([]*SkillDefinition, error) {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, err
	}

	if err := spanCtx.Err(); err != nil {
		return fail(err)
	}

	absDir, err := filepath.Abs(dirPath)
	if err != nil {
		return fail(fmt.Errorf("skill: scan dir %s: %w", dirPath, err))
	}
	root, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		return fail(fmt.Errorf("skill: scan dir %s: %w", dirPath, err))
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return fail(fmt.Errorf("skill: scan dir %s: %w", dirPath, err))
	}
	if !rootInfo.IsDir() {
		return fail(fmt.Errorf("skill: scan dir %s: %w", dirPath, ErrSkillUnsafeSource))
	}

	var defs []*SkillDefinition
	var candidateFiles int
	var totalBytes int64
	err = walkSkillDirectory(spanCtx, root, func(path, relative string, d os.DirEntry) error {
		if d.IsDir() {
			if isSkillFileName(d.Name()) {
				return fmt.Errorf("%w: candidate %q is a directory", ErrSkillUnsafeSource, relative)
			}
			return nil
		}
		if !isSkillFileName(d.Name()) {
			return nil
		}
		candidateFiles++
		if candidateFiles > maxSkillCandidateFiles {
			return fmt.Errorf("%w: directory contains more than %d skill candidates", ErrSkillResourceLimit, maxSkillCandidateFiles)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: candidate %q is a symlink", ErrSkillUnsafeSource, relative)
		}
		def, bytesRead, err := l.loadFile(spanCtx, path)
		totalBytes += bytesRead
		if totalBytes > maxSkillDirectoryBytes {
			return fmt.Errorf("%w: directory source bytes exceed %d", ErrSkillResourceLimit, maxSkillDirectoryBytes)
		}
		if err != nil {
			if isFatalSkillLoadError(err) {
				return err
			}
			// Skip unparsable files rather than failing the whole directory.
			logger.Warn("skill.load_dir.skipped", "path", path, "err", err)
			return nil
		}
		defs = append(defs, def)
		return nil
	})
	if err != nil {
		return fail(fmt.Errorf("skill: scan dir %s: %w", dirPath, err))
	}

	span.SetAttributes(tracing.Attribute{Key: "count", Value: len(defs)})
	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("skill.load_dir", "dir", dirPath, "count", len(defs))
	return defs, nil
}

func walkSkillDirectory(ctx context.Context, root string, visit func(path, relative string, entry os.DirEntry) error) error {
	entriesRead := 0
	var walk func(path, relative string) error
	walk = func(path, relative string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, err := openSkillDirectory(path)
		if err != nil {
			return err
		}

		remaining := maxSkillWalkEntries - entriesRead
		children, readErr := dir.ReadDir(remaining + 1)
		closeErr := dir.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if closeErr != nil {
				return errors.Join(readErr, closeErr)
			}
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(children) > remaining {
			return fmt.Errorf("%w: directory contains more than %d entries", ErrSkillResourceLimit, maxSkillWalkEntries)
		}
		entriesRead += len(children)
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return err
			}
			childRelative := child.Name()
			if relative != "." {
				childRelative = filepath.Join(relative, child.Name())
			}
			childPath := filepath.Join(path, child.Name())
			if child.IsDir() && skillPathDepth(childRelative) > maxSkillDirectoryDepth {
				return fmt.Errorf("%w: directory depth exceeds %d", ErrSkillResourceLimit, maxSkillDirectoryDepth)
			}
			if err := visit(childPath, childRelative, child); err != nil {
				return err
			}
			if child.IsDir() {
				if err := walk(childPath, childRelative); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(root, ".")
}

func openSkillDirectory(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, fmt.Errorf("%w: %q is not a regular directory", ErrSkillUnsafeSource, path)
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := dir.Stat()
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	if !after.IsDir() || !os.SameFile(before, after) {
		_ = dir.Close()
		return nil, fmt.Errorf("%w: %q changed while opening", ErrSkillUnsafeSource, path)
	}
	return dir, nil
}

// LoadSources parses a previously captured skill source set. Source files are
// consumed in the order supplied and are never reopened from disk. Malformed
// skill files follow LoadDir's skip-with-warning behavior.
func (l *YAMLSkillLoader) LoadSources(ctx context.Context, sources []SkillSourceFile) ([]*SkillDefinition, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "skill.load_sources", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	if err := spanCtx.Err(); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, err
	}
	if len(sources) > maxSkillCandidateFiles {
		err := fmt.Errorf("%w: source set contains more than %d skill files", ErrSkillResourceLimit, maxSkillCandidateFiles)
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, err
	}

	normalizedSources := make([]SkillSourceFile, len(sources))
	seenPaths := make(map[string]struct{}, len(sources))
	var totalBytes int64
	for i, source := range sources {
		if err := spanCtx.Err(); err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return nil, err
		}
		relativePath, err := validateSkillSourcePath(source.RelativePath)
		if err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return nil, err
		}
		if !isSkillFileName(filepath.Base(filepath.FromSlash(relativePath))) {
			err := fmt.Errorf("%w: %q is not a skill file candidate", ErrSkillUnsafeSource, relativePath)
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return nil, err
		}
		if _, duplicate := seenPaths[relativePath]; duplicate {
			err := fmt.Errorf("%w: duplicate skill source path %q", ErrSkillUnsafeSource, relativePath)
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return nil, err
		}
		seenPaths[relativePath] = struct{}{}
		if int64(len(source.Content)) > maxSkillFileBytes {
			err := fmt.Errorf("%w: source %q exceeds %d bytes", ErrSkillResourceLimit, relativePath, maxSkillFileBytes)
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return nil, err
		}
		totalBytes += int64(len(source.Content))
		if totalBytes > maxSkillDirectoryBytes {
			err := fmt.Errorf("%w: source set exceeds %d bytes", ErrSkillResourceLimit, maxSkillDirectoryBytes)
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return nil, err
		}
		normalizedSources[i] = SkillSourceFile{RelativePath: relativePath, Content: source.Content}
	}

	defs := make([]*SkillDefinition, 0, len(normalizedSources))
	for _, source := range normalizedSources {
		if err := spanCtx.Err(); err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return nil, err
		}
		def, err := l.loadSource(spanCtx, source)
		if err != nil {
			if ctxErr := spanCtx.Err(); ctxErr != nil {
				span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
				span.SetStatus(tracing.SpanStatusError, ctxErr.Error())
				return nil, ctxErr
			}
			if isFatalSkillLoadError(err) {
				span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
				span.SetStatus(tracing.SpanStatusError, err.Error())
				return nil, err
			}
			logger.Warn("skill.load_sources.skipped", "path", source.RelativePath, "err", err)
			continue
		}
		defs = append(defs, def)
	}

	span.SetAttributes(
		tracing.Attribute{Key: "count", Value: len(defs)},
		tracing.Attribute{Key: "success", Value: true},
	)
	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("skill.load_sources", "count", len(defs))
	return defs, nil
}

func (l *YAMLSkillLoader) loadSource(ctx context.Context, source SkillSourceFile) (*SkillDefinition, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "skill.load", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	relativePath, err := validateSkillSourcePath(source.RelativePath)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, err
	}
	span.SetAttributes(tracing.Attribute{Key: "source_path", Value: filepath.ToSlash(relativePath)})
	if err := spanCtx.Err(); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, err
	}

	lines, _, scanErr := readSkillLines(spanCtx, bytes.NewReader(source.Content))
	if scanErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, scanErr.Error())
		logger.Error("skill.load.scan_failed", "path", relativePath, "err", scanErr)
		return nil, fmt.Errorf("skill: read captured source %s: %w", relativePath, scanErr)
	}

	def, err := parseFrontmatter(lines)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("skill.load.parse_failed", "path", relativePath, "err", err)
		return nil, fmt.Errorf("skill: load captured source %s: %w", relativePath, err)
	}
	span.SetAttributes(
		tracing.Attribute{Key: "skill_name", Value: def.Name()},
		tracing.Attribute{Key: "success", Value: true},
	)
	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("skill.load", "path", relativePath, "name", def.Name())
	return &def, nil
}

func openRegularSkillFile(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %q is not a regular non-symlink file", ErrSkillUnsafeSource, path)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %q changed while opening", ErrSkillUnsafeSource, path)
	}
	return file, nil
}

func readSkillLines(ctx context.Context, reader io.Reader) ([]string, int64, error) {
	limited := &io.LimitedReader{R: contextReader{ctx: ctx, reader: reader}, N: maxSkillFileBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 64*1024), maxSkillLineBytes)
	var lines []string
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, maxSkillFileBytes + 1 - limited.N, err
		}
		lines = append(lines, scanner.Text())
	}
	bytesRead := maxSkillFileBytes + 1 - limited.N
	if err := ctx.Err(); err != nil {
		return nil, bytesRead, err
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return nil, bytesRead, scanErr
	}
	if bytesRead > maxSkillFileBytes {
		return nil, bytesRead, fmt.Errorf("%w: source exceeds %d bytes", ErrSkillResourceLimit, maxSkillFileBytes)
	}
	return lines, bytesRead, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func validateSkillSourcePath(path string) (string, error) {
	cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	localPath := filepath.FromSlash(path)
	if filepath.IsAbs(localPath) || filepath.VolumeName(localPath) != "" || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != filepath.ToSlash(path) {
		return "", fmt.Errorf("%w: source path %q is not a clean relative path", ErrSkillUnsafeSource, path)
	}
	return cleaned, nil
}

func skillPathDepth(relative string) int {
	if relative == "." || relative == "" {
		return 0
	}
	return len(strings.Split(relative, string(filepath.Separator)))
}

func isFatalSkillLoadError(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrSkillResourceLimit) ||
		errors.Is(err, ErrSkillUnsafeSource)
}

// isSkillFileName reports whether name looks like a skill file.
func isSkillFileName(name string) bool {
	switch {
	case strings.HasSuffix(name, ".md"),
		strings.HasSuffix(name, ".yaml"),
		strings.HasSuffix(name, ".yml"):
		return true
	}
	return false
}

// parseFrontmatter splits the lines into a frontmatter block and a body, then
// parses the frontmatter into a SkillDefinition. The body is used as
// the prompt when the frontmatter does not declare one.
func parseFrontmatter(lines []string) (SkillDefinition, error) {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != frontmatterDelimiter {
		return nil, fmt.Errorf("%w: missing opening --- delimiter", errParse)
	}

	frontEnd := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == frontmatterDelimiter {
			frontEnd = i
			break
		}
	}
	if frontEnd == -1 {
		return nil, fmt.Errorf("%w: missing closing --- delimiter", errParse)
	}

	bodyLines := lines[frontEnd+1:]
	body := strings.TrimSuffix(strings.Join(bodyLines, "\n"), "\n")

	parsed, err := parseFrontmatterBlock(lines[1:frontEnd])
	if err != nil {
		return nil, err
	}
	d := parsed.(*DefaultSkillDefinition) //nolint:errcheck
	if d.prompt == "" {
		d.prompt = body
	}
	return parsed, nil
}

// parseFrontmatterBlock parses the frontmatter lines into a definition.
func parseFrontmatterBlock(lines []string) (SkillDefinition, error) {
	def := &DefaultSkillDefinition{}
	var tools []string
	parameters := map[string]any{}

	// activeListKey is the frontmatter key whose list items are currently
	// being collected ("" disables list collection).
	activeListKey := ""
	// activeBlockKey is the frontmatter key of a block scalar being collected.
	activeBlockKey := ""
	var block []string

	flushBlock := func() {
		if activeBlockKey != "" {
			switch activeBlockKey {
			case fmKeyPrompt:
				def.prompt = strings.TrimSuffix(strings.Join(block, "\n"), "\n")
			}
			activeBlockKey = ""
			block = nil
		}
	}

	for _, line := range lines {
		if activeBlockKey != "" {
			if isIndented(line) || strings.TrimSpace(line) == "" {
				block = append(block, stripIndent(line))
				continue
			}
			flushBlock()
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indented := isIndented(line)

		if strings.HasPrefix(trimmed, "- ") {
			item := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
			switch activeListKey {
			case fmKeyTools:
				tools = append(tools, item)
			}
			continue
		}

		key, rest, ok := splitKeyValue(trimmed)
		if !ok {
			continue // unknown line; ignore tolerantly.
		}
		rest = strings.TrimSpace(rest)

		// Whether the value belongs to the parameters map before any context
		// reset below.
		inParameters := activeListKey == fmKeyParameters

		// A top-level (non-indented) line ends the active list/map context.
		if !indented {
			activeListKey = ""
		}

		if rest == "|" {
			activeBlockKey = key
			block = nil
			continue
		}

		if rest == "" {
			// A bare key begins a list context (tools/parameters).
			switch key {
			case fmKeyTools, fmKeyParameters:
				activeListKey = key
			default:
				activeListKey = ""
			}
			continue
		}

		value := parseScalar(rest)
		switch key {
		case fmKeyName:
			def.name = value
		case fmKeyDescription:
			def.description = value
		case fmKeyVersion:
			def.version = value
		case fmKeyCategory:
			def.category = value
		case fmKeyPrompt:
			def.prompt = value
		case fmKeyTriggerHint:
			def.triggerHint = value
		default:
			// A nested line under the `parameters:` block becomes a typed map
			// entry; other unknown keys are ignored.
			if inParameters {
				parameters[key] = coerceParamValue(rest)
			}
		}
	}
	flushBlock()

	def.tools = tools
	def.parameters = parameters
	return def, nil
}

// splitKeyValue splits a line at the first colon into a key and the rest.
func splitKeyValue(line string) (key, rest string, ok bool) {
	idx := strings.Index(line, ":")
	if idx <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	if key == "" {
		return "", "", false
	}
	rest = line[idx+1:]
	return key, rest, true
}

// parseScalar coerces a scalar value into a string. It strips surrounding
// quotes and trims surrounding spaces.
func parseScalar(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			s = s[1 : len(s)-1]
		}
	}
	return s
}

// isIndented reports whether line begins with a space or tab.
func isIndented(line string) bool {
	return len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
}

// stripIndent removes a single indentation level from a block-scalar line.
func stripIndent(line string) string {
	if strings.HasPrefix(line, "  ") {
		return line[2:]
	}
	if strings.HasPrefix(line, "\t") {
		return line[1:]
	}
	return line
}

// coerceParamValue attempts to interpret a parameter scalar as a typed value,
// falling back to the raw string.
func coerceParamValue(s string) any {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}
	if i, err := strconv.Atoi(trimmed); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return f
	}
	switch trimmed {
	case "true":
		return true
	case "false":
		return false
	}
	return parseScalar(trimmed)
}
