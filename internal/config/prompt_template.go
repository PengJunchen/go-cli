package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

// PromptTemplate is a reusable prompt with parameter placeholders.
type PromptTemplate struct {
	Name        string
	Template    string // contains {{.Param}} placeholders
	Description string
	Parameters  []string
}

// PromptTemplateLoader loads templates from files in a directory.
type PromptTemplateLoader struct {
	dir string
}

// tmplExt is the file extension used for prompt templates.
const tmplExt = ".tmpl"

// paramPattern matches {{.Param}} and {{ .Param }} style placeholders.
var paramPattern = regexp.MustCompile(`{{\s*\.\s*([A-Za-z_][A-Za-z0-9_]*)\s*`)

// NewPromptTemplateLoader returns a loader that reads .tmpl files from dir.
func NewPromptTemplateLoader(dir string) *PromptTemplateLoader {
	return &PromptTemplateLoader{dir: dir}
}

// Load reads a single template named {dir}/{name}.tmpl. The Name field is set
// to name; Parameters are extracted from the template body; Description is
// parsed from an optional leading line of the form "# description: ...".
func (l *PromptTemplateLoader) Load(name string) (*PromptTemplate, error) {
	path := filepath.Join(l.dir, name+tmplExt)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load prompt template %s: %w", name, err)
	}
	body := string(data)
	desc, body := parseTemplateDescription(body)

	tmpl := &PromptTemplate{
		Name:        name,
		Template:    body,
		Description: desc,
		Parameters:  extractParameters(body),
	}
	slog.Debug("config.prompt.load",
		"op", "config.prompt.load",
		"name", name,
		"params", len(tmpl.Parameters),
	)
	return tmpl, nil
}

// LoadAll loads every .tmpl file in the loader's directory, returning the
// templates sorted by name. A missing directory yields an empty slice and no
// error.
func (l *PromptTemplateLoader) LoadAll() ([]*PromptTemplate, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read prompt template dir %s: %w", l.dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), tmplExt) {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), tmplExt))
	}
	sort.Strings(names)

	out := make([]*PromptTemplate, 0, len(names))
	for _, n := range names {
		t, err := l.Load(n)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	slog.Info("config.prompt.load_all",
		"op", "config.prompt.load_all",
		"dir", l.dir,
		"count", len(out),
	)
	return out, nil
}

// Render executes tmpl with params, replacing {{.Param}} placeholders with the
// corresponding values. Declared parameters absent from params render as empty
// strings (rather than the default "<no value>") so partial renders stay clean.
func (l *PromptTemplateLoader) Render(tmpl *PromptTemplate, params map[string]string) (string, error) {
	t, err := template.New(tmpl.Name).Option("missingkey=zero").Parse(tmpl.Template)
	if err != nil {
		return "", fmt.Errorf("parse prompt template %s: %w", tmpl.Name, err)
	}
	// Pre-fill declared parameters with empty strings so missing values render
	// as "" instead of "<no value>".
	merged := make(map[string]string, len(params)+len(tmpl.Parameters))
	for _, p := range tmpl.Parameters {
		merged[p] = ""
	}
	for k, v := range params {
		merged[k] = v
	}
	var b strings.Builder
	if err := t.Execute(&b, merged); err != nil {
		return "", fmt.Errorf("render prompt template %s: %w", tmpl.Name, err)
	}
	slog.Debug("config.prompt.render",
		"op", "config.prompt.render",
		"name", tmpl.Name,
		"params", len(params),
	)
	return b.String(), nil
}

// parseTemplateDescription strips an optional leading description directive of
// the form "# description: ..." from the body and returns (description,
// remaining body). When no directive is present the whole body is returned
// with an empty description.
func parseTemplateDescription(body string) (string, string) {
	const prefix = "# description:"
	trimmed := strings.TrimLeft(body, "\n\r\t ")
	if !strings.HasPrefix(trimmed, prefix) {
		return "", body
	}
	rest := trimmed[len(prefix):]
	newline := strings.IndexByte(rest, '\n')
	if newline < 0 {
		return strings.TrimSpace(rest), ""
	}
	desc := strings.TrimSpace(rest[:newline])
	remaining := strings.TrimLeft(rest[newline+1:], "\n\r\t ")
	return desc, remaining
}

// extractParameters returns the sorted unique set of {{.Param}} placeholders
// found in body.
func extractParameters(body string) []string {
	matches := paramPattern.FindAllStringSubmatch(body, -1)
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		seen[m[1]] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
