package tools

import (
	"context"
	"strings"
)

// DiffGenerator generates unified diffs between old and new content.
type DiffGenerator interface {
	Generate(oldContent, newContent, path string) (string, error)
}

// ANSI color codes used when color output is enabled.
const (
	ansiRed   = "\033[31m"
	ansiGreen = "\033[32m"
	ansiReset = "\033[0m"
)

// UnifiedDiffGenerator implements DiffGenerator using a simple LCS-based
// line-by-line diff. It is safe for concurrent use: it holds no mutable state
// after construction.
type UnifiedDiffGenerator struct {
	// maxLines is the maximum number of body lines to show. When 0 the output
	// is unbounded.
	maxLines int
	// color enables ANSI color escape codes in the output.
	color bool
}

// Compile-time check that UnifiedDiffGenerator satisfies DiffGenerator.
var _ DiffGenerator = (*UnifiedDiffGenerator)(nil)

// NewUnifiedDiffGenerator returns a UnifiedDiffGenerator with the given options.
func NewUnifiedDiffGenerator(maxLines int, color bool) *UnifiedDiffGenerator {
	return &UnifiedDiffGenerator{maxLines: maxLines, color: color}
}

// diffOp classifies a single line in the computed diff.
type diffOp int

const (
	opEqual  diffOp = iota // line present in both old and new (context)
	opDelete               // line removed from old
	opInsert               // line added in new
)

// diffEntry pairs an op with its line text.
type diffEntry struct {
	op   diffOp
	line string
}

// Generate produces a unified diff between oldContent and newContent for the
// file at path. A new file (empty oldContent) yields all added lines; a deleted
// file (empty newContent) yields all removed lines; identical content yields an
// empty string. When maxLines is positive the body is truncated to roughly
// maxLines lines (first half + "..." + last half).
func (g *UnifiedDiffGenerator) Generate(oldContent, newContent, path string) (string, error) {
	// New file: every line is an addition.
	if oldContent == "" {
		lines := splitDiffLines(newContent)
		body := g.formatAdded(lines)
		body = g.truncateBody(body)
		return joinLines(body), nil
	}

	// Deleted file: every line is a removal.
	if newContent == "" {
		lines := splitDiffLines(oldContent)
		body := g.formatRemoved(lines)
		body = g.truncateBody(body)
		return joinLines(body), nil
	}

	oldLines := splitDiffLines(oldContent)
	newLines := splitDiffLines(newContent)
	entries := lcsDiff(oldLines, newLines)

	// No changes at all: return empty output.
	hasChange := false
	for _, e := range entries {
		if e.op != opEqual {
			hasChange = true
			break
		}
	}
	if !hasChange {
		return "", nil
	}

	body := g.formatDiff(entries)
	body = g.truncateBody(body)

	var sb strings.Builder
	sb.WriteString("--- a/")
	sb.WriteString(path)
	sb.WriteString("\n")
	sb.WriteString("+++ b/")
	sb.WriteString(path)
	sb.WriteString("\n")
	sb.WriteString(joinLines(body))
	return sb.String(), nil
}

// formatAdded renders every line as an insertion.
func (g *UnifiedDiffGenerator) formatAdded(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, g.colorize("+", l, ansiGreen))
	}
	return out
}

// formatRemoved renders every line as a deletion.
func (g *UnifiedDiffGenerator) formatRemoved(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, g.colorize("-", l, ansiRed))
	}
	return out
}

// formatDiff renders a sequence of diff entries into unified-diff body lines.
func (g *UnifiedDiffGenerator) formatDiff(entries []diffEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		switch e.op {
		case opEqual:
			out = append(out, " "+e.line)
		case opDelete:
			out = append(out, g.colorize("-", e.line, ansiRed))
		case opInsert:
			out = append(out, g.colorize("+", e.line, ansiGreen))
		}
	}
	return out
}

// colorize prefixes a line with sign and, when color is enabled, wraps it in
// the given ANSI color.
func (g *UnifiedDiffGenerator) colorize(sign, line, color string) string {
	if g.color {
		return color + sign + line + ansiReset
	}
	return sign + line
}

// truncateBody limits body to roughly maxLines entries using a first-half +
// "..." + last-half scheme. When maxLines is 0 or body already fits, it is
// returned unchanged.
func (g *UnifiedDiffGenerator) truncateBody(body []string) []string {
	if g.maxLines <= 0 || len(body) <= g.maxLines {
		return body
	}
	first := g.maxLines / 2
	last := g.maxLines - first
	out := make([]string, 0, first+1+last)
	out = append(out, body[:first]...)
	out = append(out, "...")
	out = append(out, body[len(body)-last:]...)
	return out
}

// splitDiffLines splits s into lines, dropping a trailing empty element
// produced by a final newline. Returns nil for empty input.
func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// joinLines joins lines with newlines, ensuring a trailing newline.
func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// lcsDiff computes a line-level diff between a and b using a longest-common-
// subsequence table. It runs in O(len(a)*len(b)) time and space, which is
// adequate for typical source files.
func lcsDiff(a, b []string) []diffEntry {
	n, m := len(a), len(b)

	// dp[i][j] = length of LCS of a[i:] and b[j:].
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = maxInt(dp[i+1][j], dp[i][j+1])
			}
		}
	}

	out := make([]diffEntry, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		if a[i] == b[j] {
			out = append(out, diffEntry{op: opEqual, line: a[i]})
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			out = append(out, diffEntry{op: opDelete, line: a[i]})
			i++
		} else {
			out = append(out, diffEntry{op: opInsert, line: b[j]})
			j++
		}
	}
	for i < n {
		out = append(out, diffEntry{op: opDelete, line: a[i]})
		i++
	}
	for j < m {
		out = append(out, diffEntry{op: opInsert, line: b[j]})
		j++
	}
	return out
}

// maxInt returns the larger of a and b.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// GitDiffGenerator implements DiffGenerator by delegating to `git diff` when
// inside a git repository. When the path is not inside a git repo (or the git
// command fails), it falls back to a wrapped DiffGenerator (typically
// UnifiedDiffGenerator). This gives more accurate diffs in git repos (rename
// detection, binary handling) while remaining backward-compatible outside.
type GitDiffGenerator struct {
	git      GitTool
	fallback DiffGenerator
}

// Compile-time check that GitDiffGenerator satisfies DiffGenerator.
var _ DiffGenerator = (*GitDiffGenerator)(nil)

// NewGitDiffGenerator returns a GitDiffGenerator that uses the given GitTool
// for `git diff` and falls back to fallback when git is unavailable or the diff
// is empty. Both git and fallback must be non-nil.
func NewGitDiffGenerator(git GitTool, fallback DiffGenerator) *GitDiffGenerator {
	return &GitDiffGenerator{git: git, fallback: fallback}
}

// Generate tries `git diff -- <path>` first. When the git diff succeeds and
// returns non-empty output, it is returned directly. Otherwise (not in a git
// repo, no changes, or error), the fallback DiffGenerator is used.
func (g *GitDiffGenerator) Generate(oldContent, newContent, path string) (string, error) {
	if g.git != nil && strings.TrimSpace(path) != "" {
		out, err := g.git.Diff(context.Background(), GitDiffOptions{Path: path})
		if err == nil && strings.TrimSpace(out) != "" {
			return out, nil
		}
	}
	return g.fallback.Generate(oldContent, newContent, path)
}
