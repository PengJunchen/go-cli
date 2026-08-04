package skill

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// YAMLSkillLoader implements SkillLoader for the YAML-frontmatter format.
type YAMLSkillLoader struct{}

// Compile-time assertion that YAMLSkillLoader satisfies SkillLoader.
var _ SkillLoader = (*YAMLSkillLoader)(nil)

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

// Load reads the skill file at path, parses its frontmatter and returns a
// populated SkillDefinition. It emits a `skill.load` span.
func (l *YAMLSkillLoader) Load(ctx context.Context, path string) (*SkillDefinition, error) {
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
		return nil, err
	}

	file, err := os.Open(absPath)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("skill.load.open_failed", "path", absPath, "err", err)
		return nil, fmt.Errorf("skill: open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }() //nolint:errcheck // best-effort close.

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024) // 4 MiB line buffer.
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if scanErr := scanner.Err(); scanErr != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, scanErr.Error())
		logger.Error("skill.load.scan_failed", "path", absPath, "err", scanErr)
		return nil, fmt.Errorf("skill: read %s: %w", path, scanErr)
	}

	def, err := parseFrontmatter(lines)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		logger.Error("skill.load.parse_failed", "path", absPath, "err", err)
		return nil, fmt.Errorf("skill: load %s: %w", path, err)
	}

	span.SetAttributes(
		tracing.Attribute{Key: "skill_name", Value: def.Name()},
		tracing.Attribute{Key: "success", Value: true},
	)
	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("skill.load", "path", absPath, "name", def.Name())

	return &def, nil
}

// LoadDir scans dirPath recursively for skill files and loads each one. Files
// ending in ".md", ".skill.md" or ".yaml" (or ".yml") are considered candidate
// skill files; any that fail to parse are skipped with a warning.
func (l *YAMLSkillLoader) LoadDir(ctx context.Context, dirPath string) ([]*SkillDefinition, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "skill.load_dir", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()

	if err := spanCtx.Err(); err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		return nil, err
	}

	var defs []*SkillDefinition
	err := filepath.WalkDir(dirPath, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !isSkillFileName(d.Name()) {
			return nil
		}
		def, err := l.Load(spanCtx, path)
		if err != nil {
			// Skip unparseable files rather than failing the whole directory.
			logger.Warn("skill.load_dir.skipped", "path", path, "err", err)
			return nil
		}
		defs = append(defs, def)
		return nil
	})
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, fmt.Errorf("skill: scan dir %s: %w", dirPath, err)
	}

	span.SetAttributes(tracing.Attribute{Key: "count", Value: len(defs)})
	span.SetStatus(tracing.SpanStatusOK, "")
	logger.Info("skill.load_dir", "dir", dirPath, "count", len(defs))
	return defs, nil
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
