package skill

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tools"
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

// SkillLoadReporter is the optional per-call report capability for directory loads.
type SkillLoadReporter interface {
	LoadDirWithReport(ctx context.Context, dirPath string) (SkillLoadResult, error)
}

// SkillSourceLoadReporter is the optional per-call report capability for captured sources.
type SkillSourceLoadReporter interface {
	LoadSourcesWithReport(ctx context.Context, sources []SkillSourceFile) (SkillLoadResult, error)
}

// SkillLoadResult owns the definitions and report produced by one load call.
type SkillLoadResult struct {
	Definitions []*SkillDefinition
	Report      SkillLoadReport
}

// SkillLoadReport summarizes successful definitions and skipped candidate files.
type SkillLoadReport struct {
	Summary     SkillLoadSummary
	Diagnostics []SkillLoadDiagnostic
}

// SkillLoadSummary contains path-free counts for one load call.
type SkillLoadSummary struct {
	Loaded      int
	Skipped     int
	ErrorCounts []SkillLoadErrorCount
}

// SkillLoadErrorClass is a fixed, safe classification for a skipped skill file.
type SkillLoadErrorClass string

// SkillLoadErrorCount counts skipped files by fixed error class.
type SkillLoadErrorCount struct {
	Class SkillLoadErrorClass
	Count int
}

// SkillLoadDiagnostic identifies one skipped file using a bounded relative label.
type SkillLoadDiagnostic struct {
	SourceID string
	Class    SkillLoadErrorClass
}

const (
	// SkillLoadErrorClassFrontmatter identifies frontmatter parse failures.
	SkillLoadErrorClassFrontmatter SkillLoadErrorClass = "frontmatter"
	// SkillLoadErrorClassParameterSchema identifies parameter_schema failures.
	SkillLoadErrorClassParameterSchema SkillLoadErrorClass = "parameter_schema"
	// SkillLoadErrorClassOther identifies other non-fatal file errors.
	SkillLoadErrorClassOther SkillLoadErrorClass = "other"

	maxSkillLoadDiagnosticBytes = 256
)

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
var _ SkillLoadReporter = (*YAMLSkillLoader)(nil)
var _ SkillSourceLoadReporter = (*YAMLSkillLoader)(nil)

// NewYAMLSkillLoader returns a ready-to-use YAMLSkillLoader.
func NewYAMLSkillLoader() SkillLoader { return &YAMLSkillLoader{} }

// frontmatterDelimiter is the line that opens and closes the frontmatter block.
const frontmatterDelimiter = "---"

// Frontmatter field keys. Referenced through constants (rather than inline
// string literals) in switches so the rule does not mistake a case
// label for hardcoded command routing — some of these ("version", "prompt")
// collide with known command names.
const (
	fmKeyName            = "name"
	fmKeyDescription     = "description"
	fmKeyVersion         = "version"
	fmKeyCategory        = "category"
	fmKeyPrompt          = "prompt"
	fmKeyTools           = "tools"
	fmKeyParameters      = "parameters"
	fmKeyParameterSchema = "parameter_schema"
	fmKeyTriggerHint     = "trigger_hint"
)

// errParse is returned when a skill file cannot be parsed into a definition.
var errParse = errors.New("skill: parse error")

// ErrSkillParameterSchemaInvalid classifies unsupported or malformed
// parameter_schema declarations while preserving any prepared-schema cause.
var ErrSkillParameterSchemaInvalid = errors.New("skill: invalid parameter schema")

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
	absPath := path
	if p, err := filepath.Abs(path); err == nil {
		absPath = p
	}
	return l.loadFileWithOpener(ctx, path, absPath, func() (*os.File, error) {
		return openRegularSkillFile(absPath)
	})
}

func (l *YAMLSkillLoader) loadFileWithOpener(ctx context.Context, path, absPath string, openFile func() (*os.File, error)) (*SkillDefinition, int64, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "skill.load", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	span.SetAttributes(tracing.Attribute{Key: "source_path", Value: path})

	if err := spanCtx.Err(); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, 0, err
	}

	file, err := openFile()
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
	result, err := l.LoadDirWithReport(ctx, dirPath)
	if err != nil {
		return nil, err
	}
	return result.Definitions, nil
}

// LoadDirWithReport scans a directory and returns a deterministic report for
// skipped candidates.
func (l *YAMLSkillLoader) LoadDirWithReport(ctx context.Context, dirPath string) (SkillLoadResult, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "skill.load_dir", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()
	fail := func(err error) (SkillLoadResult, error) {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return SkillLoadResult{}, err
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
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return fail(fmt.Errorf("skill: scan dir %s: %w", dirPath, err))
	}

	var defs []*SkillDefinition
	var candidateFiles int
	var totalBytes int64
	skipped := make(map[SkillLoadErrorClass]int)
	diagnostics := make([]SkillLoadDiagnostic, 0)
	err = walkSkillDirectory(spanCtx, rootHandle, root, func(path, relative string, d os.FileInfo, openCandidate func() (*os.File, error)) error {
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
		if d.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: candidate %q is a symlink", ErrSkillUnsafeSource, relative)
		}
		def, bytesRead, err := l.loadFileWithOpener(spanCtx, path, path, openCandidate)
		totalBytes += bytesRead
		if totalBytes > maxSkillDirectoryBytes {
			return fmt.Errorf("%w: directory source bytes exceed %d", ErrSkillResourceLimit, maxSkillDirectoryBytes)
		}
		if err != nil {
			if isFatalSkillLoadError(err) {
				return err
			}
			// Skip unparsable files rather than failing the whole directory.
			class := classifySkillLoadError(err)
			recordSkillLoadSkip(skipped, &diagnostics, relative, class)
			logger.Warn("skill.load_dir.skipped", "path", path, "err", err)
			return nil
		}
		defs = append(defs, def)
		return nil
	})
	if closeErr := rootHandle.Close(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	if err != nil {
		return fail(fmt.Errorf("skill: scan dir %s: %w", dirPath, err))
	}

	span.SetAttributes(tracing.Attribute{Key: "count", Value: len(defs)})
	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("skill.load_dir", "dir", dirPath, "count", len(defs))
	return newSkillLoadResult(defs, skipped, diagnostics), nil
}

func walkSkillDirectory(ctx context.Context, root *os.Root, rootPath string, visit func(path, relative string, entry os.FileInfo, openCandidate func() (*os.File, error)) error) error {
	entriesRead := 0
	var walk func(relative string, expected os.FileInfo) error
	walk = func(relative string, expected os.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, err := openSkillDirectory(root, relative, expected)
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
			childInfo, err := lstatSkillPath(root, childRelative)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) && !child.IsDir() {
					continue
				}
				return err
			}
			if childInfo.IsDir() && skillPathDepth(childRelative) > maxSkillDirectoryDepth {
				return fmt.Errorf("%w: directory depth exceeds %d", ErrSkillResourceLimit, maxSkillDirectoryDepth)
			}
			var openCandidate func() (*os.File, error)
			if !childInfo.IsDir() && isSkillFileName(child.Name()) {
				openCandidate = func() (*os.File, error) {
					return openRegularSkillFileAt(root, childRelative, childInfo)
				}
			}
			childPath := filepath.Join(rootPath, childRelative)
			if err := visit(childPath, childRelative, childInfo, openCandidate); err != nil {
				return err
			}
			if childInfo.IsDir() {
				if err := walk(childRelative, childInfo); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(".", nil)
}

func openSkillDirectory(root *os.Root, relative string, expected os.FileInfo) (*os.File, error) {
	before, err := lstatSkillPath(root, relative)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() || (expected != nil && !os.SameFile(expected, before)) {
		return nil, fmt.Errorf("%w: %q is not a regular directory", ErrSkillUnsafeSource, relative)
	}
	dir, err := root.Open(relative)
	if err != nil {
		return nil, fmt.Errorf("%w: open directory %q: %w", ErrSkillUnsafeSource, relative, err)
	}
	after, err := dir.Stat()
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	afterPath, err := lstatSkillPath(root, relative)
	if err != nil {
		closeErr := dir.Close()
		return nil, errors.Join(err, closeErr)
	}
	if !after.IsDir() || afterPath.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) || !os.SameFile(before, afterPath) {
		_ = dir.Close()
		return nil, fmt.Errorf("%w: %q changed while opening", ErrSkillUnsafeSource, relative)
	}
	return dir, nil
}

func lstatSkillPath(root *os.Root, relative string) (os.FileInfo, error) {
	if relative == "." {
		return root.Stat(".")
	}
	cleaned := filepath.Clean(relative)
	if filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" || cleaned != relative || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%w: path %q is not root-relative", ErrSkillUnsafeSource, relative)
	}

	parts := strings.Split(cleaned, string(filepath.Separator))
	prefix := ""
	var info os.FileInfo
	var err error
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("%w: path %q is not root-relative", ErrSkillUnsafeSource, relative)
		}
		if prefix == "" {
			prefix = part
		} else {
			prefix = filepath.Join(prefix, part)
		}
		info, err = root.Lstat(prefix)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: inspect %q: %w", ErrSkillUnsafeSource, relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if i == len(parts)-1 {
				return info, nil
			}
			return nil, fmt.Errorf("%w: path %q has a symlink ancestor", ErrSkillUnsafeSource, relative)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, fmt.Errorf("%w: path %q has a non-directory ancestor", ErrSkillUnsafeSource, relative)
		}
	}
	return info, nil
}

func openRegularSkillFileAt(root *os.Root, relative string, expected os.FileInfo) (*os.File, error) {
	before, err := lstatSkillPath(root, relative)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || (expected != nil && !os.SameFile(expected, before)) {
		return nil, fmt.Errorf("%w: %q is not the expected regular file", ErrSkillUnsafeSource, relative)
	}

	file, err := root.Open(relative)
	if err != nil {
		if current, statErr := lstatSkillPath(root, relative); errors.Is(statErr, ErrSkillUnsafeSource) {
			return nil, statErr
		} else if statErr == nil && current.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%w: %q changed to a symlink while opening", ErrSkillUnsafeSource, relative)
		}
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	afterPath, err := lstatSkillPath(root, relative)
	if err != nil {
		closeErr := file.Close()
		return nil, errors.Join(err, closeErr)
	}
	if !after.Mode().IsRegular() || afterPath.Mode()&os.ModeSymlink != 0 || !os.SameFile(before, after) || !os.SameFile(before, afterPath) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %q changed while opening", ErrSkillUnsafeSource, relative)
	}
	return file, nil
}

// LoadSources parses a previously captured skill source set. Source files are
// consumed in the order supplied and are never reopened from disk. Malformed
// skill files follow LoadDir's skip-with-warning behavior.
func (l *YAMLSkillLoader) LoadSources(ctx context.Context, sources []SkillSourceFile) ([]*SkillDefinition, error) {
	result, err := l.LoadSourcesWithReport(ctx, sources)
	if err != nil {
		return nil, err
	}
	return result.Definitions, nil
}

// LoadSourcesWithReport parses captured sources without reopening their paths
// and reports skipped candidates.
func (l *YAMLSkillLoader) LoadSourcesWithReport(ctx context.Context, sources []SkillSourceFile) (SkillLoadResult, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "skill.load_sources", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	if err := spanCtx.Err(); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return SkillLoadResult{}, err
	}
	if len(sources) > maxSkillCandidateFiles {
		err := fmt.Errorf("%w: source set contains more than %d skill files", ErrSkillResourceLimit, maxSkillCandidateFiles)
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return SkillLoadResult{}, err
	}

	normalizedSources := make([]SkillSourceFile, len(sources))
	seenPaths := make(map[string]struct{}, len(sources))
	var totalBytes int64
	for i, source := range sources {
		if err := spanCtx.Err(); err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return SkillLoadResult{}, err
		}
		relativePath, err := validateSkillSourcePath(source.RelativePath)
		if err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return SkillLoadResult{}, err
		}
		if !isSkillFileName(filepath.Base(filepath.FromSlash(relativePath))) {
			err := fmt.Errorf("%w: %q is not a skill file candidate", ErrSkillUnsafeSource, relativePath)
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return SkillLoadResult{}, err
		}
		if _, duplicate := seenPaths[relativePath]; duplicate {
			err := fmt.Errorf("%w: duplicate skill source path %q", ErrSkillUnsafeSource, relativePath)
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return SkillLoadResult{}, err
		}
		seenPaths[relativePath] = struct{}{}
		if int64(len(source.Content)) > maxSkillFileBytes {
			err := fmt.Errorf("%w: source %q exceeds %d bytes", ErrSkillResourceLimit, relativePath, maxSkillFileBytes)
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return SkillLoadResult{}, err
		}
		totalBytes += int64(len(source.Content))
		if totalBytes > maxSkillDirectoryBytes {
			err := fmt.Errorf("%w: source set exceeds %d bytes", ErrSkillResourceLimit, maxSkillDirectoryBytes)
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return SkillLoadResult{}, err
		}
		normalizedSources[i] = SkillSourceFile{RelativePath: relativePath, Content: source.Content}
	}

	defs := make([]*SkillDefinition, 0, len(normalizedSources))
	skipped := make(map[SkillLoadErrorClass]int)
	diagnostics := make([]SkillLoadDiagnostic, 0)
	for _, source := range normalizedSources {
		if err := spanCtx.Err(); err != nil {
			span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
			span.SetStatus(tracing.SpanStatusError, err.Error())
			return SkillLoadResult{}, err
		}
		def, err := l.loadSource(spanCtx, source)
		if err != nil {
			if ctxErr := spanCtx.Err(); ctxErr != nil {
				span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
				span.SetStatus(tracing.SpanStatusError, ctxErr.Error())
				return SkillLoadResult{}, ctxErr
			}
			if isFatalSkillLoadError(err) {
				span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
				span.SetStatus(tracing.SpanStatusError, err.Error())
				return SkillLoadResult{}, err
			}
			class := classifySkillLoadError(err)
			recordSkillLoadSkip(skipped, &diagnostics, source.RelativePath, class)
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
	return newSkillLoadResult(defs, skipped, diagnostics), nil
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

func classifySkillLoadError(err error) SkillLoadErrorClass {
	if errors.Is(err, ErrSkillParameterSchemaInvalid) {
		return SkillLoadErrorClassParameterSchema
	}
	if errors.Is(err, errParse) {
		return SkillLoadErrorClassFrontmatter
	}
	return SkillLoadErrorClassOther
}

func recordSkillLoadSkip(counts map[SkillLoadErrorClass]int, diagnostics *[]SkillLoadDiagnostic, relative string, class SkillLoadErrorClass) {
	counts[class]++
	if len(*diagnostics) >= maxSkillCandidateFiles {
		return
	}
	sourceID, ok := skillLoadDiagnosticSourceID(relative)
	if !ok {
		sourceID = `skill:"<invalid-relative-source>"`
	}
	*diagnostics = append(*diagnostics, SkillLoadDiagnostic{SourceID: sourceID, Class: class})
}

func newSkillLoadResult(definitions []*SkillDefinition, counts map[SkillLoadErrorClass]int, diagnostics []SkillLoadDiagnostic) SkillLoadResult {
	errorCounts := make([]SkillLoadErrorCount, 0, len(counts))
	skipped := 0
	for class, count := range counts {
		errorCounts = append(errorCounts, SkillLoadErrorCount{Class: class, Count: count})
		skipped += count
	}
	sort.Slice(errorCounts, func(i, j int) bool {
		return errorCounts[i].Class < errorCounts[j].Class
	})
	sort.Slice(diagnostics, func(i, j int) bool {
		return diagnostics[i].SourceID < diagnostics[j].SourceID
	})
	return SkillLoadResult{
		Definitions: definitions,
		Report: SkillLoadReport{
			Summary: SkillLoadSummary{
				Loaded:      len(definitions),
				Skipped:     skipped,
				ErrorCounts: errorCounts,
			},
			Diagnostics: diagnostics,
		},
	}
}

func skillLoadDiagnosticSourceID(relative string) (string, bool) {
	if relative == "" {
		return "", false
	}
	localPath := filepath.FromSlash(relative)
	if filepath.IsAbs(localPath) || filepath.VolumeName(localPath) != "" {
		return "", false
	}
	cleaned := filepath.Clean(localPath)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned != localPath {
		return "", false
	}
	normalized := filepath.ToSlash(cleaned)
	for _, part := range strings.Split(normalized, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}

	const prefix = "skill:"
	quoted := strconv.QuoteToASCII(normalized)
	sourceID := prefix + quoted
	if len(sourceID) <= maxSkillLoadDiagnosticBytes {
		return sourceID, true
	}

	digest := sha256.Sum256([]byte(normalized))
	digestText := hex.EncodeToString(digest[:])
	quotedPath := strings.Trim(quoted, `"`)
	marker := "~" + digestText + "~"
	fragmentBudget := maxSkillLoadDiagnosticBytes - len(prefix) - len(marker)
	headBytes := fragmentBudget / 2
	tailBytes := fragmentBudget - headBytes
	return prefix + quotedPath[:headBytes] + marker + quotedPath[len(quotedPath)-tailBytes:], true
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
		if hasTopLevelParameterSchema(lines[1:]) {
			return nil, fmt.Errorf("%w: missing closing frontmatter delimiter", ErrSkillParameterSchemaInvalid)
		}
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
	parameterSchemaSeen := false

	// activeListKey is the frontmatter key whose list items are currently
	// being collected ("" disables list collection).
	activeListKey := ""
	// activeBlockKey is the frontmatter key of a block scalar being collected.
	activeBlockKey := ""
	var block []string

	flushBlock := func() error {
		if activeBlockKey != "" {
			switch activeBlockKey {
			case fmKeyPrompt:
				def.prompt = strings.TrimSuffix(strings.Join(block, "\n"), "\n")
			case fmKeyParameterSchema:
				schema, err := decodeParameterSchemaJSON(strings.Join(block, "\n"))
				if err != nil {
					return err
				}
				def.parameterSchema = schema
			}
			activeBlockKey = ""
			block = nil
		}
		return nil
	}

	for _, line := range lines {
		if activeBlockKey != "" {
			if isIndented(line) || strings.TrimSpace(line) == "" {
				block = append(block, stripIndent(line))
				continue
			}
			if err := flushBlock(); err != nil {
				return nil, err
			}
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
		if key == fmKeyParameterSchema && !indented {
			if parameterSchemaSeen {
				return nil, fmt.Errorf("%w: duplicate top-level parameter_schema", ErrSkillParameterSchemaInvalid)
			}
			parameterSchemaSeen = true
			if rest != "|" {
				return nil, fmt.Errorf("%w: parameter_schema must use a literal | JSON block", ErrSkillParameterSchemaInvalid)
			}
			activeBlockKey = fmKeyParameterSchema
			block = nil
			continue
		}

		// Whether the value belongs to the parameters map before any context
		// reset below.
		inParameters := activeListKey == fmKeyParameters

		// A top-level (non-indented) line ends the active list/map context.
		if !indented {
			activeListKey = ""
		}

		if rest == "|" {
			if key == fmKeyParameterSchema {
				activeBlockKey = "ignored_parameter_schema"
			} else {
				activeBlockKey = key
			}
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
	if err := flushBlock(); err != nil {
		return nil, err
	}

	def.tools = tools
	def.parameters = parameters
	return def, nil
}

func decodeParameterSchemaJSON(text string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var schema any
	if err := decoder.Decode(&schema); err != nil {
		return nil, fmt.Errorf("%w: decode parameter_schema JSON: %w", ErrSkillParameterSchemaInvalid, err)
	}
	if _, ok := schema.(map[string]any); !ok {
		return nil, fmt.Errorf("%w: parameter_schema JSON root must be an object", ErrSkillParameterSchemaInvalid)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%w: parameter_schema contains more than one JSON value", ErrSkillParameterSchemaInvalid)
		}
		return nil, fmt.Errorf("%w: trailing parameter_schema JSON: %w", ErrSkillParameterSchemaInvalid, err)
	}
	if err := tools.ValidateParameterSchema(schema); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSkillParameterSchemaInvalid, err)
	}
	return schema, nil
}

func hasTopLevelParameterSchema(lines []string) bool {
	for _, line := range lines {
		if isIndented(line) {
			continue
		}
		key, _, ok := splitKeyValue(strings.TrimSpace(line))
		if ok && key == fmKeyParameterSchema {
			return true
		}
	}
	return false
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
