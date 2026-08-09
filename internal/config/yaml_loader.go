package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// ConfigFormat enumerates the supported configuration file encodings.
type ConfigFormat int

const (
	// ConfigFormatJSON is the JSON configuration format (the default and the
	// only format supported before Phase 4 YAML support).
	ConfigFormatJSON ConfigFormat = iota
	// ConfigFormatYAML is the YAML configuration format.
	ConfigFormatYAML
	// ConfigFormatAuto detects the format from the file path extension when
	// the caller does not want to commit to a specific encoding.
	ConfigFormatAuto
)

// String returns the lowercase name of the format ("json", "yaml", "auto").
func (f ConfigFormat) String() string {
	switch f {
	case ConfigFormatJSON:
		return "json"
	case ConfigFormatYAML:
		return "yaml"
	case ConfigFormatAuto:
		return "auto"
	default:
		return "unknown"
	}
}

// DetectConfigFormat infers the configuration format from path's file
// extension. ".json" selects JSON, ".yaml"/".yml" select YAML. Any other or
// absent extension returns a clear error; the caller typically falls back to
// JSON for backward compatibility (see Loader.Load).
func DetectConfigFormat(path string) (ConfigFormat, error) {
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".json":
		return ConfigFormatJSON, nil
	case ".yaml", ".yml":
		return ConfigFormatYAML, nil
	default:
		return ConfigFormatAuto, fmt.Errorf(
			"config: unsupported or missing extension %q for %s (want .json, .yaml or .yml)",
			ext, path)
	}
}

// UnmarshalConfig parses data according to format and stores the result in
// target (an addressable Config or nested struct). JSON is decoded with
// encoding/json; YAML is decoded with this package's hand-written, zero
// external dependency YAML-subset parser. ConfigFormatAuto resolves to JSON.
func UnmarshalConfig(data []byte, format ConfigFormat, target any) error {
	if format == ConfigFormatAuto {
		format = ConfigFormatJSON
	}
	switch format {
	case ConfigFormatJSON:
		return json.Unmarshal(data, target)
	case ConfigFormatYAML:
		tree, err := parseYAMLTree(data)
		if err != nil {
			return err
		}
		m, ok := tree.(map[string]any)
		if !ok {
			return fmt.Errorf("yaml: top level must be a mapping")
		}
		return assignFromMap(target, m)
	default:
		return fmt.Errorf("config: unsupported format %s", format)
	}
}

// YAMLConfigLoader loads a Config from a configuration file, using its
// extension to choose between YAML and JSON. It is a small, self-contained
// loader for callers that want format-aware file loading without the full
// layered Loader.
type YAMLConfigLoader struct{}

// Compile-time assertion. There is no ConfigProvider interface in this
// package, so a plain assignability check is used.
var _ = (*YAMLConfigLoader)(nil)

// NewYAMLConfigLoader returns a ready-to-use YAMLConfigLoader.
func NewYAMLConfigLoader() *YAMLConfigLoader { return &YAMLConfigLoader{} }

// Load reads the file at path, detects its format, parses it into a Config
// and validates nothing (validation is the caller's concern). It emits a
// config.load.yaml span and logs the detected format at debug level.
func (l *YAMLConfigLoader) Load(ctx context.Context, path string) (*Config, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "config.load.yaml", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	defer span.End()
	span.SetAttributes(tracing.Attribute{Key: "path", Value: path})

	format, err := DetectConfigFormat(path)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, fmt.Errorf("config: detect format %s: %w", path, err)
	}
	logger.DebugContext(spanCtx, "config.load.yaml.format",
		"op", "config.load.yaml",
		"path", path,
		"format", format.String(),
	)

	data, err := os.ReadFile(path)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	if err := UnmarshalConfig(data, format, &cfg); err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return nil, fmt.Errorf("config: parse %s as %s: %w", path, format, err)
	}

	span.SetAttributes(tracing.Attribute{Key: "config_keys", Value: countKeys(&cfg)})
	span.SetStatus(tracing.SpanStatusOK, "")
	return &cfg, nil
}

// ---------------------------------------------------------------------------
// Hand-written YAML-subset parser.
//
// It is deliberately minimal and covers the configuration schema shape: a
// top-level mapping whose values are scalar fields, one level of nested
// mappings (provider/model/tracing/...), and lists of scalar strings (tools
// builtin/registry). Unquoted scalars are coerced to bool/int/float/string so
// typed Config fields populate correctly. It also supports tab-indented
// lines (tab = 4-space tab-stops), flow maps ({key: value}), and block
// scalars (| literal and > folded, with optional - strip chomping). It does
// not implement anchors, aliases, or multi-document streams.
// ---------------------------------------------------------------------------

// yamlLine is a single significant line from a YAML document.
type yamlLine struct {
	indent   int
	text     string
	isBlank  bool
	trimmed  string
	listItem bool
}

// yamlParser walks the normalized lines of a YAML document.
type yamlParser struct {
	lines []yamlLine
	pos   int
}

// buildYAMLLines strips comments and splits data into indentation-tagged lines.
func buildYAMLLines(data []byte) []yamlLine {
	var out []yamlLine
	for _, raw := range strings.Split(string(data), "\n") {
		raw = strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(stripYAMLComment(raw))
		if trimmed == "" {
			out = append(out, yamlLine{isBlank: true})
			continue
		}
		out = append(out, yamlLine{
			indent:   indentWidth(raw),
			text:     trimmed,
			trimmed:  trimmed,
			listItem: strings.HasPrefix(trimmed, "-"),
		})
	}
	return out
}

// peek returns the next significant (non-blank) line, consuming any blank
// lines it skips, and an indicator that no more lines remain.
func (p *yamlParser) peek() (yamlLine, bool) {
	for p.pos < len(p.lines) {
		ln := p.lines[p.pos]
		if ln.isBlank {
			p.pos++
			continue
		}
		return ln, true
	}
	return yamlLine{}, false
}

// parseYAMLTree parses a full YAML document into an any tree whose leaves are
// native Go scalars, maps and slices.
func parseYAMLTree(data []byte) (any, error) {
	p := &yamlParser{lines: buildYAMLLines(data)}
	ln, ok := p.peek()
	if !ok {
		return map[string]any{}, nil
	}
	if ln.listItem {
		return p.parseList(ln.indent)
	}
	return p.parseMap(ln.indent)
}

// parseMap consumes consecutive sibling `key: value` lines all at the given indent.
func (p *yamlParser) parseMap(indent int) (any, error) {
	m := map[string]any{}
	for {
		ln, ok := p.peek()
		if !ok || ln.indent != indent || ln.listItem {
			break
		}
		p.pos++
		key, inline, ok := splitKeyValue(ln.text)
		if !ok || key == "" {
			return nil, fmt.Errorf("yaml: invalid mapping line %q", ln.text)
		}
		v, err := p.valueFor(key, inline, indent)
		if err != nil {
			return nil, err
		}
		m[key] = v
	}
	return m, nil
}

// parseList consumes consecutive sibling `- item` lines all at the given indent.
// Each list item may be a single scalar or a multi-line mapping that starts
// with a key-value on the "- " line and continues with deeper-indented
// key-value lines belonging to the same item.
func (p *yamlParser) parseList(indent int) (any, error) {
	list := []any{}
	for {
		ln, ok := p.peek()
		if !ok || ln.indent != indent || !ln.listItem {
			break
		}
		p.pos++
		text := strings.TrimSpace(strings.TrimPrefix(ln.text, "-"))
		if text == "" {
			// Empty list item: may be followed by a deeper block.
			child, childOK := p.peek()
			if childOK && child.indent > indent {
				v, err := p.parseMapOrList(child)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			} else {
				list = append(list, map[string]any{})
			}
			continue
		}
		if key, inline, ok := splitKeyValue(text); ok && key != "" {
			// First key-value pair on the "- " line. Collect deeper
			// key-value lines into the same mapping.
			sub := map[string]any{}
			v, err := p.valueFor(key, inline, indent)
			if err != nil {
				return nil, err
			}
			sub[key] = v
			// Collect additional key-value lines that are deeper than
			// the "- " line but not new list items at the same indent.
			for {
				child, childOK := p.peek()
				if !childOK || child.indent <= indent || child.listItem {
					break
				}
				// The child line belongs to this list item's mapping.
				p.pos++
				ck, cinline, cok := splitKeyValue(child.text)
				if !cok || ck == "" {
					return nil, fmt.Errorf("yaml: invalid mapping line %q", child.text)
				}
				cv, err := p.valueFor(ck, cinline, child.indent)
				if err != nil {
					return nil, err
				}
				sub[ck] = cv
			}
			list = append(list, sub)
			continue
		}
		list = append(list, parseScalar(text))
	}
	return list, nil
}

// parseMapOrList dispatches to parseMap or parseList depending on whether the
// next line is a list item.
func (p *yamlParser) parseMapOrList(ln yamlLine) (any, error) {
	if ln.listItem {
		return p.parseList(ln.indent)
	}
	return p.parseMap(ln.indent)
}

// valueFor parses the value that follows a `key:` at the given indent. An
// inline scalar returns immediately; an empty value collects the deeper
// (map or list) block that follows. Flow maps ({...}) and block scalars
// (| and >) are detected from the inline value.
func (p *yamlParser) valueFor(key, inline string, indent int) (any, error) {
	v := strings.TrimSpace(inline)
	if v != "" {
		// Flow map: {key: value, ...}
		if strings.HasPrefix(v, "{") {
			return parseFlowMap(v)
		}
		// Block scalar indicators: | (literal) or > (folded), with optional
		// chomping suffix (- strip trailing newline, + keep, none = clip).
		if v == "|" || v == ">" || strings.HasPrefix(v, "|-") || strings.HasPrefix(v, ">-") {
			strip := strings.HasSuffix(v, "-")
			return p.parseBlockScalar(indent, v[0], strip)
		}
		return parseScalar(v), nil
	}
	child, ok := p.peek()
	if !ok {
		return map[string]any{}, nil
	}
	if child.indent > indent {
		if child.listItem {
			return p.parseList(child.indent)
		}
		return p.parseMap(child.indent)
	}
	// No deeper block: an empty value, tolerantly an empty mapping.
	return map[string]any{}, nil
}

// parseBlockScalar collects subsequent lines that are deeper than the given
// indent and joins them as a block scalar. mode '|' preserves newlines;
// mode '>' folds single newlines into spaces (paragraph breaks from blank
// lines remain). When strip is true (the |- or >- indicator), the trailing
// newline is omitted. The method advances p.pos past the consumed block lines.
func (p *yamlParser) parseBlockScalar(parentIndent int, mode byte, strip bool) (string, error) {
	var lines []string
	blockIndent := -1
	for {
		if p.pos >= len(p.lines) {
			break
		}
		ln := p.lines[p.pos]
		if ln.isBlank {
			lines = append(lines, "")
			p.pos++
			continue
		}
		if blockIndent < 0 {
			blockIndent = ln.indent
			if blockIndent <= parentIndent {
				break // no deeper content; block scalar is empty
			}
		}
		if ln.indent < blockIndent {
			break
		}
		// Dedent the line by blockIndent (for content alignment)
		content := ln.trimmed
		if ln.indent > blockIndent {
			content = strings.Repeat(" ", ln.indent-blockIndent) + content
		}
		lines = append(lines, content)
		p.pos++
	}

	// Remove trailing empty lines
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	trailingNL := "\n"
	if strip {
		trailingNL = ""
	}

	if len(lines) == 0 {
		return "", nil // empty block scalar yields empty string
	}

	if mode == '|' {
		return strings.Join(lines, "\n") + trailingNL, nil
	}
	// Folded mode '>': single newlines become spaces, blank lines become newlines
	var result strings.Builder
	for i, line := range lines {
		if i > 0 {
			if lines[i-1] == "" || line == "" {
				result.WriteString("\n")
			} else {
				result.WriteString(" ")
			}
		}
		result.WriteString(line)
	}
	return result.String() + trailingNL, nil
}

// splitKeyValue splits a line at the first colon into a key and the remainder.
func splitKeyValue(line string) (key, rest string, ok bool) {
	idx := strings.Index(line, ":")
	if idx <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	if key == "" {
		return "", "", false
	}
	return key, strings.TrimSpace(line[idx+1:]), true
}

// indentWidth returns the indentation width of line, counting spaces and tabs.
// A tab advances to the next multiple of 4 (equivalent to 4-space tab stops).
func indentWidth(line string) int {
	n := 0
	hasTab := false
	hasSpace := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			n++
			hasSpace = true
		case '\t':
			n = ((n + 4) / 4) * 4
			hasTab = true
		default:
			if hasTab && hasSpace {
				slog.Warn("yaml_mixed_indent", "line", line, "reason", "mixed tabs and spaces")
			}
			return n
		}
	}
	return n
}

// stripYAMLComment removes a YAML comment (a '#' that starts a word) unless it
// sits inside a single- or double-quoted region.
func stripYAMLComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				if quote == '"' && i > 0 && s[i-1] == '\\' {
					continue
				}
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '#':
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return strings.TrimRight(s[:i], " \t")
			}
		}
	}
	return s
}

// parseScalar coerces a YAML scalar to a native Go value: quoted strings stay
// strings, and unquoted true/false/null, integers and floats are converted.
func parseScalar(s string) any {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	switch s {
	case "true":
		return true
	case "false":
		return false
	case "null", "~":
		return nil
	}
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// parseFlowMap parses a YAML flow map like {key: value, key2: value2} into
// a map[string]any. Supports quoted keys/values and nested flow maps.
func parseFlowMap(s string) (map[string]any, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("yaml: invalid flow map: %q", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return map[string]any{}, nil
	}
	m := map[string]any{}
	parts := splitFlowItems(inner)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, val, ok := splitKeyValue(part)
		if !ok || key == "" {
			return nil, fmt.Errorf("yaml: invalid flow map entry: %q", part)
		}
		// Strip quotes from key if present
		if len(key) >= 2 && (key[0] == '"' || key[0] == '\'') && key[len(key)-1] == key[0] {
			key = key[1 : len(key)-1]
		}
		val = strings.TrimSpace(val)
		if strings.HasPrefix(val, "{") {
			nested, err := parseFlowMap(val)
			if err != nil {
				return nil, err
			}
			m[key] = nested
		} else {
			m[key] = parseScalar(val)
		}
	}
	return m, nil
}

// splitFlowItems splits a flow collection body by top-level commas,
// respecting nested braces and quoted strings.
func splitFlowItems(s string) []string {
	var parts []string
	depth := 0
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// assignFromMap fills an addressable struct target from a parsed YAML mapping,
// matching fields by their json tag name. It returns an error describing the
// first field that cannot be populated.
func assignFromMap(target any, m map[string]any) error {
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("yaml: target must be a non-nil pointer")
	}
	return assignValue(rv.Elem(), m)
}

// assignValue assigns src into the settable destination dst, recursing into
// structs, slices and pointers.
func assignValue(dst reflect.Value, src any) error {
	if !dst.IsValid() || !dst.CanSet() {
		return nil
	}
	if m, ok := src.(map[string]any); ok && dst.Kind() == reflect.Struct {
		for i := 0; i < dst.NumField(); i++ {
			f := dst.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = strings.ToLower(f.Name)
			}
			v, exists := m[name]
			if !exists {
				continue
			}
			if err := assignValue(dst.Field(i), v); err != nil {
				return fmt.Errorf("yaml: key %q: %w", name, err)
			}
		}
		return nil
	}
	if dst.Kind() == reflect.Pointer {
		if src == nil {
			dst.Set(reflect.Zero(dst.Type()))
			return nil
		}
		np := reflect.New(dst.Type().Elem())
		if err := assignValue(np.Elem(), src); err != nil {
			return err
		}
		dst.Set(np)
		return nil
	}
	if dst.Kind() == reflect.Slice {
		switch v := src.(type) {
		case []any:
			out := reflect.MakeSlice(dst.Type(), len(v), len(v))
			for i, item := range v {
				if err := assignValue(out.Index(i), item); err != nil {
					return err
				}
			}
			dst.Set(out)
			return nil
		case map[string]any:
			if len(v) > 0 {
				return fmt.Errorf("cannot assign a mapping to a slice")
			}
			dst.Set(reflect.MakeSlice(dst.Type(), 0, 0))
			return nil
		case nil:
			dst.Set(reflect.Zero(dst.Type()))
			return nil
		default:
			return fmt.Errorf("cannot assign %T to a slice", src)
		}
	}
	return assignScalar(dst, src)
}

// assignScalar coerces a scalar value into dst according to its kind.
func assignScalar(dst reflect.Value, src any) error {
	switch dst.Kind() {
	case reflect.String:
		v, ok := scalarToString(src)
		if !ok {
			return fmt.Errorf("cannot assign %T to string", src)
		}
		dst.SetString(v)
	case reflect.Bool:
		v, ok := scalarToBool(src)
		if !ok {
			return fmt.Errorf("cannot assign %T to bool", src)
		}
		dst.SetBool(v)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v, ok := scalarToInt64(src)
		if !ok {
			return fmt.Errorf("cannot assign %T to integer", src)
		}
		dst.SetInt(v)
	case reflect.Float32, reflect.Float64:
		v, ok := scalarToFloat64(src)
		if !ok {
			return fmt.Errorf("cannot assign %T to float", src)
		}
		dst.SetFloat(v)
	default:
		return fmt.Errorf("unsupported destination kind %v", dst.Kind())
	}
	return nil
}

func scalarToString(src any) (string, bool) {
	switch v := src.(type) {
	case string:
		return v, true
	case int64:
		return strconv.FormatInt(v, 10), true
	case int:
		return strconv.Itoa(v), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(v), true
	default:
		return "", false
	}
}

func scalarToBool(src any) (bool, bool) {
	switch v := src.(type) {
	case bool:
		return v, true
	case int64:
		return v != 0, true
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		return b, err == nil
	default:
		return false, false
	}
}

func scalarToInt64(src any) (int64, bool) {
	switch v := src.(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

func scalarToFloat64(src any) (float64, bool) {
	switch v := src.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	default:
		return 0, false
	}
}
