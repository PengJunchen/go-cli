package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
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

// GitLogOptions configures a git log invocation.
type GitLogOptions struct {
	// MaxCount limits the number of commits returned (--max-count).
	MaxCount int
	// Since restricts to commits after the given date (--since).
	Since string
	// Until restricts to commits before the given date (--until).
	Until string
	// Author filters commits by author name/email (--author).
	Author string
	// Path, when non-empty, restricts the log to commits touching the given
	// path via `-- <path>`.
	Path string
}

// GitLogEntry represents a single commit from git log.
type GitLogEntry struct {
	Hash    string
	Author  string
	Email   string
	Date    string
	Message string
}

// GitBranch describes a single branch (local or remote).
type GitBranch struct {
	Name    string
	Current bool
	Remote  bool
}

// GitBlameLine represents a single line's attribution from git blame.
type GitBlameLine struct {
	Hash    string
	Author  string
	Email   string
	Date    string
	LineNum int
	Content string
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
	// Log returns commit history entries matching the given options.
	Log(ctx context.Context, opts GitLogOptions) ([]GitLogEntry, error)
	// Branch returns the list of local and remote branches.
	Branch(ctx context.Context) ([]GitBranch, error)
	// Checkout switches to the named branch.
	Checkout(ctx context.Context, branch string) error
	// Blame returns line-by-line attribution for a file range.
	Blame(ctx context.Context, file string, startLine, endLine int) ([]GitBlameLine, error)
	// Push pushes the named branch to the named remote, optionally forcing.
	Push(ctx context.Context, remote string, branch string, force bool) error
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

// Log runs `git log` with the given options and parses the result into
// GitLogEntry values. The format uses a unit-separator (0x1f) delimiter
// between fields so that commit messages containing spaces parse correctly.
func (g *DefaultGitTool) Log(ctx context.Context, opts GitLogOptions) ([]GitLogEntry, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}

	args := []string{"log", "--format=%H%x1f%an%x1f%ae%x1f%ad%x1f%s"}
	if opts.MaxCount > 0 {
		args = append(args, fmt.Sprintf("--max-count=%d", opts.MaxCount))
	}
	if strings.TrimSpace(opts.Since) != "" {
		args = append(args, "--since="+opts.Since)
	}
	if strings.TrimSpace(opts.Until) != "" {
		args = append(args, "--until="+opts.Until)
	}
	if strings.TrimSpace(opts.Author) != "" {
		args = append(args, "--author="+opts.Author)
	}
	if strings.TrimSpace(opts.Path) != "" {
		args = append(args, "--", opts.Path)
	}

	out, err := g.run(ctx, args...)
	if err != nil {
		return nil, err
	}

	var entries []GitLogEntry
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x1f", 5)
		if len(parts) < 5 {
			continue
		}
		entries = append(entries, GitLogEntry{
			Hash:    parts[0],
			Author:  parts[1],
			Email:   parts[2],
			Date:    parts[3],
			Message: parts[4],
		})
	}

	slog.Debug("git.log", "entries", len(entries))
	return entries, nil
}

// Branch runs `git branch -a` and parses local and remote branches.
func (g *DefaultGitTool) Branch(ctx context.Context) ([]GitBranch, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}

	out, err := g.run(ctx, "branch", "-a")
	if err != nil {
		return nil, err
	}

	var branches []GitBranch
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		current := strings.HasPrefix(line, "*")
		name := strings.TrimSpace(line)
		name = strings.TrimPrefix(name, "* ")
		name = strings.TrimSpace(name)

		remote := false
		if strings.HasPrefix(name, "remotes/") {
			remote = true
			name = strings.TrimPrefix(name, "remotes/")
		}

		if name == "" || name == "HEAD" || strings.HasPrefix(name, "(HEAD") {
			continue
		}

		branches = append(branches, GitBranch{
			Name:    name,
			Current: current,
			Remote:  remote,
		})
	}

	slog.Debug("git.branch", "branches", len(branches))
	return branches, nil
}

// Checkout runs `git checkout <branch>` to switch the working tree to the
// named branch.
func (g *DefaultGitTool) Checkout(ctx context.Context, branch string) error {
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("git: branch name is required")
	}
	if err := g.ensureRepo(ctx); err != nil {
		return err
	}

	if _, err := g.run(ctx, "checkout", branch); err != nil {
		return fmt.Errorf("git: checkout %s: %w", branch, err)
	}

	slog.Debug("git.checkout", "branch", branch)
	return nil
}

// Blame runs `git blame --line-porcelain -L <start>,<end> <file>` and parses
// the result into GitBlameLine values.
func (g *DefaultGitTool) Blame(ctx context.Context, file string, startLine, endLine int) ([]GitBlameLine, error) {
	if strings.TrimSpace(file) == "" {
		return nil, fmt.Errorf("git: file is required")
	}
	if startLine < 1 {
		startLine = 1
	}
	if endLine < startLine {
		endLine = startLine
	}
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}

	rangeArg := fmt.Sprintf("%d,%d", startLine, endLine)
	out, err := g.run(ctx, "blame", "--line-porcelain", "-L", rangeArg, file)
	if err != nil {
		return nil, err
	}

	var lines []GitBlameLine
	var cur GitBlameLine
	started := false

	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "\t"):
			cur.Content = line[1:]
			lines = append(lines, cur)
			cur = GitBlameLine{}
			started = false
		case strings.HasPrefix(line, "author "):
			cur.Author = strings.TrimPrefix(line, "author ")
		case strings.HasPrefix(line, "author-mail "):
			email := strings.TrimPrefix(line, "author-mail ")
			cur.Email = strings.Trim(email, "<>")
		case strings.HasPrefix(line, "author-time "):
			ts := strings.TrimPrefix(line, "author-time ")
			if sec, parseErr := strconv.ParseInt(ts, 10, 64); parseErr == nil {
				cur.Date = time.Unix(sec, 0).Format(time.RFC3339)
			}
		case !started:
			// Header line: <hash> <orig-line> <final-line>
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				cur.Hash = parts[0]
				if n, parseErr := strconv.Atoi(parts[2]); parseErr == nil {
					cur.LineNum = n
				}
			}
			started = true
		}
	}

	slog.Debug("git.blame", "lines", len(lines))
	return lines, nil
}

// Push runs `git push [--force] <remote> <branch>`.
func (g *DefaultGitTool) Push(ctx context.Context, remote string, branch string, force bool) error {
	if strings.TrimSpace(remote) == "" {
		return fmt.Errorf("git: remote is required")
	}
	if strings.TrimSpace(branch) == "" {
		return fmt.Errorf("git: branch is required")
	}
	if err := g.ensureRepo(ctx); err != nil {
		return err
	}

	args := []string{"push"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, remote, branch)

	if _, err := g.run(ctx, args...); err != nil {
		return fmt.Errorf("git: push %s %s: %w", remote, branch, err)
	}

	slog.Debug("git.push", "remote", remote, "branch", branch, "force", force)
	return nil
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
