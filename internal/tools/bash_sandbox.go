package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultCommandBlacklist lists commands that are too destructive to allow
// inside a sandboxed bash tool.
var defaultCommandBlacklist = []string{
	"rm", "rmdir", "dd", "mkfs", "fdisk",
	"shutdown", "reboot", "halt", "poweroff",
	"kill", "killall", "pkill",
}

// BashSandbox validates bash commands before execution. Implementations check
// constraints such as allowed working directories and blocked commands.
type BashSandbox interface {
	Validate(ctx context.Context, cmd string, workDir string) error
}

// PathWhitelist checks if a working directory falls under one of the allowed
// base paths. An empty whitelist allows any path.
type PathWhitelist struct {
	paths []string
}

// NewPathWhitelist builds a PathWhitelist from the given base paths. Each path
// is cleaned with filepath.Clean before storage.
func NewPathWhitelist(paths []string) PathWhitelist {
	cleaned := make([]string, 0, len(paths))
	for _, p := range paths {
		cleaned = append(cleaned, filepath.Clean(p))
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
	cleaned := filepath.Clean(workDir)
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
// disallowed command names. It understands pipes (|), logical operators
// (&& and ||), and command substitution $(...).
type CommandFilter struct {
	blacklist []string
}

// NewCommandFilter builds a CommandFilter from the given blacklist.
func NewCommandFilter(blacklist []string) CommandFilter {
	return CommandFilter{blacklist: blacklist}
}

// IsBlocked reports whether any command referenced by cmd is on the blacklist.
// The command string is split on pipes and logical operators and each segment's
// first token is checked. Command substitutions $(...) are recursively
// inspected.
func (f CommandFilter) IsBlocked(cmd string) bool {
	return f.hasBlocked(cmd)
}

func (f CommandFilter) hasBlocked(s string) bool {
	// Recursively inspect command substitutions.
	for _, inner := range extractSubShells(s) {
		if f.hasBlocked(inner) {
			return true
		}
	}
	// Remove substitutions so they don't pollute first-token extraction,
	// then split on operators and check each segment.
	stripped := stripSubShells(s)
	for _, seg := range splitCommands(stripped) {
		name := firstToken(seg)
		if name == "" {
			continue
		}
		if f.isBlacklisted(name) {
			return true
		}
	}
	return false
}

func (f CommandFilter) isBlacklisted(name string) bool {
	for _, b := range f.blacklist {
		if name == b {
			return true
		}
	}
	return false
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
// against the blacklist. It returns a descriptive error if any check fails.
func (s *DefaultBashSandbox) Validate(_ context.Context, cmd, workDir string) error {
	if !s.whitelist.IsAllowed(workDir) {
		return fmt.Errorf("bash sandbox: workdir %q is not in the path whitelist", workDir)
	}
	if s.filter.IsBlocked(cmd) {
		return fmt.Errorf("bash sandbox: command contains a blacklisted command: %s", cmd)
	}
	return nil
}

// --- command parsing helpers (no third-party deps) ---

// subShellRange describes a $(...) expression within a command string.
type subShellRange struct {
	start      int // index of '$'
	innerStart int // index after '$('
	innerEnd   int // index of matching ')'
	end        int // index after ')'
}

// findSubShells returns the ranges of all top-level $(...) expressions in s.
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
		} else {
			i++
		}
	}
	return ranges
}

// extractSubShells returns the inner command strings of all $(...) expressions.
func extractSubShells(s string) []string {
	ranges := findSubShells(s)
	subs := make([]string, 0, len(ranges))
	for _, r := range ranges {
		subs = append(subs, s[r.innerStart:r.innerEnd])
	}
	return subs
}

// stripSubShells replaces every $(...) expression with a single space.
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

// splitCommands splits a command string on pipes (|) and logical operators
// (&& and ||), returning the trimmed non-empty segments.
func splitCommands(s string) []string {
	const delim = "\x00"
	s = strings.ReplaceAll(s, "&&", delim)
	s = strings.ReplaceAll(s, "||", delim)
	s = strings.ReplaceAll(s, "|", delim)
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
