package approval

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tools"
)

var errApprovalCacheBoundToDifferentFence = errors.New("approval: cache is bound to a different dispatch fence")

type approvalGrantPublisher interface {
	PublishApprovalGrant(tools.DispatchFenceSnapshot, func()) (tools.DispatchFenceSnapshot, error)
}

func publishApprovalGrant(fence *tools.DispatchFence, expected tools.DispatchFenceSnapshot, update func()) (tools.DispatchFenceSnapshot, error) {
	publisher, ok := any(fence).(approvalGrantPublisher)
	if !ok || fence == nil {
		return tools.DispatchFenceSnapshot{}, tools.ErrDispatchFenceUnavailable
	}
	return publisher.PublishApprovalGrant(expected, update)
}

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
func (c *InteractiveApprovalCallback) RequestApproval(ctx context.Context, toolName string, args map[string]any) (ApprovalResult, error) {
	result, _, _, err := c.requestApprovalWithChoice(ctx, toolName, args)
	return result, err
}

func (c *InteractiveApprovalCallback) requestApprovalWithChoice(_ context.Context, toolName string, _ map[string]any) (ApprovalResult, string, string, error) {
	if !isTerminalReader(c.stdin) {
		return ApprovalDeny, "", "non_tty_deny", nil
	}

	fmt.Fprintf(c.stdout, "[approval] Tool '%s' wants to execute. Allow? (y/n/a): ", toolName) //nolint:errcheck

	reader := bufio.NewReader(c.stdin)
	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return ApprovalDeny, "", "callback_error", fmt.Errorf("read approval input: %w", err)
		}
		answer := strings.TrimSpace(strings.ToLower(line))
		switch answer {
		case "y", "yes":
			return ApprovalAllow, "allow_once", "user", nil
		case "n", "no":
			return ApprovalDeny, "deny", "user", nil
		case "a", "always":
			return ApprovalAlwaysAllow, "allow_always", "user", nil
		}
		if err == io.EOF {
			return ApprovalDeny, "", "input_ended", nil
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

// DiffPreviewFunc generates a unified diff preview string for a tool call. It
// returns "" when no preview is available (e.g. for non-file tools). The
// function is called from TeaApprovalCallback before sending the request to the
// TUI so the user can see the proposed change before approving.
type DiffPreviewFunc func(ctx context.Context, toolName string, args map[string]any) string

// TeaApprovalCallback sends approval requests to the TUI via a channel instead
// of blocking on stdin readline. It implements ApprovalCallback so it can be a
// drop-in replacement for InteractiveApprovalCallback: the middleware calls
// RequestApproval, which sends a InteractiveRequest on the channel and blocks
// until the TUI delivers a InteractiveResponse (or the context is canceled).
type TeaApprovalCallback struct {
	requestCh     chan<- InteractiveRequest
	diffPreviewFn DiffPreviewFunc
}

var _ ApprovalCallback = (*TeaApprovalCallback)(nil)

// NewTeaApprovalCallback builds a TeaApprovalCallback that forwards approval
// requests to the given channel. The channel is typically shared with the
// BubbleteaApp's approval listener (see tui.WithApprovalChannel). An optional
// diffPreviewFn generates a unified diff for edit/write tool calls.
func NewTeaApprovalCallback(ch chan<- InteractiveRequest, opts ...TeaApprovalOption) *TeaApprovalCallback {
	c := &TeaApprovalCallback{requestCh: ch}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// TeaApprovalOption configures a TeaApprovalCallback at construction time.
type TeaApprovalOption func(*TeaApprovalCallback)

// WithDiffPreviewFunc sets the function used to generate diff previews for
// edit/write tool calls.
func WithDiffPreviewFunc(fn DiffPreviewFunc) TeaApprovalOption {
	return func(c *TeaApprovalCallback) { c.diffPreviewFn = fn }
}

// RequestApproval sends an interactive request and blocks until a response.
// If the context is canceled first, the call is denied with the context error.
// When configured, a diff preview is included for edit/write tools.
func (c *TeaApprovalCallback) RequestApproval(ctx context.Context, toolName string, args map[string]any) (ApprovalResult, error) {
	result, _, _, err := c.requestApprovalWithChoice(ctx, toolName, args)
	return result, err
}

func (c *TeaApprovalCallback) requestApprovalWithChoice(ctx context.Context, toolName string, args map[string]any) (ApprovalResult, string, string, error) {
	respCh := make(chan InteractiveResponse, 1)
	req := InteractiveRequest{
		ToolName:   toolName,
		Args:       args,
		ResponseCh: respCh,
		Context:    ctx,
	}
	if c.diffPreviewFn != nil {
		req.DiffPreview = c.diffPreviewFn(ctx, toolName, args)
	}
	select {
	case c.requestCh <- req:
	case <-ctx.Done():
		return ApprovalDeny, "", "canceled", ctx.Err()
	}
	select {
	case resp := <-respCh:
		result, err := approvalResultFromResponse(ctx, resp)
		if err != nil {
			return ApprovalDeny, "", "canceled", err
		}
		choice := ""
		switch resp.Decision {
		case InteractiveAllow:
			choice = "allow_once"
		case InteractiveAlwaysAllow:
			choice = "allow_always"
		case InteractiveDeny:
			choice = "deny"
		default:
			return ApprovalDeny, "", "invalid_response", nil
		}
		switch resp.DecisionSource {
		case "user":
			return result, choice, "user", nil
		case "canceled":
			return ApprovalDeny, "", "canceled", nil
		default:
			return result, choice, "callback", nil
		}
	case <-ctx.Done():
		return ApprovalDeny, "", "canceled", ctx.Err()
	}
}

func approvalResultFromResponse(ctx context.Context, resp InteractiveResponse) (ApprovalResult, error) {
	if err := ctx.Err(); err != nil {
		return ApprovalDeny, err
	}
	switch resp.Decision {
	case InteractiveAllow:
		return ApprovalAllow, nil
	case InteractiveAlwaysAllow:
		return ApprovalAlwaysAllow, nil
	default:
		return ApprovalDeny, nil
	}
}

// ApprovalCache persists always-allow decisions so subsequent identical calls
// skip the interactive prompt. It is a concurrency-safe map of decision keys
// (format "mode:toolName:argsHash", matching sessionKey in middleware.go) to a
// boolean allowed flag. The cache can be loaded from and saved to a JSON file.
type ApprovalCache struct {
	mu      sync.RWMutex
	path    string
	entries map[string]bool // key -> allowed
	fence   *tools.DispatchFence
	epoch   uint64
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
// missing file leaves the cache empty and returns nil. For security, the file
// must have 0600 permissions and be owned by the current user; otherwise the
// load is rejected to prevent tampering via world-readable or foreign-owned
// cache files. Replacing a cache after it already contains grants publishes an
// approval-policy revision so prepared invocations captured before the
// replacement become stale.
func (c *ApprovalCache) LoadFromFile(path string) error {
	entries := make(map[string]bool)
	var loadErr error
	info, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			loadErr = fmt.Errorf("stat approval cache: %w", err)
		}
	} else {
		// Verify file permissions: only the owner may read or write the cache.
		if perm := info.Mode().Perm(); perm != 0o600 {
			slog.Warn("approval.cache_insecure_permissions", "path", path, "perm", fmt.Sprintf("%o", perm))
			loadErr = fmt.Errorf("approval cache %s has permissions %o, expected 0600", path, perm)
		}

		// Reuse the platform owner check; unsupported platforms reject rather
		// than treating an unverifiable cache as a grant of authority.
		if loadErr == nil {
			loadErr = validateFileOwner(info, path)
		}

		if loadErr == nil {
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				loadErr = fmt.Errorf("read approval cache: %w", readErr)
			} else if unmarshalErr := json.Unmarshal(data, &entries); unmarshalErr != nil {
				loadErr = fmt.Errorf("unmarshal approval cache: %w", unmarshalErr)
			}
		}
	}

	for {
		c.mu.RLock()
		fence := c.fence
		c.mu.RUnlock()

		if fence == nil {
			c.mu.Lock()
			if c.fence != nil {
				c.mu.Unlock()
				continue
			}
			c.replaceLocked(path, entries)
			c.mu.Unlock()
			return loadErr
		}

		_, publishErr := fence.Publish(tools.DispatchFenceApproval, nil, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.replaceLocked(path, entries)
		})
		if publishErr != nil {
			return errors.Join(loadErr, fmt.Errorf("publish approval cache replacement: %w", publishErr))
		}
		return loadErr
	}
}

func (c *ApprovalCache) replaceLocked(path string, entries map[string]bool) {
	if len(c.entries) > 0 {
		c.epoch++
	}
	c.path = path
	c.entries = entries
}

// SaveToFile writes the cache entries as JSON to the given path with 0600
// permissions (owner read/write only). If the file already exists with broader
// permissions, they are tightened to 0600 to prevent other users from reading
// or tampering with the cache.
func (c *ApprovalCache) SaveToFile(path string) error {
	c.mu.RLock()
	entries := make(map[string]bool, len(c.entries))
	for key, allowed := range c.entries {
		entries[key] = allowed
	}
	c.mu.RUnlock()

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal approval cache: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create approval cache file: %w", err)
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close() //nolint:errcheck
		return fmt.Errorf("write approval cache: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close approval cache: %w", err)
	}

	// Ensure 0600 even if the file already existed with broader permissions
	// (OpenFile does not change permissions of an existing file).
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("set approval cache permissions: %w", err)
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
	for {
		c.mu.RLock()
		fence := c.fence
		c.mu.RUnlock()
		if fence == nil {
			c.mu.Lock()
			if c.fence != nil {
				c.mu.Unlock()
				continue
			}
			c.entries[key] = true
			c.mu.Unlock()
			return
		}
		if _, err := c.publishPreparedGrant(fence, fence.Snapshot(), key, nil); err != nil {
			slog.Warn("approval.cache_grant_failed", "error", err)
		}
		return
	}
}

func (c *ApprovalCache) bindFence(fence *tools.DispatchFence) error {
	if fence == nil {
		return tools.ErrDispatchFenceUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fence != nil && c.fence != fence {
		return errApprovalCacheBoundToDifferentFence
	}
	c.fence = fence
	return nil
}

func (c *ApprovalCache) publishPreparedGrant(fence *tools.DispatchFence, expected tools.DispatchFenceSnapshot, key string, after func()) (tools.DispatchFenceSnapshot, error) {
	c.mu.RLock()
	bound := c.fence
	c.mu.RUnlock()
	if bound != fence {
		return tools.DispatchFenceSnapshot{}, errApprovalCacheBoundToDifferentFence
	}
	return publishApprovalGrant(fence, expected, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.setPreparedGrantLocked(key)
		if after != nil {
			after()
		}
	})
}

func (c *ApprovalCache) setPreparedGrantLocked(key string) {
	c.entries[key] = true
}

func (c *ApprovalCache) policyEpoch() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.epoch
}
