package approval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// ApprovalResult represents the user's decision.
type ApprovalResult int

const (
	// ApprovalDeny denies the tool call.
	ApprovalDeny ApprovalResult = iota // 0 - deny
	// ApprovalAllow allows the tool call once.
	ApprovalAllow // 1 - allow once
	// ApprovalAlwaysAllow allows the tool call and persists the decision to the
	// approval cache so subsequent identical calls skip the prompt.
	ApprovalAlwaysAllow // 2 - allow always (persist to cache)
)

// ApprovalCallback handles interactive approval requests. When the classifier
// returns Ask and a callback is wired into the middleware, the callback is
// consulted to resolve the decision via interactive y/n/a confirmation.
type ApprovalCallback interface {
	RequestApproval(ctx context.Context, toolName string, args map[string]any) (ApprovalResult, error)
}

// InteractiveApprovalCallback reads y/n/a from stdin. It prints a prompt to
// stdout and reads a single line from stdin: "y" allows once, "n" denies, and
// "a" allows and persists the decision. When stdin is not a terminal the call
// is denied without blocking so piped input never stalls the middleware.
type InteractiveApprovalCallback struct {
	stdin  io.Reader
	stdout io.Writer
}

var _ ApprovalCallback = (*InteractiveApprovalCallback)(nil)

// NewInteractiveApprovalCallback builds an InteractiveApprovalCallback that
// reads confirmation input from stdin and writes prompts to stdout.
func NewInteractiveApprovalCallback(stdin io.Reader, stdout io.Writer) *InteractiveApprovalCallback {
	return &InteractiveApprovalCallback{stdin: stdin, stdout: stdout}
}

// RequestApproval prints a y/n/a prompt and reads a line from stdin. "y" allows
// the call once, "n" denies it, and "a" allows it and persists the decision.
// When stdin is not a terminal (non-TTY) the call is denied without blocking.
func (c *InteractiveApprovalCallback) RequestApproval(_ context.Context, toolName string, _ map[string]any) (ApprovalResult, error) {
	if !isTerminalReader(c.stdin) {
		return ApprovalDeny, nil
	}

	fmt.Fprintf(c.stdout, "[approval] Tool '%s' wants to execute. Allow? (y/n/a): ", toolName) //nolint:errcheck

	reader := bufio.NewReader(c.stdin)
	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return ApprovalDeny, fmt.Errorf("read approval input: %w", err)
		}
		answer := strings.TrimSpace(strings.ToLower(line))
		switch answer {
		case "y", "yes":
			return ApprovalAllow, nil
		case "n", "no":
			return ApprovalDeny, nil
		case "a", "always":
			return ApprovalAlwaysAllow, nil
		}
		if err == io.EOF {
			return ApprovalDeny, nil
		}
		fmt.Fprintf(c.stdout, "[approval] Invalid input. Allow? (y/n/a): ") //nolint:errcheck
	}
}

// isTerminalReader reports whether r is an interactive terminal. Non-file
// readers (e.g. strings.Reader in tests) are treated as terminals so the
// prompt/read path is exercised; *os.File readers are checked via stat.
func isTerminalReader(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return true
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// ApprovalCache persists always-allow decisions so subsequent identical calls
// skip the interactive prompt. It is a concurrency-safe map of decision keys
// (format "toolName:argsHash", matching sessionKey in middleware.go) to a
// boolean allowed flag. The cache can be loaded from and saved to a JSON file.
type ApprovalCache struct {
	mu      sync.RWMutex
	path    string
	entries map[string]bool // key -> allowed
}

// NewApprovalCache builds an ApprovalCache backed by the given file path. The
// cache starts empty; call LoadFromFile to hydrate it from disk.
func NewApprovalCache(path string) *ApprovalCache {
	return &ApprovalCache{
		path:    path,
		entries: make(map[string]bool),
	}
}

// LoadFromFile reads the cache entries from a JSON file mapping key->true. A
// missing file leaves the cache empty and returns nil.
func (c *ApprovalCache) LoadFromFile(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.path = path
	c.entries = make(map[string]bool)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read approval cache: %w", err)
	}
	if err := json.Unmarshal(data, &c.entries); err != nil {
		return fmt.Errorf("unmarshal approval cache: %w", err)
	}
	return nil
}

// SaveToFile writes the cache entries as JSON to the given path.
func (c *ApprovalCache) SaveToFile(path string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

	data, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal approval cache: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write approval cache: %w", err)
	}
	return nil
}

// Get returns whether the key is always-allowed and whether it was found.
func (c *ApprovalCache) Get(key string) (bool, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.entries[key]
	return v, ok
}

// Set marks the key as always-allowed.
func (c *ApprovalCache) Set(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = true
}
