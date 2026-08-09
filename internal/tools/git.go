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

// MergeResult describes the outcome of a git merge operation.
type MergeResult struct {
	// Success reports whether the merge completed without conflicts.
	Success bool
	// Conflicts lists the files with merge conflicts (when Success is false).
	Conflicts []string
	// Message is a human-readable summary of the merge result.
	Message string
}

// RemoteInfo describes a single remote repository entry from git remote -v.
type RemoteInfo struct {
	// Name is the remote name (e.g. "origin").
	Name string
	// URL is the fetch/push URL.
	URL string
	// Type is "fetch" or "push".
	Type string
}

// WorktreeInfo describes a single git worktree entry from `git worktree list`.
type WorktreeInfo struct {
	// Path is the absolute filesystem path of the worktree.
	Path string
	// Head is the commit hash the worktree is at.
	Head string
	// Branch is the refs/heads/... name when the worktree is on a branch,
	// empty for detached HEAD.
	Branch string
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
	// CreateBranch creates a new branch named name from the given base commit
	// or branch. When base is empty, the current HEAD is used.
	CreateBranch(ctx context.Context, name string, base string) error
	// Merge merges the named branch into the current branch. It returns a
	// MergeResult describing whether conflicts occurred.
	Merge(ctx context.Context, branch string) (*MergeResult, error)
	// Stash saves uncommitted changes via git stash.
	Stash(ctx context.Context) error
	// StashPop restores the most recently stashed changes via git stash pop.
	StashPop(ctx context.Context) error
	// Reset performs git reset with the given mode (e.g. "hard", "soft",
	// "mixed"). This is a destructive operation.
	Reset(ctx context.Context, mode string) error
	// Revert creates a new commit that undoes the given commit.
	Revert(ctx context.Context, commit string) error
	// Fetch downloads objects and refs from the named remote.
	Fetch(ctx context.Context, remote string) error
	// Pull fetches from and integrate with the named remote and branch.
	Pull(ctx context.Context, remote string, branch string) error
	// Remote lists the configured remote repositories.
	Remote(ctx context.Context) ([]RemoteInfo, error)
	// WorktreeAdd creates a new worktree at the given path. When branch is
	// non-empty, a new branch named branch is created for the worktree;
	// otherwise a detached worktree at HEAD is created.
	WorktreeAdd(ctx context.Context, path string, branch string) error
	// WorktreeList lists all worktrees of the repository.
	WorktreeList(ctx context.Context) ([]WorktreeInfo, error)
	// WorktreeRemove removes the worktree at the given path. The --force flag
	// is used so that worktrees with untracked files are also removed.
	WorktreeRemove(ctx context.Context, path string) error
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

// CreateBranch runs `git branch <name> [<base>]` to create a new branch from
// the given base. When base is empty, the current HEAD is used.
func (g *DefaultGitTool) CreateBranch(ctx context.Context, name string, base string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("git: branch name is required")
	}
	if err := g.ensureRepo(ctx); err != nil {
		return err
	}

	args := []string{"branch", name}
	if strings.TrimSpace(base) != "" {
		args = append(args, base)
	}

	if _, err := g.run(ctx, args...); err != nil {
		return fmt.Errorf("git: create branch %s: %w", name, err)
	}

	slog.Debug("git.create_branch", "name", name, "base", base)
	return nil
}

// Merge runs `git merge <branch>` and parses the output to detect conflicts.
// When conflicts are present, the MergeResult.Success is false and Conflicts
// lists the conflicting files (parsed from git diff --name-only --diff-filter=U).
func (g *DefaultGitTool) Merge(ctx context.Context, branch string) (*MergeResult, error) {
	if strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("git: branch name is required")
	}
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}

	out, err := g.run(ctx, "merge", branch)
	if err != nil {
		// Merge conflicts cause git merge to exit non-zero. Check whether
		// there are unmerged files to distinguish conflicts from other errors.
		conflictOut, cErr := g.run(ctx, "diff", "--name-only", "--diff-filter=U")
		if cErr == nil {
			var conflicts []string
			for _, line := range strings.Split(conflictOut, "\n") {
				if f := strings.TrimSpace(line); f != "" {
					conflicts = append(conflicts, f)
				}
			}
			if len(conflicts) > 0 {
				return &MergeResult{
					Success:   false,
					Conflicts: conflicts,
					Message:   fmt.Sprintf("merge conflicts in %d file(s)", len(conflicts)),
				}, nil
			}
		}
		return nil, fmt.Errorf("git: merge %s: %w", branch, err)
	}

	slog.Debug("git.merge", "branch", branch)
	return &MergeResult{
		Success: true,
		Message: strings.TrimSpace(out),
	}, nil
}

// Stash runs `git stash` to save uncommitted changes.
func (g *DefaultGitTool) Stash(ctx context.Context) error {
	if err := g.ensureRepo(ctx); err != nil {
		return err
	}

	if _, err := g.run(ctx, "stash"); err != nil {
		return fmt.Errorf("git: stash: %w", err)
	}

	slog.Debug("git.stash")
	return nil
}

// StashPop runs `git stash pop` to restore the most recently stashed changes.
func (g *DefaultGitTool) StashPop(ctx context.Context) error {
	if err := g.ensureRepo(ctx); err != nil {
		return err
	}

	if _, err := g.run(ctx, "stash", "pop"); err != nil {
		return fmt.Errorf("git: stash pop: %w", err)
	}

	slog.Debug("git.stash_pop")
	return nil
}

// Reset runs `git reset --<mode>` to reset the working tree. The mode must be
// one of "hard", "soft", "mixed", "keep", or "merge". This is a destructive
// operation that can discard uncommitted changes.
func (g *DefaultGitTool) Reset(ctx context.Context, mode string) error {
	validModes := map[string]bool{"hard": true, "soft": true, "mixed": true, "keep": true, "merge": true}
	if !validModes[mode] {
		return fmt.Errorf("git: invalid reset mode %q (valid: hard, soft, mixed, keep, merge)", mode)
	}
	if err := g.ensureRepo(ctx); err != nil {
		return err
	}

	if _, err := g.run(ctx, "reset", "--"+mode); err != nil {
		return fmt.Errorf("git: reset --%s: %w", mode, err)
	}

	slog.Debug("git.reset", "mode", mode)
	return nil
}

// Revert runs `git revert <commit>` to create a new commit that undoes the
// given commit. This is a potentially destructive operation.
func (g *DefaultGitTool) Revert(ctx context.Context, commit string) error {
	if strings.TrimSpace(commit) == "" {
		return fmt.Errorf("git: commit is required")
	}
	if err := g.ensureRepo(ctx); err != nil {
		return err
	}

	if _, err := g.run(ctx, "revert", "--no-edit", commit); err != nil {
		return fmt.Errorf("git: revert %s: %w", commit, err)
	}

	slog.Debug("git.revert", "commit", commit)
	return nil
}

// Fetch runs `git fetch <remote>` to download objects and refs from the named
// remote. When remote is empty, "origin" is used.
func (g *DefaultGitTool) Fetch(ctx context.Context, remote string) error {
	if err := g.ensureRepo(ctx); err != nil {
		return err
	}

	if strings.TrimSpace(remote) == "" {
		remote = "origin"
	}

	if _, err := g.run(ctx, "fetch", remote); err != nil {
		return fmt.Errorf("git: fetch %s: %w", remote, err)
	}

	slog.Debug("git.fetch", "remote", remote)
	return nil
}

// Pull runs `git pull <remote> <branch>` to fetch and integrate changes. When
// remote is empty, "origin" is used. When branch is empty, the current branch
// is used.
func (g *DefaultGitTool) Pull(ctx context.Context, remote string, branch string) error {
	if err := g.ensureRepo(ctx); err != nil {
		return err
	}

	if strings.TrimSpace(remote) == "" {
		remote = "origin"
	}

	args := []string{"pull", remote}
	if strings.TrimSpace(branch) != "" {
		args = append(args, branch)
	}

	if _, err := g.run(ctx, args...); err != nil {
		return fmt.Errorf("git: pull %s %s: %w", remote, branch, err)
	}

	slog.Debug("git.pull", "remote", remote, "branch", branch)
	return nil
}

// Remote runs `git remote -v` and parses the result into RemoteInfo entries.
// Each remote appears twice (once for fetch, once for push) unless they share
// the same URL.
func (g *DefaultGitTool) Remote(ctx context.Context) ([]RemoteInfo, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}

	out, err := g.run(ctx, "remote", "-v")
	if err != nil {
		return nil, err
	}

	var remotes []RemoteInfo
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Format: <name>\t<url> (fetch|push)
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		name := parts[0]
		url := parts[1]
		remoteType := strings.Trim(parts[2], "()")
		remotes = append(remotes, RemoteInfo{
			Name: name,
			URL:  url,
			Type: remoteType,
		})
	}

	slog.Debug("git.remote", "remotes", len(remotes))
	return remotes, nil
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

// WorktreeAdd creates a new worktree at the given path. When branch is
// non-empty, a new branch named branch is created for the worktree via
// `git worktree add -b <branch> <path>`. When branch is empty, a detached
// worktree is created at HEAD via `git worktree add --detach <path>`.
func (g *DefaultGitTool) WorktreeAdd(ctx context.Context, path string, branch string) error {
	if err := g.ensureRepo(ctx); err != nil {
		return err
	}
	args := []string{"worktree", "add"}
	if branch != "" {
		args = append(args, "-b", branch, "--", path)
	} else {
		args = append(args, "--detach", "--", path)
	}
	_, err := g.run(ctx, args...)
	return err
}

// WorktreeList lists all worktrees of the repository via
// `git worktree list --porcelain`.
func (g *DefaultGitTool) WorktreeList(ctx context.Context) ([]WorktreeInfo, error) {
	if err := g.ensureRepo(ctx); err != nil {
		return nil, err
	}
	out, err := g.run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreePorcelain(out), nil
}

// WorktreeRemove removes the worktree at the given path. The --force flag is
// used so that worktrees with untracked or modified files are also removed.
func (g *DefaultGitTool) WorktreeRemove(ctx context.Context, path string) error {
	if err := g.ensureRepo(ctx); err != nil {
		return err
	}
	_, err := g.run(ctx, "worktree", "remove", "--force", "--", path)
	return err
}

// parseWorktreePorcelain parses the output of `git worktree list --porcelain`
// into a slice of WorktreeInfo. The porcelain format uses one record per
// worktree, with fields prefixed by their key ("worktree", "HEAD", "branch")
// and records separated by blank lines.
func parseWorktreePorcelain(out string) []WorktreeInfo {
	var infos []WorktreeInfo
	var current *WorktreeInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if current != nil {
				infos = append(infos, *current)
				current = nil
			}
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		key := parts[0]
		var value string
		if len(parts) > 1 {
			value = parts[1]
		}
		switch key {
		case "worktree":
			if current != nil {
				infos = append(infos, *current)
			}
			current = &WorktreeInfo{Path: value}
		case "HEAD":
			if current != nil {
				current.Head = value
			}
		case "branch":
			if current != nil {
				current.Branch = value
			}
		}
	}
	if current != nil {
		infos = append(infos, *current)
	}
	return infos
}
