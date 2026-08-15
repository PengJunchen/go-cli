package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// defaultCommandBlacklist lists commands that are too destructive to allow
// inside a sandboxed bash tool. It also blocks commands that can be used to
// bypass the sandbox by spawning a new shell or evaluating arbitrary input.
//
// The list is organized into categories:
//  1. Destructive commands that can damage the system or user data.
//  2. Shell escape / eval commands that bypass the sandbox by spawning a new
//     shell or evaluating arbitrary input.
//  3. Script interpreters (python, perl, ruby, etc.) that can execute arbitrary
//     code and thereby bypass the command blacklist. Network tools (curl, wget,
//     nc, etc.) are also blocked to prevent data exfiltration and unauthorized
//     outbound connections from within the sandbox.
//  4. Environment disclosure commands (env, printenv) that leak secrets stored
//     in environment variables.
var defaultCommandBlacklist = []string{
	// Destructive commands.
	"rm", "rmdir", "dd", "mkfs", "fdisk",
	"shutdown", "reboot", "halt", "poweroff",
	"kill", "killall", "pkill",
	// Shell escape / eval commands.
	"eval", "bash", "sh", "source", "exec",
	// Script interpreters: can execute arbitrary code, bypassing the blacklist.
	"python", "python3", "perl", "ruby", "node", "php", "lua",
	// Network tools: can exfiltrate data or open unauthorized connections.
	"curl", "wget", "nc", "ncat", "telnet", "scp", "rsync",
	// Environment disclosure: leak secrets stored in env vars.
	"env", "printenv",
}

// commandPrefixes lists commands that prefix another command and would bypass
// the first-token blacklist check (e.g. "sudo rm" has first token "sudo"). The
// "function" keyword is included so that "function rm { ... }" definitions are
// reduced to the defined name ("rm") before the blacklist comparison. Note that
// "env" is intentionally NOT a prefix: it is blacklisted directly (see AC-3) so
// that bare "env" / "printenv" invocations are blocked regardless of args.
var commandPrefixes = map[string]bool{
	"sudo": true, "doas": true, "su": true,
	"nohup": true, "time": true, "nice": true, "command": true,
	"xargs": true, "strace": true, "ltrace": true, "timeout": true,
	"function": true,
}

// DefaultCommandWhitelist lists command prefixes that are considered safe to run
// without explicit approval when the sandbox operates in whitelist mode (see
// WithCommandWhitelist). Entries may be multi-token (e.g. "go test") to scope
// the allowance to a specific subcommand. Blacklisted commands and indirect
// executors are always blocked, even when they appear in this list.
var DefaultCommandWhitelist = []string{
	"go test", "go build", "go vet", "go fmt", "go run", "go mod", "go list", "go doc",
	"ls", "cat", "echo", "pwd", "head", "tail", "wc", "grep", "sort", "uniq", "tr", "cut",
	"git status", "git diff", "git log", "git show", "git branch",
	"mkdir", "touch", "date", "which", "file", "stat", "find",
}

// BashSandbox validates bash commands before execution. Implementations check
// constraints such as allowed working directories and blocked commands.
type BashSandbox interface {
	Validate(ctx context.Context, cmd string, workDir string) error
}

// AllowAllSandbox is a no-op BashSandbox that permits every command and
// working directory. It is the "no-sandbox" equivalent used by
// WithNoSandbox() to explicitly opt out of sandbox enforcement.
type AllowAllSandbox struct{}

var _ BashSandbox = AllowAllSandbox{}

// Validate always returns nil, allowing any command in any directory.
func (AllowAllSandbox) Validate(_ context.Context, _, _ string) error { return nil }

// PathWhitelist checks if a working directory falls under one of the allowed
// base paths. An empty whitelist allows any path.
type PathWhitelist struct {
	paths []string
}

// NewPathWhitelist builds a PathWhitelist from the given base paths. Each path
// is cleaned with filepath.Clean and resolved with resolveSymlinks before
// storage, so that whitelist comparisons operate on real paths.
func NewPathWhitelist(paths []string) PathWhitelist {
	cleaned := make([]string, 0, len(paths))
	for _, p := range paths {
		cleaned = append(cleaned, resolveSymlinks(filepath.Clean(p)))
	}
	return PathWhitelist{paths: cleaned}
}

// IsAllowed reports whether workDir is either equal to or nested under one of
// the whitelisted base paths. When no paths are configured every directory is
// allowed.
func (wl PathWhitelist) IsAllowed(workDir string) bool {
	if len(wl.paths) == 0 {
		return true
	}
	cleaned := resolveSymlinks(filepath.Clean(workDir))
	for _, base := range wl.paths {
		if cleaned == base {
			return true
		}
		if strings.HasPrefix(cleaned, base+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// CommandFilter checks parsed command strings against a blacklist of
// disallowed command names and, optionally, a whitelist of approved command
// prefixes. It understands command separators (;), pipes (|), logical operators
// (&& and ||), command substitution $(...) and `...`, and normalizes shell
// quoting/escapes/function definitions before matching so that 'rm', "rm",
// $'rm', \rm and rm(){} are all reduced to "rm".
type CommandFilter struct {
	blacklist []string
	whitelist []string
}

// NewCommandFilter builds a CommandFilter from the given blacklist. The
// whitelist is empty (whitelist mode disabled) unless WithCommandWhitelist is
// used.
func NewCommandFilter(blacklist []string) CommandFilter {
	return CommandFilter{blacklist: blacklist}
}

// IsBlocked reports whether any command referenced by cmd is blocked. A command
// is blocked when it matches the blacklist, uses an indirect executor, or
// (when a whitelist is configured) does not match any whitelisted prefix.
func (f CommandFilter) IsBlocked(cmd string) bool {
	blocked, _ := f.blockReason(cmd)
	return blocked
}

// BlockReason returns a non-empty human-readable reason when cmd is blocked, or
// "" when the command is allowed. It is used by Validate to produce descriptive
// errors.
func (f CommandFilter) BlockReason(cmd string) string {
	_, reason := f.blockReason(cmd)
	return reason
}

// blockReason is the recursive core of the filter. It returns whether the
// command string is blocked and, when it is, a short reason category. The
// command string is split on semicolons, pipes and logical operators and each
// segment's effective command is checked after normalization. Command
// substitutions $(...) and backticks `...` and function-definition bodies are
// recursively inspected.
func (f CommandFilter) blockReason(s string) (bool, string) {
	// Block heredoc syntax (<<) which can write arbitrary content to files.
	if containsHeredoc(s) {
		return true, "blacklisted heredoc redirection"
	}
	// Recursively inspect command substitutions.
	for _, inner := range extractSubShells(s) {
		if blocked, reason := f.blockReason(inner); blocked {
			return true, reason
		}
	}
	// Recursively inspect function-definition bodies so that commands hidden
	// inside a function (e.g. "helper(){ rm file; }") are caught.
	for _, body := range extractFunctionBodies(s) {
		if blocked, reason := f.blockReason(body); blocked {
			return true, reason
		}
	}
	// Remove substitutions so they don't pollute first-token extraction,
	// then split on operators and check each segment.
	stripped := stripSubShells(s)
	for _, seg := range splitCommands(stripped) {
		// Check variable assignments for blacklisted values (e.g. x=rm).
		if f.hasBlockedAssignment(seg) {
			return true, "blacklisted command in variable assignment"
		}
		name := effectiveCommand(seg)
		if name != "" && f.isBlacklisted(name) {
			return true, "blacklisted command: " + name
		}
		if hasIndirectExecutor(seg) {
			return true, "blacklisted indirect executor"
		}
		// Whitelist mode: when configured, any segment whose effective command
		// is not a structural token and does not match a whitelisted prefix
		// requires approval (i.e. is blocked). Structural tokens (braces) and
		// empty names (pure variable assignments) are exempt.
		if len(f.whitelist) > 0 && name != "" && name != "{" && name != "}" && !f.matchesWhitelist(seg) {
			return true, "command not in whitelist (approval required): " + name
		}
	}
	return false, ""
}

func (f CommandFilter) isBlacklisted(name string) bool {
	for _, b := range f.blacklist {
		if name == b {
			return true
		}
	}
	return false
}

// containsHeredoc reports whether the command string contains a heredoc
// redirection (<< or <<-). Heredocs can write arbitrary content to any
// file path and are therefore blocked in the sandbox.
func containsHeredoc(s string) bool {
	return strings.Contains(s, "<<")
}

// resolveSymlinks resolves symbolic links in path. Unlike filepath.EvalSymlinks,
// which fails when any component of the path does not exist, resolveSymlinks
// walks up the path to find the longest existing prefix, resolves it, and
// re-appends the non-existent suffix. This ensures consistent comparison
// even when the workDir or whitelist base has not been created yet.
func resolveSymlinks(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	// Walk up to the longest existing ancestor, resolve it, then
	// re-append the non-existent suffix.
	dir := filepath.Dir(path)
	suffix := filepath.Base(path)
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, suffix)
		}
		if dir == "/" || dir == "." || dir == filepath.Dir(dir) {
			return path
		}
		suffix = filepath.Join(filepath.Base(dir), suffix)
		dir = filepath.Dir(dir)
	}
}

// DefaultBashSandbox implements BashSandbox using a PathWhitelist, a
// CommandFilter, and optional resource limits.
type DefaultBashSandbox struct {
	whitelist PathWhitelist
	filter    CommandFilter
	maxCPU    time.Duration
	maxMemory int64
}

var _ BashSandbox = (*DefaultBashSandbox)(nil)

// SandboxOption configures a DefaultBashSandbox.
type SandboxOption func(*DefaultBashSandbox)

// WithWhitelist sets the allowed base paths for the sandbox. An empty slice
// allows all paths (no restriction).
func WithWhitelist(paths []string) SandboxOption {
	return func(s *DefaultBashSandbox) { s.whitelist = NewPathWhitelist(paths) }
}

// WithAllowedPaths sets the allowed base paths for the sandbox. When paths is
// empty, the current working directory is used as a safe default rather than
// allowing all paths.
func WithAllowedPaths(paths []string) SandboxOption {
	return func(s *DefaultBashSandbox) {
		if len(paths) == 0 {
			cwd, err := os.Getwd()
			if err != nil {
				cwd = "."
			}
			paths = []string{cwd}
		}
		s.whitelist = NewPathWhitelist(paths)
	}
}

// WithBlacklist sets the disallowed command names for the sandbox.
func WithBlacklist(blacklist []string) SandboxOption {
	return func(s *DefaultBashSandbox) { s.filter = NewCommandFilter(blacklist) }
}

// WithCommandWhitelist enables whitelist mode for the sandbox. When set, only
// commands whose leading tokens match one of the given prefixes are allowed
// without approval; everything else is blocked (requires approval). Blacklisted
// commands and indirect executors are always blocked regardless of the
// whitelist. An empty slice disables whitelist mode.
func WithCommandWhitelist(commands []string) SandboxOption {
	return func(s *DefaultBashSandbox) { s.filter.whitelist = commands }
}

// WithMaxCPU sets the maximum CPU duration allowed for a command.
func WithMaxCPU(d time.Duration) SandboxOption {
	return func(s *DefaultBashSandbox) { s.maxCPU = d }
}

// WithMaxMemory sets the maximum memory (in bytes) allowed for a command.
func WithMaxMemory(n int64) SandboxOption {
	return func(s *DefaultBashSandbox) { s.maxMemory = n }
}

// NewDefaultBashSandbox returns a sandbox with the default command blacklist
// and an empty whitelist (allowing all paths). Options may override either.
func NewDefaultBashSandbox(opts ...SandboxOption) *DefaultBashSandbox {
	s := &DefaultBashSandbox{
		whitelist: NewPathWhitelist(nil),
		filter:    NewCommandFilter(defaultCommandBlacklist),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Validate checks the working directory against the whitelist and the command
// against the blacklist/whitelist filter. It returns a descriptive error if any
// check fails.
func (s *DefaultBashSandbox) Validate(_ context.Context, cmd, workDir string) error {
	if !s.whitelist.IsAllowed(workDir) {
		return fmt.Errorf("bash sandbox: workdir %q is not in the path whitelist", workDir)
	}
	if reason := s.filter.BlockReason(cmd); reason != "" {
		return fmt.Errorf("bash sandbox: command blocked (%s): %s", reason, cmd)
	}
	return nil
}

// --- command parsing helpers (no third-party deps) ---

// subShellRange describes a command substitution expression within a command
// string, either a $(...) form or a `...` (backtick) form.
type subShellRange struct {
	start      int // index of opening char ('$' or '`')
	innerStart int // index after the opening ('$(' or '`')
	innerEnd   int // index of the closing char (')' or '`')
	end        int // index after the closing char
}

// findSubShells returns the ranges of all top-level command substitution
// expressions in s, covering both $(...) and `...` (backtick) forms. Nested
// substitutions are reported only at the top level; their inner content is
// inspected recursively by callers.
func findSubShells(s string) []subShellRange {
	var ranges []subShellRange
	i := 0
	for i < len(s) {
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '(' {
			depth := 1
			innerStart := i + 2
			j := innerStart
			for j < len(s) && depth > 0 {
				if j+1 < len(s) && s[j] == '$' && s[j+1] == '(' {
					depth++
					j += 2
				} else if s[j] == ')' {
					depth--
					if depth == 0 {
						break
					}
					j++
				} else {
					j++
				}
			}
			if depth == 0 {
				ranges = append(ranges, subShellRange{
					start:      i,
					innerStart: innerStart,
					innerEnd:   j,
					end:        j + 1,
				})
				i = j + 1
			} else {
				i++ // unmatched '('; skip
			}
		} else if s[i] == '`' {
			// Backtick command substitution: scan to the next unescaped
			// backtick. Escaped characters (\x) are skipped so that a
			// backslash-escaped backtick does not terminate the substitution
			// prematurely.
			innerStart := i + 1
			j := innerStart
			closed := false
			for j < len(s) {
				if s[j] == '\\' && j+1 < len(s) {
					j += 2
					continue
				}
				if s[j] == '`' {
					closed = true
					break
				}
				j++
			}
			if closed {
				ranges = append(ranges, subShellRange{
					start:      i,
					innerStart: innerStart,
					innerEnd:   j,
					end:        j + 1,
				})
				i = j + 1
			} else {
				i++ // unmatched backtick; skip
			}
		} else {
			i++
		}
	}
	return ranges
}

// extractSubShells returns the inner command strings of all command
// substitution expressions ($(...) and `...`).
func extractSubShells(s string) []string {
	ranges := findSubShells(s)
	subs := make([]string, 0, len(ranges))
	for _, r := range ranges {
		subs = append(subs, s[r.innerStart:r.innerEnd])
	}
	return subs
}

// stripSubShells replaces every command substitution expression ($(...) and
// `...`) with a single space.
func stripSubShells(s string) string {
	ranges := findSubShells(s)
	if len(ranges) == 0 {
		return s
	}
	var b strings.Builder
	prev := 0
	for _, r := range ranges {
		b.WriteString(s[prev:r.start])
		b.WriteByte(' ')
		prev = r.end
	}
	b.WriteString(s[prev:])
	return b.String()
}

// splitCommands splits a command string on command separators (;, &, newlines),
// pipes (|), and logical operators (&& and ||), returning the trimmed non-empty
// segments. The & operator is handled after && so that logical-AND is not
// broken into two background operators.
func splitCommands(s string) []string {
	const delim = "\x00"
	s = strings.ReplaceAll(s, "&&", delim)
	s = strings.ReplaceAll(s, "||", delim)
	s = strings.ReplaceAll(s, "|", delim)
	s = strings.ReplaceAll(s, ";", delim)
	s = strings.ReplaceAll(s, "&", delim)
	s = strings.ReplaceAll(s, "\n", delim)
	parts := strings.Split(s, delim)
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// firstToken returns the first whitespace-delimited token of s, or "" if s is
// blank.
func firstToken(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// effectiveCommand returns the effective command name from a segment, skipping
// known prefix/wrapper commands like sudo, doas, etc. This prevents blacklist
// bypass via "sudo rm" where the first token "sudo" is not blacklisted but the
// actual command "rm" is. The command token is normalized for shell
// quotes/escapes/function definitions (so 'rm', "rm", $'rm', \rm and rm(){}
// all reduce to "rm") and filepath.Base is applied so that path-prefixed
// invocations like "/usr/bin/rm" are reduced to "rm" before the blacklist check.
func effectiveCommand(s string) string {
	cmd, _ := effectiveCommandAndArgs(strings.Fields(s))
	return cmd
}

// effectiveCommandAndArgs returns the effective command name and the argument
// tokens that follow it. It skips leading variable assignments (FOO=bar) and
// known prefix/wrapper commands (sudo, doas, function, ...). The command token
// is normalized via normalizeCommandToken before filepath.Base is applied.
func effectiveCommandAndArgs(fields []string) (cmd string, args []string) {
	for i, f := range fields {
		if isVariableAssignment(f) {
			continue
		}
		base := filepath.Base(normalizeCommandToken(f))
		if commandPrefixes[base] {
			continue
		}
		return base, fields[i+1:]
	}
	return "", nil
}

// normalizeCommandToken reduces a single command token to its bare command name
// by stripping shell quoting and escape characters and by recognizing function
// definitions. For example 'rm', "rm", $'rm', \rm and rm(){} all reduce to
// "rm".
func normalizeCommandToken(token string) string {
	token = stripQuotes(token)
	if name := extractFunctionName(token); name != "" {
		return name
	}
	return token
}

// stripQuotes removes surrounding shell quoting from a token: single quotes
// ('...'), double quotes ("..."), ANSI-C dollar quotes ($'...'), and leading
// backslash escapes (\rm -> rm). Unterminated quotes are left untouched.
func stripQuotes(token string) string {
	if len(token) == 0 {
		return token
	}
	// $'...' ANSI-C quoting.
	if strings.HasPrefix(token, "$'") && strings.HasSuffix(token, "'") && len(token) >= 3 {
		return token[2 : len(token)-1]
	}
	// '...' single quote.
	if token[0] == '\'' && len(token) >= 2 && token[len(token)-1] == '\'' {
		return token[1 : len(token)-1]
	}
	// "..." double quote.
	if token[0] == '"' && len(token) >= 2 && token[len(token)-1] == '"' {
		return token[1 : len(token)-1]
	}
	// Leading backslash escapes (e.g. \rm or \\rm) -> rm.
	if strings.HasPrefix(token, "\\") {
		return strings.TrimLeft(token, "\\")
	}
	return token
}

// funcNameRe matches a shell function definition header "name()" at the start
// of a token, capturing the function name.
var funcNameRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*)\s*\(\s*\)`)

// extractFunctionName returns the function name when token begins with a
// "name()" definition header (e.g. "rm(){}" -> "rm"), or "" otherwise.
func extractFunctionName(token string) string {
	if m := funcNameRe.FindStringSubmatch(token); m != nil {
		return m[1]
	}
	return ""
}

// funcBodyRe matches the opening of a function definition body: either
// "name() {" or "function name {". The brace that opens the body is the last
// character matched.
var funcBodyRe = regexp.MustCompile(`(?:[A-Za-z_][A-Za-z0-9_-]*\s*\(\s*\)|function\s+[A-Za-z_][A-Za-z0-9_-]*)\s*\{`)

// extractFunctionBodies finds shell function definitions in s and returns their
// inner body strings (the text between the matching braces) so that callers can
// recursively inspect them for blocked commands. Brace matching is balanced.
func extractFunctionBodies(s string) []string {
	locs := funcBodyRe.FindAllStringIndex(s, -1)
	var bodies []string
	for _, loc := range locs {
		bodyStart := loc[1] // index just after '{'
		depth := 1
		i := bodyStart
		for i < len(s) && depth > 0 {
			switch s[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					bodies = append(bodies, s[bodyStart:i])
				}
			}
			i++
		}
	}
	return bodies
}

// isVariableAssignment reports whether token looks like a shell variable
// assignment (NAME=value). A leading name must consist of alphanumeric
// characters and underscores, starting with a letter or underscore.
func isVariableAssignment(token string) bool {
	idx := strings.Index(token, "=")
	if idx <= 0 {
		return false
	}
	name := token[:idx]
	for i, r := range name {
		if i == 0 && !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// hasBlockedAssignment checks whether any variable assignment in the segment
// has a blacklisted command as its value. This prevents bypasses like
// "x=rm; $x file" where a blacklisted command is assigned to a variable and
// then invoked indirectly.
func (f CommandFilter) hasBlockedAssignment(seg string) bool {
	fields := strings.Fields(seg)
	for _, field := range fields {
		if !isVariableAssignment(field) {
			continue
		}
		idx := strings.Index(field, "=")
		value := field[idx+1:]
		// Normalize shell quoting/escapes so that x="rm", x='rm', x=$'rm',
		// and x=\rm are all reduced to "rm" before the blacklist check.
		value = filepath.Base(normalizeCommandToken(value))
		if f.isBlacklisted(value) {
			return true
		}
	}
	return false
}

// systemCallRe matches a call to the system() function inside interpreters
// like awk/gawk/mawk, which can execute arbitrary shell commands.
var systemCallRe = regexp.MustCompile(`\bsystem\s*\(`)

// hasIndirectExecutor reports whether seg uses an indirect executor that can
// run arbitrary commands, bypassing the first-token blacklist. This covers:
//   - awk/gawk/mawk with a system() call
//   - find -exec / -execdir
//   - make -f / --file / --makefile
//   - git -c / --config
func hasIndirectExecutor(seg string) bool {
	fields := strings.Fields(seg)
	if len(fields) == 0 {
		return false
	}
	cmd := filepath.Base(normalizeCommandToken(fields[0]))
	switch cmd {
	case "awk", "gawk", "mawk":
		return systemCallRe.MatchString(seg)
	case "find":
		return containsAny(fields, "-exec", "-execdir")
	case "make":
		return containsAny(fields, "-f", "--file", "--makefile")
	case "git":
		return containsAny(fields, "-c", "--config")
	}
	return false
}

// containsAny reports whether any element of fields equals any of the given
// values.
func containsAny(fields []string, values ...string) bool {
	for _, f := range fields {
		for _, v := range values {
			if f == v {
				return true
			}
		}
	}
	return false
}

// matchesWhitelist reports whether seg's leading tokens match at least one
// entry in the whitelist. Each whitelist entry is tokenized and compared as a
// prefix: "go test" matches "go test ./..." but not "go run main.go". When the
// whitelist is empty, whitelist mode is disabled and this method is not called
// by blockReason.
func (f CommandFilter) matchesWhitelist(seg string) bool {
	cmd, args := effectiveCommandAndArgs(strings.Fields(seg))
	if cmd == "" {
		return false
	}
	leading := append([]string{cmd}, args...)
	for _, entry := range f.whitelist {
		if prefixTokens(strings.Fields(entry), leading) {
			return true
		}
	}
	return false
}

// prefixTokens reports whether leading begins with all tokens of prefix (in
// order). An empty prefix matches everything.
func prefixTokens(prefix, leading []string) bool {
	if len(prefix) > len(leading) {
		return false
	}
	for i, p := range prefix {
		if p != leading[i] {
			return false
		}
	}
	return true
}
