package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// GitDiffOptions configures a git diff invocation.
type GitDiffOptions struct {
	// Staged selects the staged (cached) diff via --staged instead of the
	// unstaged working-tree diff.
	Staged bool
	// Path, when non-empty, restricts the diff to the given path via
	// `-- <path>`.
	Path string
}

// GitFileStatus describes a single file's status in the working tree.
type GitFileStatus struct {
	// File is the path of the changed file.
	File string
	// Status is one of "modified", "added", "deleted", "untracked",
	// "renamed", "copied", "unmerged".
	Status string
	// Staged reports whether the change is staged in the index.
	Staged bool
}

// GitCommitOptions configures a git commit invocation.
type GitCommitOptions struct {
	// Message is the commit message.
	Message string
	// AddAll runs `git add -A` before committing when true.
	AddAll bool
}

// GitTool wraps git command execution with zero dependencies (exec.Command).
// Implementations run git against a fixed working directory and return parsed
// results.
type GitTool interface {
	// Diff returns the unified diff text for staged or unstaged changes.
	Diff(ctx context.Context, opts GitDiffOptions) (string, error)
	// Status returns the list of changed files in the working tree.
	Status(ctx context.Context) ([]GitFileStatus, error)
	// Commit stages (optionally) and commits changes, returning the resulting
	// commit hash.
	Commit(ctx context.Context, opts GitCommitOptions) (string, error)
}

// DefaultGitTool implements GitTool by shelling out to the system git binary.
type DefaultGitTool struct {
	cwd string
}

var _ GitTool = (*DefaultGitTool)(nil)

// NewDefaultGitTool returns a DefaultGitTool that runs git in cwd.
func NewDefaultGitTool(cwd string) *DefaultGitTool {
	return &DefaultGitTool{cwd: cwd}
}

// ensureRepo verifies that cwd is inside a git work tree. It returns a
// user-friendly error when it is not, instead of panicking.
func (g *DefaultGitTool) ensureRepo(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = g.cwd
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git: not inside a work tree (run from within a git repository): %w", err)
	}
	return nil
}

// Diff runs `git diff` (or `git diff --staged`) with an optional path filter
// and returns the unified diff text.
func (g *DefaultGitTool) Diff(ctx context.Context, opts GitDiffOptions) (string, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return "", err
	}

	args := []string{"diff"}
	if opts.Staged {
		args = append(args, "--staged")
	}
	if strings.TrimSpace(opts.Path) != "" {
		args = append(args, "--", opts.Path)
	}

	out, err := g.run(ctx, args...)
	if err != nil {
		return "", err
	}
	slog.Debug("git.diff", "staged", opts.Staged, "path", opts.Path, "bytes", len(out))
	return out, nil
}

// Status runs `git status --porcelain` and parses the result into a list of
// GitFileStatus entries.
func (g *DefaultGitTool) Status(ctx context.Context) ([]GitFileStatus, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}

	out, err := g.run(ctx, "status", "--porcelain")
	if err != nil {
		return nil, err
	}

	var files []GitFileStatus
	for _, line := range strings.Split(out, "\n") {
		files = append(files, parsePorcelain(line)...)
	}
	slog.Debug("git.status", "files", len(files))
	return files, nil
}

// Commit optionally stages all changes with `git add -A` and then creates a
// commit with the given message. It returns the resulting commit hash.
func (g *DefaultGitTool) Commit(ctx context.Context, opts GitCommitOptions) (string, error) {
	if strings.TrimSpace(opts.Message) == "" {
		return "", fmt.Errorf("git: commit message is required")
	}
	if err := g.ensureRepo(ctx); err != nil {
		return "", err
	}

	if opts.AddAll {
		if _, err := g.run(ctx, "add", "-A"); err != nil {
			return "", fmt.Errorf("git: add -A: %w", err)
		}
	}

	if _, err := g.run(ctx, "commit", "-m", opts.Message); err != nil {
		return "", fmt.Errorf("git: commit: %w", err)
	}

	hash, err := g.run(ctx, "rev-parse", "HEAD")
	if err != nil {
		// Commit succeeded but hash lookup failed; return a generic success.
		slog.Warn("git.commit.hash_lookup_failed", "err", err)
		return "commit created", nil
	}
	slog.Debug("git.commit", "hash", strings.TrimSpace(hash))
	return strings.TrimSpace(hash), nil
}

// run executes a git command in cwd and returns its combined stdout+stderr.
func (g *DefaultGitTool) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// parsePorcelain parses a single `git status --porcelain` line into zero or
// more GitFileStatus entries. The porcelain format is `XY filename` where X is
// the index (staged) status and Y is the working-tree (unstaged) status.
func parsePorcelain(line string) []GitFileStatus {
	if len(line) < 3 {
		return nil
	}

	x := line[0]
	y := line[1]

	// The filename starts after the two status codes and a single space
	// (positions 0,1 are XY; position 2 is a space).
	file := strings.TrimSpace(line[3:])

	// Renames/copies are reported as `old -> new`; keep the new name.
	if idx := strings.Index(file, " -> "); idx >= 0 {
		file = file[idx+4:]
	}

	if file == "" {
		return nil
	}

	// Untracked files are reported as `??`.
	if x == '?' && y == '?' {
		return []GitFileStatus{{File: file, Status: "untracked", Staged: false}}
	}

	// Ignored files (`!!`) are skipped.
	if x == '!' && y == '!' {
		return nil
	}

	var results []GitFileStatus
	if x != ' ' && x != '?' {
		results = append(results, GitFileStatus{File: file, Status: porcelainStatus(x), Staged: true})
	}
	if y != ' ' && y != '?' {
		results = append(results, GitFileStatus{File: file, Status: porcelainStatus(y), Staged: false})
	}

	// If both X and Y are space (unmodified) there is nothing to report.
	if len(results) == 0 {
		return nil
	}
	return results
}

// porcelainStatus maps a porcelain status code to a human-readable label.
func porcelainStatus(code byte) string {
	switch code {
	case 'M':
		return "modified"
	case 'A':
		return "added"
	case 'D':
		return "deleted"
	case 'R':
		return "renamed"
	case 'C':
		return "copied"
	case 'U':
		return "unmerged"
	default:
		return "modified"
	}
}
