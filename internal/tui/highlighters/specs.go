// Package highlighters provides language-specific syntax highlighting
// specifications used by the TUI code highlighter. Each LanguageSpec defines
// the keyword set, comment styles, and string delimiters for a language.
package highlighters

// LanguageSpec defines syntax highlighting rules for a language.
type LanguageSpec struct {
	// Keywords is the set of reserved words to color as keywords.
	Keywords map[string]bool
	// CommentLine is the line-comment prefix ("//", "#", "--").
	// When empty, the highlighter falls back to checking both "//" and "#".
	CommentLine string
	// CommentBlock is the block-comment open/close pair (e.g. "/*" "*/").
	// Both elements must be non-empty for block comment handling; otherwise
	// leave them empty.
	CommentBlock [2]string
	// StringDelims lists the characters that start/end string literals
	// (e.g. "\"", "'", "`").
	StringDelims []string
	// CaseInsensitive indicates whether keyword matching should ignore case
	// (e.g. SQL where SELECT and select are both keywords).
	CaseInsensitive bool
}

// goSpec defines highlighting rules for Go.
var goSpec = LanguageSpec{
	Keywords: map[string]bool{
		"break": true, "case": true, "chan": true, "const": true, "continue": true,
		"default": true, "defer": true, "else": true, "for": true, "func": true,
		"go": true, "if": true, "import": true, "interface": true, "map": true,
		"nil": true, "package": true, "range": true, "return": true, "select": true,
		"struct": true, "switch": true, "type": true, "var": true,
		"true": true, "false": true,
	},
	CommentLine:  "//",
	CommentBlock: [2]string{"/*", "*/"},
	StringDelims: []string{"\"", "'", "`"},
}

// pythonSpec defines highlighting rules for Python.
var pythonSpec = LanguageSpec{
	Keywords: map[string]bool{
		"def": true, "class": true, "import": true, "from": true, "return": true,
		"if": true, "elif": true, "else": true, "for": true, "while": true,
		"try": true, "except": true, "finally": true, "with": true, "as": true,
		"lambda": true, "yield": true, "pass": true, "break": true, "continue": true,
		"raise": true, "None": true, "True": true, "False": true,
		"and": true, "or": true, "not": true, "in": true, "is": true,
	},
	CommentLine:  "#",
	CommentBlock: [2]string{},
	StringDelims: []string{"\"", "'"},
}

// javascriptSpec defines highlighting rules for JavaScript.
var javascriptSpec = LanguageSpec{
	Keywords: map[string]bool{
		"function": true, "var": true, "let": true, "const": true, "class": true,
		"extends": true, "return": true, "if": true, "else": true, "for": true,
		"while": true, "switch": true, "case": true, "break": true, "continue": true,
		"new": true, "this": true, "typeof": true, "instanceof": true,
		"null": true, "undefined": true, "true": true, "false": true,
		"async": true, "await": true, "import": true, "export": true,
		"from": true, "default": true,
	},
	CommentLine:  "//",
	CommentBlock: [2]string{"/*", "*/"},
	StringDelims: []string{"\"", "'", "`"},
}

// typescriptSpec defines highlighting rules for TypeScript.
var typescriptSpec = LanguageSpec{
	Keywords: map[string]bool{
		"function": true, "var": true, "let": true, "const": true, "class": true,
		"extends": true, "return": true, "if": true, "else": true, "for": true,
		"while": true, "switch": true, "case": true, "break": true, "continue": true,
		"new": true, "this": true, "typeof": true, "instanceof": true,
		"null": true, "undefined": true, "true": true, "false": true,
		"async": true, "await": true, "import": true, "export": true,
		"from": true, "default": true,
		// TypeScript-specific keywords.
		"type": true, "interface": true, "enum": true, "namespace": true,
		"as": true, "is": true,
	},
	CommentLine:  "//",
	CommentBlock: [2]string{"/*", "*/"},
	StringDelims: []string{"\"", "'", "`"},
}

// rustSpec defines highlighting rules for Rust.
var rustSpec = LanguageSpec{
	Keywords: map[string]bool{
		"fn": true, "let": true, "mut": true, "const": true, "static": true,
		"struct": true, "enum": true, "trait": true, "impl": true, "pub": true,
		"use": true, "mod": true, "return": true, "if": true, "else": true,
		"for": true, "while": true, "loop": true, "match": true, "break": true,
		"continue": true, "unsafe": true, "ref": true, "self": true, "Self": true,
		"true": true, "false": true,
	},
	CommentLine:  "//",
	CommentBlock: [2]string{"/*", "*/"},
	StringDelims: []string{"\"", "'"},
}

// javaSpec defines highlighting rules for Java.
var javaSpec = LanguageSpec{
	Keywords: map[string]bool{
		"public": true, "private": true, "protected": true, "class": true,
		"interface": true, "extends": true, "implements": true, "static": true,
		"final": true, "void": true, "int": true, "long": true, "double": true,
		"float": true, "boolean": true, "char": true, "String": true,
		"return": true, "if": true, "else": true, "for": true, "while": true,
		"switch": true, "case": true, "break": true, "continue": true,
		"new": true, "this": true, "null": true, "true": true, "false": true,
	},
	CommentLine:  "//",
	CommentBlock: [2]string{"/*", "*/"},
	StringDelims: []string{"\"", "'"},
}

// bashSpec defines highlighting rules for Bash/Shell.
var bashSpec = LanguageSpec{
	Keywords: map[string]bool{
		"if": true, "then": true, "else": true, "elif": true, "fi": true,
		"for": true, "while": true, "do": true, "done": true, "case": true,
		"esac": true, "function": true, "return": true, "export": true,
		"local": true, "echo": true, "read": true, "source": true, "exit": true,
		"true": true, "false": true,
	},
	CommentLine:  "#",
	CommentBlock: [2]string{},
	StringDelims: []string{"\"", "'"},
}

// jsonSpec defines highlighting rules for JSON.
var jsonSpec = LanguageSpec{
	Keywords: map[string]bool{
		"true": true, "false": true, "null": true,
	},
	CommentLine:  "",
	CommentBlock: [2]string{},
	StringDelims: []string{"\""},
}

// yamlSpec defines highlighting rules for YAML.
var yamlSpec = LanguageSpec{
	Keywords: map[string]bool{
		"true": true, "false": true, "null": true,
		"yes": true, "no": true, "on": true, "off": true,
	},
	CommentLine:  "#",
	CommentBlock: [2]string{},
	StringDelims: []string{"\"", "'"},
}

// sqlSpec defines highlighting rules for SQL (case-insensitive).
var sqlSpec = LanguageSpec{
	Keywords: map[string]bool{
		"SELECT": true, "FROM": true, "WHERE": true, "INSERT": true,
		"UPDATE": true, "DELETE": true, "CREATE": true, "TABLE": true,
		"DROP": true, "ALTER": true, "INDEX": true, "VIEW": true,
		"JOIN": true, "LEFT": true, "RIGHT": true, "INNER": true,
		"OUTER": true, "ON": true, "AS": true, "ORDER": true, "BY": true,
		"GROUP": true, "HAVING": true, "LIMIT": true, "OFFSET": true,
		"DISTINCT": true, "UNION": true, "ALL": true, "AND": true,
		"OR": true, "NOT": true, "NULL": true, "IN": true, "EXISTS": true,
		"BETWEEN": true, "LIKE": true, "CASE": true, "WHEN": true,
		"THEN": true, "ELSE": true, "END": true, "COUNT": true, "SUM": true,
		"AVG": true, "MIN": true, "MAX": true,
	},
	CommentLine:     "--",
	CommentBlock:    [2]string{"/*", "*/"},
	StringDelims:    []string{"'"},
	CaseInsensitive: true,
}

// Specs maps language names (including common aliases) to their LanguageSpec.
// Aliases map to the same underlying spec values.
var Specs = map[string]LanguageSpec{
	"go":         goSpec,
	"golang":     goSpec,
	"python":     pythonSpec,
	"py":         pythonSpec,
	"javascript": javascriptSpec,
	"js":         javascriptSpec,
	"typescript": typescriptSpec,
	"ts":         typescriptSpec,
	"rust":       rustSpec,
	"rs":         rustSpec,
	"java":       javaSpec,
	"bash":       bashSpec,
	"sh":         bashSpec,
	"shell":      bashSpec,
	"json":       jsonSpec,
	"yaml":       yamlSpec,
	"yml":        yamlSpec,
	"sql":        sqlSpec,
}
