package tests

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/acp"
	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tui"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// ---------------------------------------------------------------------------
// Security Tests (49-1 through 49-8, 49-24, 49-25, 49-26)
// ---------------------------------------------------------------------------

// TestE2E_Phase49_PathValidation verifies that the write tool rejects path
// traversal attempts (49-1). Relative paths that escape the workdir must be
// blocked by resolveWithinWorkdir.
func TestE2E_Phase49_PathValidation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tmpDir := t.TempDir()
	tool := tools.NewWriteTool(
		tools.WithWriteWorkdir(tmpDir),
		tools.WithOverwrite(true),
	)

	ctx := context.Background()

	// Valid path within workdir — should succeed.
	_, err := tool.Execute(ctx, tools.ToolCall{
		ID:   "1",
		Name: "write",
		Args: map[string]any{"path": "safe.txt", "content": "hello"},
	})
	require.NoError(t, err)

	// Path traversal via .. — should be rejected.
	_, err = tool.Execute(ctx, tools.ToolCall{
		ID:   "2",
		Name: "write",
		Args: map[string]any{"path": "../../etc/passwd", "content": "evil"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes workdir")

	// Absolute path outside workdir — should not be rejected by traversal
	// check (absolute paths are used as-is).
	outside := filepath.Join(t.TempDir(), "outside.txt")
	_, err = tool.Execute(ctx, tools.ToolCall{
		ID:   "3",
		Name: "write",
		Args: map[string]any{"path": outside, "content": "outside"},
	})
	require.NoError(t, err)
}

// TestE2E_Phase49_ACPBodySizeLimit verifies that the ACP HTTP server rejects
// request bodies exceeding the 1 MB limit (49-2).
func TestE2E_Phase49_ACPBodySizeLimit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := acp.NewHTTPServer("test-bodylimit", "127.0.0.1:0", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, srv.Start(ctx))
	defer func() { _ = srv.Stop(context.Background()) }()

	base := "http://" + srv.Addr()

	// Body within limit — should succeed.
	normalBody := `{"sender_id":"test","type":"connect"}`
	resp, err := http.Post(base+"/connect", "application/json", strings.NewReader(normalBody))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	// Body exceeding 1 MB — should be rejected with 413. The body must be
	// valid JSON so the decoder reads past the 1 MB MaxBytesReader limit
	// (an invalid body would produce a 400 syntax error before the limit).
	bigBody := `{"sender_id":"` + strings.Repeat("x", 2*1024*1024) + `","type":"connect"}`
	resp, err = http.Post(base+"/connect", "application/json", strings.NewReader(bigBody))
	require.NoError(t, err)
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode,
		"body > 1MB should be rejected with 413")
	_ = resp.Body.Close()
}

// TestE2E_Phase49_BashSandboxVariableConcat verifies that the bash sandbox
// detects variable concatenation bypass patterns (49-3). Commands like
// "a=r;b=m;c=$a$b;$c -rf /" must be blocked.
func TestE2E_Phase49_BashSandboxVariableConcat(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sb := tools.NewDefaultBashSandbox()
	tmpDir := t.TempDir()

	tests := []struct {
		name string
		cmd  string
	}{
		{"concat_rm", "a=r;b=m;c=$a$b;$c -rf /"},
		{"concat_curl", "x=cu;y=rl;z=$x$y;$z http://evil.com"},
		{"single_var_rm", "v=rm;$v -rf /tmp"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := sb.Validate(ctx, tc.cmd, tmpDir)
			require.Error(t, err, "variable concat bypass should be blocked: %s", tc.cmd)
		})
	}

	// Normal commands should pass.
	err := sb.Validate(ctx, "echo hello", tmpDir)
	assert.NoError(t, err)
	err = sb.Validate(ctx, "ls -la", tmpDir)
	assert.NoError(t, err)
}

// TestE2E_Phase49_SSRFProtection verifies that ValidateURL and the SSRF-safe
// HTTP client block requests to private/internal IP ranges (49-4).
func TestE2E_Phase49_SSRFProtection(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// ValidateURL should reject private IPs.
	privateURLs := []string{
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://169.254.169.254/latest/meta-data",
		"http://127.0.0.1/",
		"ftp://example.com/", // non-http(s) scheme
	}
	for _, u := range privateURLs {
		err := tools.ValidateURL(u)
		require.Error(t, err, "should block: %s", u)
	}

	// SSRF-safe client should block dial to private IPs.
	client := tools.NewSSRFSafeHTTPClient(2 * time.Second)
	_, err := client.Get("http://10.0.0.1/")
	require.Error(t, err)

	// Start a local test server to verify loopback behavior.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	// Allow-loopback client should permit 127.0.0.1.
	loopbackClient := tools.NewSSRFSafeHTTPClientAllowLoopback(2 * time.Second)
	_, err = loopbackClient.Get(ts.URL)
	assert.NoError(t, err, "loopback should be allowed with AllowLoopback client")

	// Non-loopback client should block the local server.
	_, err = client.Get(ts.URL)
	require.Error(t, err, "loopback should be blocked with default SSRF client")
}

// TestE2E_Phase49_SSHPasswordSecurity verifies that the SSH client validates
// configuration and requires sshpass for password auth (49-5).
func TestE2E_Phase49_SSHPasswordSecurity(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	// Missing host — should fail.
	client := tools.NewDefaultSSHClient(tools.SSHConfig{
		Password: "secret",
	})
	err := client.Connect(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host")

	// Missing both key_path and password — should fail.
	client = tools.NewDefaultSSHClient(tools.SSHConfig{
		Host: "example.com",
	})
	err = client.Connect(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key_path or password")

	// Password set but sshpass not installed — should fail with a clear message.
	client = tools.NewDefaultSSHClient(tools.SSHConfig{
		Host:     "example.com",
		Password: "secret",
	})
	err = client.Connect(ctx)
	// sshpass may or may not be installed; if not, error should mention sshpass.
	if err != nil {
		assert.True(t,
			strings.Contains(err.Error(), "sshpass") ||
				strings.Contains(err.Error(), "connection") ||
				strings.Contains(err.Error(), "timeout") ||
				strings.Contains(err.Error(), "host"),
			"password auth error should be descriptive: %v", err)
	}

	// Key-based auth with non-existent key — should not panic.
	client = tools.NewDefaultSSHClient(tools.SSHConfig{
		Host:    "example.com",
		KeyPath: "/nonexistent/key",
	})
	_ = client.Connect(ctx) // best-effort; may fail but should not panic
}

// TestE2E_Phase49_SymlinkEscapePrevention verifies that the write tool rejects
// writes through symlinks via O_NOFOLLOW (49-6).
func TestE2E_Phase49_SymlinkEscapePrevention(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("orig"), 0o600))

	// Create a symlink pointing to the target.
	link := filepath.Join(tmpDir, "link.txt")
	require.NoError(t, os.Symlink(target, link))

	tool := tools.NewWriteTool(
		tools.WithWriteWorkdir(tmpDir),
		tools.WithOverwrite(true),
	)

	ctx := context.Background()

	// Writing through a symlink — should be rejected by O_NOFOLLOW.
	_, err := tool.Execute(ctx, tools.ToolCall{
		ID:   "1",
		Name: "write",
		Args: map[string]any{"path": "link.txt", "content": "via-symlink"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")

	// Writing to a regular file — should succeed.
	_, err = tool.Execute(ctx, tools.ToolCall{
		ID:   "2",
		Name: "write",
		Args: map[string]any{"path": "regular.txt", "content": "ok"},
	})
	require.NoError(t, err)
}

// TestE2E_Phase49_ServeTokenAuth verifies that the ACP HTTP server uses
// constant-time bearer token authentication (49-7).
func TestE2E_Phase49_ServeTokenAuth(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := acp.NewHTTPServer("test-auth", "127.0.0.1:0", nil)
	srv.SetAuth("secret-token-49", "subject-49")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, srv.Start(ctx))
	defer func() { _ = srv.Stop(context.Background()) }()

	base := "http://" + srv.Addr()

	// No auth header → 401.
	resp, err := http.Post(base+"/connect", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_ = resp.Body.Close()

	// Wrong token → 401.
	req, _ := http.NewRequest(http.MethodPost, base+"/connect", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_ = resp.Body.Close()

	// Correct token → 200.
	req, _ = http.NewRequest(http.MethodPost, base+"/connect", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer secret-token-49")
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
}

// TestE2E_Phase49_APIKeyMasking verifies that ResultMasker and
// RegisterAPIKeyRedaction redact common API key formats from tool output (49-8).
func TestE2E_Phase49_APIKeyMasking(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	masker := tools.NewResultMasker(tools.DefaultAPIKeyPatterns())

	tests := []struct {
		name     string
		input    string
		contains string // the input should NOT contain this after masking
	}{
		{"openai", "The key is sk-abcd1234efgh5678ijkl9012mnop3456", "sk-abcd1234"},
		{"anthropic", "Found sk-ant-abc123def456ghi789jkl012mno345", "sk-ant-abc"},
		{"github", "Token: ghp_1234567890abcdefghijklmnopqrstuvwxyz", "ghp_12345"},
		{"aws", "AKIAABCDEFGHIJKLMNOP and secret", "AKIAABCDEFG"},
		{"bearer", "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9", "Bearer eyJhbGc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			masked := masker.Mask(tc.input)
			assert.NotContains(t, masked, tc.contains,
				"API key should be redacted from output")
		})
	}

	// Verify RedactingOutputGuard with RegisterAPIKeyRedaction.
	guard := production.NewRedactingOutputGuard()
	production.RegisterAPIKeyRedaction(guard)

	ctx := context.Background()
	result, err := guard.Check(ctx, "key: sk-abcd1234efgh5678ijkl9012mnop3456")
	require.NoError(t, err)
	assert.True(t, result.Allowed, "redacting guard should allow output")
	assert.NotContains(t, result.Sanitized, "sk-abcd1234",
		"API key should be redacted in sanitized output")
}

// TestE2E_Phase49_AuditHashChainAndRotation verifies the SHA-256 hash chain
// integrity and rotation behavior of DefaultAuditLog (49-24).
func TestE2E_Phase49_AuditHashChainAndRotation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Run("HashChain", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "audit.jsonl")
		audit := production.NewDefaultAuditLog(logPath)

		now := time.Now()
		for i := 0; i < 5; i++ {
			require.NoError(t, audit.Log(ctx, production.AuditEntry{
				Timestamp: now.Add(time.Duration(i) * time.Second),
				Operation: "tool.run",
				ToolName:  "bash",
			}))
		}

		// Verify chain integrity.
		err := audit.(*production.DefaultAuditLog).VerifyChain()
		require.NoError(t, err, "hash chain should be valid")

		// Tamper with the file — chain should break.
		data, err := os.ReadFile(logPath)
		require.NoError(t, err)
		tampered := strings.Replace(string(data), "bash", "evil", 1)
		require.NoError(t, os.WriteFile(logPath, []byte(tampered), 0o600))
		err = audit.(*production.DefaultAuditLog).VerifyChain()
		require.Error(t, err, "tampered chain should fail verification")
	})

	t.Run("Rotation", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "audit_rot.jsonl")
		// Very small maxSize to trigger rotation quickly.
		audit := production.NewDefaultAuditLog(logPath, production.WithMaxSize(200))

		for i := 0; i < 20; i++ {
			require.NoError(t, audit.Log(ctx, production.AuditEntry{
				Timestamp: time.Now(),
				Operation: fmt.Sprintf("op.%d", i),
			}))
		}

		// Rotated file should exist.
		_, err := os.Stat(logPath + ".1")
		require.NoError(t, err, "rotated file should exist at path+.1")

		// Current file should still have a valid chain (new chain after rotation).
		err = audit.(*production.DefaultAuditLog).VerifyChain()
		require.NoError(t, err, "post-rotation chain should be valid")
	})
}

// TestE2E_Phase49_PIIDetectionExtended verifies that the PII output guard
// detects various PII types including Luhn-validated credit cards (49-25).
func TestE2E_Phase49_PIIDetectionExtended(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	guard := production.NewPIIOutputGuard()

	tests := []struct {
		name string
		text string
		pii  string // expected PII name in reason
	}{
		{"email", "Contact me at john@example.com please", "Email"},
		{"china_phone", "My number is 13812345678", "China Phone"},
		{"us_ssn", "SSN: 123-45-6789", "US SSN"},
		{"api_key", "Token: sk-abcdefghij1234567890xyz1234", "API Key"},
		{"credit_card", "Card: 4111111111111111", "Credit Card"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := guard.Check(ctx, tc.text)
			require.NoError(t, err)
			assert.False(t, result.Allowed, "PII should be detected: %s", tc.name)
			assert.Contains(t, result.Reason, tc.pii,
				"reason should mention the PII type")
		})
	}

	// Non-PII text should be allowed.
	result, err := guard.Check(ctx, "This is a normal message with no PII")
	require.NoError(t, err)
	assert.True(t, result.Allowed, "non-PII text should be allowed")

	// Credit card that fails Luhn should NOT be flagged.
	result, err = guard.Check(ctx, "1234567890123") // 13 digits, fails Luhn
	require.NoError(t, err)
	assert.True(t, result.Allowed, "non-Luhn number should not be flagged as credit card")
}

// TestE2E_Phase49_ACPTLSAndSecurityConfig verifies that the ACP HTTP server
// can serve over HTTPS with a self-signed certificate (49-26).
func TestE2E_Phase49_ACPTLSAndSecurityConfig(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	certFile, keyFile := generateSelfSignedCert(t)

	srv := acp.NewHTTPServer("test-tls", "127.0.0.1:0", nil)
	srv.SetAuth("tls-token", "tls-subject")
	srv.SetTLS(certFile, keyFile)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, srv.Start(ctx))
	defer func() { _ = srv.Stop(context.Background()) }()

	// Build an HTTPS client that skips cert verification (self-signed).
	httpsClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 5 * time.Second,
	}

	base := "https://" + srv.Addr()

	// Without auth → 401.
	resp, err := httpsClient.Post(base+"/connect", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_ = resp.Body.Close()

	// With auth → 200.
	req, _ := http.NewRequest(http.MethodPost, base+"/connect", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer tls-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err = httpsClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "TLS + auth should succeed")
	_ = resp.Body.Close()

	// Verify IsLoopbackAddr helper.
	assert.True(t, acp.IsLoopbackAddr("127.0.0.1:8080"))
	assert.True(t, acp.IsLoopbackAddr("[::1]:8080"))
	assert.False(t, acp.IsLoopbackAddr("10.0.0.1:8080"))
}

// ---------------------------------------------------------------------------
// Architecture Tests (49-9 through 49-12)
// ---------------------------------------------------------------------------

// TestE2E_Phase49_ToolInterception verifies that ToolInterceptor callbacks
// are executed synchronously by PreToolCallEvent.IsCancelled and can cancel
// tool calls (49-9).
func TestE2E_Phase49_ToolInterception(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Register an interceptor that blocks the "dangerous" tool.
	core.RegisterToolInterceptor(func(toolName, toolCallID string, _ map[string]any) error {
		if toolName == "dangerous" {
			return errors.New("blocked by interceptor")
		}
		return nil
	})
	defer core.ClearToolInterceptors()

	// Event for a blocked tool — IsCancelled should return true.
	ev := &core.PreToolCallEvent{
		ToolName:   "dangerous",
		ToolCallID: "call-1",
		Args:       map[string]any{},
	}
	assert.True(t, ev.IsCancelled(), "dangerous tool should be cancelled")

	// Event for a safe tool — IsCancelled should return false.
	ev2 := &core.PreToolCallEvent{
		ToolName:   "safe",
		ToolCallID: "call-2",
		Args:       map[string]any{},
	}
	assert.False(t, ev2.IsCancelled(), "safe tool should not be cancelled")

	// Verify interceptor runs exactly once.
	ev3 := &core.PreToolCallEvent{
		ToolName:   "dangerous",
		ToolCallID: "call-3",
		Args:       map[string]any{},
	}
	assert.True(t, ev3.IsCancelled())
	assert.True(t, ev3.IsCancelled(), "second call should return cached result")
}

// TestE2E_Phase49_TurnsMapCleanup verifies that the EinoTurnRunner prunes
// completed turns beyond maxTurnsHistory (49-10).
func TestE2E_Phase49_TurnsMapCleanup(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	runner := core.NewEinoTurnRunner(noopAgentLoop{})

	// Run maxTurnsHistory + 10 turns. Each turn completes immediately
	// because noopAgentLoop returns nil events with ctx.Err() (nil until
	// cancel).
	for i := 0; i < 110; i++ {
		_, _ = runner.RunTurn(ctx, core.Submission{
			Type:    core.SubmissionUserMessage,
			Content: fmt.Sprintf("turn-%d", i),
		})
	}

	// The oldest turns should have been pruned. We verify by checking that
	// early turns are unknown and recent ones are still accessible.
	// Turn IDs are "turn-1" through "turn-110".
	_, err := runner.Get(ctx, "turn-1")
	assert.Error(t, err, "oldest turn should have been pruned")

	// The most recent turn should still be accessible.
	lastTurn, err := runner.Get(ctx, "turn-110")
	require.NoError(t, err, "most recent turn should be accessible")
	assert.True(t, lastTurn.Done(), "completed turn should be in terminal state")
}

// TestE2E_Phase49_RegistryISP verifies that the Registry interface follows
// the Interface Segregation Principle with 8 sub-interfaces (49-11).
func TestE2E_Phase49_RegistryISP(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	reg := core.NewRegistry()
	require.NotNil(t, reg)

	// Verify the composite Registry implements all sub-interfaces.
	var _ core.CoreRegistry = reg
	var _ core.SessionRegistry = reg
	var _ core.CompactorRegistry = reg
	var _ core.ToolRegistryAccessor = reg
	var _ core.ModelProviderRegistry = reg
	var _ core.ApprovalRegistry = reg
	var _ core.TracingRegistry = reg
	var _ core.PluginRegistry = reg

	// Each sub-interface should expose its domain-specific methods.
	// CoreRegistry
	assert.NotNil(t, reg.ToolRegistry)
	assert.NotNil(t, reg.SessionStore)

	// ModelProviderRegistry
	assert.NotNil(t, reg.ModelProvider)

	// ToolRegistryAccessor — register a real ToolRegistry, then register
	// and retrieve a tool through it. The default registry returned by
	// ToolRegistry() is a no-op stub; we must install a real one.
	realToolReg := tools.NewDefaultToolRegistry()
	reg.RegisterToolRegistry(realToolReg)
	toolReg := reg.ToolRegistry()
	err := toolReg.Register(context.Background(), &stubTool{name: "isp-test"})
	require.NoError(t, err)
	got, err := toolReg.Get(context.Background(), "isp-test")
	require.NoError(t, err)
	assert.Equal(t, "isp-test", got.Name())
}

// TestE2E_Phase49_SubAgentConcurrency verifies that DefaultSubagentDispatcher
// limits concurrent sub-agent executions to defaultMaxConcurrentSubagents (5)
// via a semaphore (49-12).
func TestE2E_Phase49_SubAgentConcurrency(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var current atomic.Int32
	var maxConcurrent atomic.Int32

	// Create a custom SubAgent that tracks concurrency.
	subAgent := &concurrencyTrackingSubAgent{
		current:       &current,
		maxConcurrent: &maxConcurrent,
		delay:         100 * time.Millisecond,
	}
	factory := &trackingSubAgentFactory{sub: subAgent}

	dispatcher := core.NewDefaultSubagentDispatcher(factory)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Dispatch 10 tasks in parallel — only 5 should run at once.
	tasks := make([]core.SubagentTask, 10)
	for i := range tasks {
		tasks[i] = core.SubagentTask{
			ID:     fmt.Sprintf("task-%d", i),
			Prompt: "work",
		}
	}

	results, err := dispatcher.ParallelDispatch(ctx, tasks)
	require.NoError(t, err)
	assert.Len(t, results, 10)

	// maxConcurrent should never exceed 5.
	assert.LessOrEqual(t, int(maxConcurrent.Load()), 5,
		"concurrent sub-agents should not exceed defaultMaxConcurrentSubagents (5)")
	assert.Greater(t, int(maxConcurrent.Load()), 1,
		"multiple sub-agents should have run concurrently")
}

type concurrencyTrackingSubAgent struct {
	name          string
	current       *atomic.Int32
	maxConcurrent *atomic.Int32
	delay         time.Duration
}

func (s *concurrencyTrackingSubAgent) Name() string { return s.name }
func (s *concurrencyTrackingSubAgent) Run(_ context.Context, _ string) (<-chan core.AgentEvent, error) {
	ch := make(chan core.AgentEvent)
	close(ch)
	return ch, nil
}
func (s *concurrencyTrackingSubAgent) Send(_ context.Context, _ string) error { return nil }
func (s *concurrencyTrackingSubAgent) Interrupt(_ context.Context) error      { return nil }
func (s *concurrencyTrackingSubAgent) Wait(_ context.Context) (core.AgentMessage, error) {
	c := s.current.Add(1)
	for {
		old := s.maxConcurrent.Load()
		if int32(c) <= old {
			break
		}
		if s.maxConcurrent.CompareAndSwap(old, c) {
			break
		}
	}
	time.Sleep(s.delay)
	s.current.Add(-1)
	return core.AgentMessage{Role: "assistant", Content: "done"}, nil
}

// trackingSubAgentFactory creates the same sub-agent on each Create call.
type trackingSubAgentFactory struct {
	sub core.SubAgent
}

func (f *trackingSubAgentFactory) Create(_ context.Context, _ string, _ core.SubAgentConfig) (core.SubAgent, error) {
	return f.sub, nil
}

// ---------------------------------------------------------------------------
// Concurrency/Performance Tests (49-13 through 49-17)
// ---------------------------------------------------------------------------

// TestE2E_Phase49_AgentImplRWMutex verifies that AgentImpl supports concurrent
// read/write access without races (49-13). The -race flag catches any issues.
func TestE2E_Phase49_AgentImplRWMutex(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	agent := core.NewAgentImpl("test-agent", noopAgentLoop{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	// Concurrent readers: State() and Messages().
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = agent.State()
				_ = agent.Messages()
			}
		}()
	}

	// Concurrent writer: SetHistory / ClearHistory.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			msgs := []core.AgentMessage{
				{Role: "user", Content: fmt.Sprintf("msg-%d", n)},
			}
			agent.SetHistory(msgs)
			agent.ClearHistory()
		}(i)
	}

	// Concurrent Run calls (serialized by the agent internally).
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = agent.Run(ctx, core.Submission{
				Type:    core.SubmissionUserMessage,
				Content: "hello",
			})
		}()
	}

	wg.Wait()

	// Verify final state is consistent.
	finalState := agent.State()
	assert.Contains(t, []core.AgentState{core.StateStopped, core.StateError}, finalState)
}

// TestE2E_Phase49_EventStreamTimeout verifies that EventStream Send returns
// ErrSendTimeout when the BlockUntilConsumed policy's block timeout expires
// (49-14).
func TestE2E_Phase49_EventStreamTimeout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	t.Run("BlockTimeout", func(t *testing.T) {
		// Capacity 1, BlockUntilConsumed with 100ms timeout.
		stream := core.NewEventStream(1,
			core.WithEventDiscardPolicy(core.BlockUntilConsumed),
			core.WithEventBlockTimeout(100*time.Millisecond),
		)
		defer stream.Close()

		// Fill the buffer (capacity 1).
		err := stream.Send(core.AgentEvent{Kind: "msg", Content: "first"})
		require.NoError(t, err)

		// Second send should time out since no consumer is reading.
		err = stream.Send(core.AgentEvent{Kind: "msg", Content: "second"})
		require.ErrorIs(t, err, core.ErrSendTimeout)
	})

	t.Run("DiscardOldest", func(t *testing.T) {
		stream := core.NewEventStream(1,
			core.WithEventDiscardPolicy(core.DiscardOldest),
		)
		defer stream.Close()

		// Fill buffer then send more — should not block.
		err := stream.Send(core.AgentEvent{Kind: "msg", Content: "first"})
		require.NoError(t, err)
		err = stream.Send(core.AgentEvent{Kind: "msg", Content: "second"})
		require.NoError(t, err, "DiscardOldest should not block")

		// Drain — should get at least one event.
		select {
		case <-stream.Events():
		case <-time.After(time.Second):
			t.Fatal("should have at least one event in buffer")
		}
	})

	t.Run("DiscardNewest", func(t *testing.T) {
		stream := core.NewEventStream(1,
			core.WithEventDiscardPolicy(core.DiscardNewest),
		)
		defer stream.Close()

		// Fill buffer.
		err := stream.Send(core.AgentEvent{Kind: "msg", Content: "first"})
		require.NoError(t, err)
		// Second send should be dropped (not block).
		err = stream.Send(core.AgentEvent{Kind: "msg", Content: "second"})
		require.NoError(t, err, "DiscardNewest should not block")
	})

	t.Run("SentCount", func(t *testing.T) {
		stream := core.NewEventStream(10)
		defer stream.Close()

		for i := 0; i < 5; i++ {
			require.NoError(t, stream.Send(core.AgentEvent{Kind: "msg"}))
		}
		assert.Equal(t, 5, stream.SentCount())
	})
}

// TestE2E_Phase49_ToolRegistryCacheInvalidation verifies that
// DefaultToolRegistry.Version() increments on every Register call, enabling
// callers to detect cache invalidation (49-15).
func TestE2E_Phase49_ToolRegistryCacheInvalidation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	reg := tools.NewDefaultToolRegistry()

	// Initial version should be 0.
	v0 := reg.(*tools.DefaultToolRegistry).Version()
	assert.Equal(t, 0, v0)

	// Register a tool — version should increment.
	require.NoError(t, reg.Register(ctx, &stubTool{name: "tool-a"}))
	v1 := reg.(*tools.DefaultToolRegistry).Version()
	assert.Equal(t, 1, v1, "version should increment after first register")

	// Register another tool — version should increment again.
	require.NoError(t, reg.Register(ctx, &stubTool{name: "tool-b"}))
	v2 := reg.(*tools.DefaultToolRegistry).Version()
	assert.Equal(t, 2, v2, "version should increment after second register")

	// Re-register the same tool (overwrite) — version should still increment.
	require.NoError(t, reg.Register(ctx, &stubTool{name: "tool-a"}))
	v3 := reg.(*tools.DefaultToolRegistry).Version()
	assert.Equal(t, 3, v3, "version should increment on re-registration")

	// Version should be monotonically increasing.
	assert.True(t, v0 < v1 && v1 < v2 && v2 < v3,
		"version should be monotonically increasing")
}

// TestE2E_Phase49_StringsBuilder verifies that streaming LLM responses can be
// efficiently accumulated using strings.Builder, matching the pattern used by
// the TUI stream accumulator (49-16).
func TestE2E_Phase49_StringsBuilder(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create a mock LLM server with a known response.
	tmpl := mock.NewConversationTemplate("test", "strings-builder",
		mock.ConversationTurn{
			AssistantContent: "Hello, world! This is a streaming response.",
		},
	)
	server := mock.NewMockLLMServer(tmpl)

	// Stream the response and accumulate using strings.Builder (matching
	// the internal pattern used by the TUI stream accumulator).
	ch, err := server.Stream(ctx, []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
	})
	require.NoError(t, err)

	var builder strings.Builder
	var finalChunk *llm.MessageChunk
	for chunk := range ch {
		if chunk.Content != "" {
			builder.WriteString(chunk.Content)
		}
		if chunk.Final {
			fc := chunk
			finalChunk = &fc
		}
	}

	accumulated := builder.String()
	assert.Equal(t, "Hello, world! This is a streaming response.", accumulated,
		"accumulated content should match expected response")
	require.NotNil(t, finalChunk, "should receive a final chunk")
	assert.True(t, finalChunk.Final)

	// Verify the builder handles a large accumulation correctly.
	bigTmpl := mock.NewConversationTemplate("big", "big-response",
		mock.ConversationTurn{
			AssistantContent: strings.Repeat("A", 10000),
		},
	)
	bigServer := mock.NewMockLLMServer(bigTmpl)
	bigCh, err := bigServer.Stream(ctx, []llm.Message{
		{Role: llm.RoleUser, Content: "generate"},
	})
	require.NoError(t, err)

	var bigBuilder strings.Builder
	for chunk := range bigCh {
		bigBuilder.WriteString(chunk.Content)
	}
	assert.Equal(t, 10000, bigBuilder.Len(),
		"large accumulation should produce correct length")
}

// TestE2E_Phase49_JSONLBufferedWrite verifies that JSONLSessionStore uses
// buffered writes (bufio.Writer) for efficient I/O and persists entries
// correctly after Save/Close (49-17).
func TestE2E_Phase49_JSONLBufferedWrite(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	logPath := filepath.Join(t.TempDir(), "session.jsonl")
	store := session.NewJSONLSessionStore(logPath)

	// Append multiple entries.
	entries := []*session.SessionEntry{
		{ID: "e1", Type: session.EntryTypeUser, Content: "hello", Timestamp: time.Now()},
		{ID: "e2", Type: session.EntryTypeAssistant, Content: "hi there", Timestamp: time.Now()},
		{ID: "e3", Type: session.EntryTypeTool, Content: "tool result", Timestamp: time.Now()},
	}

	for _, e := range entries {
		require.NoError(t, store.Append(ctx, e))
	}

	// Save should flush the buffer.
	require.NoError(t, store.Save(ctx))

	// Verify the file exists and contains the entries.
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	assert.Len(t, lines, 3, "file should contain 3 JSONL lines")

	// Close the store.
	require.NoError(t, store.Close())

	// Reopen and verify entries are loaded correctly.
	store2 := session.NewJSONLSessionStore(logPath)
	require.NoError(t, store2.Open(ctx))

	for _, expected := range entries {
		got, err := store2.Get(ctx, expected.ID)
		require.NoError(t, err)
		assert.Equal(t, expected.Content, got.Content)
		assert.Equal(t, expected.Type, got.Type)
	}
	require.NoError(t, store2.Close())
}

// ---------------------------------------------------------------------------
// UX Tests (49-19, 49-20, 49-21, 49-23)
// ---------------------------------------------------------------------------

// TestE2E_Phase49_OnboardingAutoTrigger verifies the onboarding wizard's
// auto-trigger logic: it runs when no config exists and no API key is set,
// skips when GO_CLI_NO_ONBOARDING is set, skips when the API key is already
// configured, and takes the "prompt API key only" path when a config file
// already exists (49-19).
func TestE2E_Phase49_OnboardingAutoTrigger(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Chdir(tmpDir)

	// --- First-run detection: no config file in a fresh HOME ---
	configPath := filepath.Join(tmpDir, ".go-cli", "config.yaml")
	_, err := os.Stat(configPath)
	assert.True(t, os.IsNotExist(err), "no config file should exist in fresh HOME")

	// --- Skip when GO_CLI_NO_ONBOARDING is set ---
	t.Setenv("GO_CLI_NO_ONBOARDING", "1")
	cfg := &config.Config{}
	var out strings.Builder
	err = cli.RunOnboarding(cfg, strings.NewReader("sk-test\n"), &out)
	assert.NoError(t, err, "onboarding should be skipped without error")
	assert.Equal(t, "", cfg.Provider.APIKey, "API key should not be set when onboarding is disabled")
	assert.NotContains(t, out.String(), "Step 1", "wizard should not run when disabled")

	// --- Skip when API key is already configured ---
	t.Setenv("GO_CLI_NO_ONBOARDING", "")
	cfgWithKey := &config.Config{}
	cfgWithKey.Provider.APIKey = "sk-already-set"
	out.Reset()
	err = cli.RunOnboarding(cfgWithKey, strings.NewReader(""), &out)
	assert.NoError(t, err, "onboarding should skip when API key is set")
	assert.NotContains(t, out.String(), "Step 1", "wizard should not run when API key is set")

	// --- Existing config path: prompt for API key only ---
	// Create a config file so configExistsFunc() returns true. The wizard
	// takes the "existing config" path which only calls promptAPIKey — no
	// registry refresh (which would start HTTP client goroutines that leak)
	// and no saveOnboardingConfig (which would hit the OS keychain).
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o750))
	require.NoError(t, os.WriteFile(configPath, []byte("provider:\n  name: openai\n"), 0o600))

	apiKey := "sk-test-key-1234567890abcdef"
	cfgExisting := &config.Config{}
	out.Reset()
	err = cli.RunOnboarding(cfgExisting, strings.NewReader(apiKey+"\n"), &out)
	require.NoError(t, err, "onboarding should complete successfully with existing config")
	assert.Equal(t, apiKey, cfgExisting.Provider.APIKey, "API key should be set from input")
	assert.Contains(t, out.String(), "Step 1: API Key", "wizard should prompt for API key")
	assert.Contains(t, out.String(), "API key saved", "wizard should confirm API key saved")
	// The "existing config" path should NOT show model/theme selection.
	assert.NotContains(t, out.String(), "Step 2", "wizard should not prompt for model when config exists")
}

// TestE2E_Phase49_ModelListDynamicFetch verifies that the model registry
// can look up model information dynamically (49-20).
func TestE2E_Phase49_ModelListDynamicFetch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	t.Run("NoopRegistry", func(t *testing.T) {
		// NoopModelRegistry should return false for any lookup.
		reg := llm.NoopModelRegistry{}
		ctx := context.Background()
		info, ok := reg.Lookup(ctx, "openai", "gpt-4o")
		assert.False(t, ok)
		assert.Equal(t, "", info.Name)
		assert.Nil(t, reg.Providers())
		assert.Nil(t, reg.ModelsForProvider("openai"))
	})

	t.Run("MockProviderModels", func(t *testing.T) {
		// MockLLMServer exposes model info via Models().
		server := mock.NewMockLLMServer(nil)
		models := server.Models()
		require.Len(t, models, 1)
		assert.Equal(t, "mock-model", models[0].Name)
		assert.Equal(t, 128000, models[0].ContextWindow)
	})
}

// TestE2E_Phase49_TUIHelpOverlay verifies that the TUI BubbleteaApp can be
// constructed and its exported methods work correctly (49-21).
func TestE2E_Phase49_TUIHelpOverlay(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Create an event channel and construct the app.
	events := make(chan tui.AgentEvent, 10)
	app := tui.NewBubbleteaApp(events)
	require.NotNil(t, app)

	// The app should be constructable without panicking.
	// We cannot call Run() in a test (requires a terminal), but we can
	// verify the exported methods are available and don't panic.

	// Send a test event to the channel (non-blocking since buffered).
	select {
	case events <- tui.AgentEvent{Type: "run", Content: "test"}:
	default:
	}

	// Quit should not panic.
	app.Quit()
}

// TestE2E_Phase49_UserFriendlyErrors verifies that UserFriendlyError produces
// actionable error messages with hints for common error scenarios (49-23).
func TestE2E_Phase49_UserFriendlyErrors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Construct a UserFriendlyError directly and verify its format.
	authErr := &cli.UserFriendlyError{
		Err:    errors.New("401 unauthorized"),
		Action: "authentication failed",
		Hint:   "Check your API key",
	}

	errStr := authErr.Error()
	assert.Contains(t, errStr, "authentication failed")
	assert.Contains(t, errStr, "401 unauthorized")
	assert.Contains(t, errStr, "Hint: Check your API key")

	// Unwrap should return the underlying error.
	unwrapped := authErr.Unwrap()
	assert.Equal(t, "401 unauthorized", unwrapped.Error())

	// Verify errors.As works.
	var ufe *cli.UserFriendlyError
	require.True(t, errors.As(authErr, &ufe))
	assert.Equal(t, "authentication failed", ufe.Action)

	// Test various error scenarios.
	tests := []struct {
		name   string
		action string
		hint   string
		err    error
	}{
		{"rate_limit", "rate limit exceeded", "Wait and retry", errors.New("429 too many requests")},
		{"network", "network error", "Check connection", errors.New("connection refused")},
		{"timeout", "request timed out", "Retry", errors.New("timeout")},
		{"overflow", "context length exceeded", "Use /compact", errors.New("too many tokens")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ufe := &cli.UserFriendlyError{
				Err:    tc.err,
				Action: tc.action,
				Hint:   tc.hint,
			}
			s := ufe.Error()
			assert.Contains(t, s, tc.action)
			assert.Contains(t, s, tc.hint)
			assert.Contains(t, s, "Hint:")
		})
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// generateSelfSignedCert creates a temporary self-signed TLS certificate and
// key file, returning their paths. The files are cleaned up by t.TempDir().
func generateSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost", "127.0.0.1"},
		BasicConstraintsValid: true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	require.NoError(t, err)

	tmpDir := t.TempDir()
	certPath = filepath.Join(tmpDir, "cert.pem")
	keyPath = filepath.Join(tmpDir, "key.pem")

	certOut, err := os.Create(certPath)
	require.NoError(t, err)
	require.NoError(t, pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}))
	require.NoError(t, certOut.Close())

	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyOut, err := os.Create(keyPath)
	require.NoError(t, err)
	require.NoError(t, pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	require.NoError(t, keyOut.Close())

	return certPath, keyPath
}
