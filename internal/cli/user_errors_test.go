package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// TestClassifyNetworkError verifies that connection-refused and timeout errors
// are classified with the appropriate recovery hint.
func TestClassifyNetworkError(t *testing.T) {
	// Connection refused.
	connErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	ufe := classifyError(connErr)
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError for connection refused")
	}
	if !strings.Contains(strings.ToLower(ufe.Action), "connection refused") {
		t.Errorf("expected action to mention 'connection refused', got %q", ufe.Action)
	}
	if ufe.Hint == "" {
		t.Error("expected non-empty hint for connection refused")
	}

	// Timeout via net.Error interface.
	timeoutErr := &timeoutError{msg: "i/o timeout"}
	ufe = classifyError(timeoutErr)
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError for timeout")
	}
	if !strings.Contains(strings.ToLower(ufe.Action), "timed out") {
		t.Errorf("expected action to mention 'timed out', got %q", ufe.Action)
	}

	// DNS error via string matching.
	dnsErr := errors.New("dial tcp: lookup api.invalid: no such host")
	ufe = classifyError(dnsErr)
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError for DNS failure")
	}
	if !strings.Contains(strings.ToLower(ufe.Action), "dns") {
		t.Errorf("expected action to mention 'dns', got %q", ufe.Action)
	}
}

// timeoutError implements net.Error with Timeout() returning true.
type timeoutError struct{ msg string }

func (e *timeoutError) Error() string   { return e.msg }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return false }

// TestClassifyAuthError verifies that 401 and 403 provider errors are
// classified with an API-key hint.
func TestClassifyAuthError(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"unauthorized 401", 401},
		{"forbidden 403", 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pe := &llm.ProviderError{
				StatusCode: tt.status,
				ErrorType:  llm.ErrTypeAuth,
				Provider:   "test",
				Message:    "invalid api key",
			}
			ufe := classifyError(pe)
			if ufe == nil {
				t.Fatal("expected non-nil UserFriendlyError")
			}
			if !strings.Contains(strings.ToLower(ufe.Action), "auth") {
				t.Errorf("expected action to mention 'auth', got %q", ufe.Action)
			}
			if !strings.Contains(strings.ToLower(ufe.Hint), "api key") {
				t.Errorf("expected hint to mention 'api key', got %q", ufe.Hint)
			}
			// Unwrap should return the original ProviderError.
			if !errors.Is(ufe, pe) {
				t.Error("Unwrap should return the original ProviderError")
			}
		})
	}
}

// TestClassifyRateLimitError verifies that 429 errors are classified with a
// rate-limit hint.
func TestClassifyRateLimitError(t *testing.T) {
	pe := &llm.ProviderError{
		StatusCode: 429,
		ErrorType:  llm.ErrTypeRateLimit,
		Provider:   "test",
		Message:    "too many requests",
	}
	ufe := classifyError(pe)
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError")
	}
	if !strings.Contains(strings.ToLower(ufe.Action), "rate limit") {
		t.Errorf("expected action to mention 'rate limit', got %q", ufe.Action)
	}
}

// TestClassifyConfigError verifies that file-not-found and YAML parse errors
// are classified with a doctor hint.
func TestClassifyConfigError(t *testing.T) {
	// File not found via os.ErrNotExist.
	ufe := classifyError(os.ErrNotExist)
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError for ErrNotExist")
	}
	if !strings.Contains(strings.ToLower(ufe.Hint), "doctor") {
		t.Errorf("expected hint to mention 'doctor', got %q", ufe.Hint)
	}

	// YAML parse error via string matching.
	yamlErr := errors.New("yaml: unmarshal errors: line 5: cannot unmarshal !!str into config")
	ufe = classifyError(yamlErr)
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError for YAML error")
	}
	if !strings.Contains(strings.ToLower(ufe.Action), "parse") {
		t.Errorf("expected action to mention 'parse', got %q", ufe.Action)
	}
	if !strings.Contains(strings.ToLower(ufe.Hint), "doctor") {
		t.Errorf("expected hint to mention 'doctor', got %q", ufe.Hint)
	}
}

// TestClassifyDefault verifies that unclassified errors get a generic hint.
func TestClassifyDefault(t *testing.T) {
	ufe := classifyError(errors.New("something unexpected happened"))
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError for generic error")
	}
	if !strings.Contains(strings.ToLower(ufe.Action), "failed") {
		t.Errorf("expected action to mention 'failed', got %q", ufe.Action)
	}
	if ufe.Hint == "" {
		t.Error("expected non-empty hint for generic error")
	}
}

// TestClassifyNil verifies that nil input returns nil.
func TestClassifyNil(t *testing.T) {
	if ufe := classifyError(nil); ufe != nil {
		t.Errorf("expected nil for nil input, got %v", ufe)
	}
}

// TestClassifyContextCanceled verifies that context cancellation errors
// return nil (they are expected, not failures needing hints).
func TestClassifyContextCanceled(t *testing.T) {
	if ufe := classifyError(context.Canceled); ufe != nil {
		t.Errorf("expected nil for context.Canceled, got %v", ufe)
	}
	if ufe := classifyError(context.DeadlineExceeded); ufe != nil {
		t.Errorf("expected nil for context.DeadlineExceeded, got %v", ufe)
	}
}

// TestUserFriendlyErrorFormat verifies the Error() output format.
func TestUserFriendlyErrorFormat(t *testing.T) {
	ufe := &UserFriendlyError{
		Err:    errors.New("boom"),
		Action: "doing thing",
		Hint:   "try harder",
	}
	s := ufe.Error()
	if !strings.Contains(s, "doing thing") {
		t.Errorf("expected action in output, got %q", s)
	}
	if !strings.Contains(s, "boom") {
		t.Errorf("expected underlying error in output, got %q", s)
	}
	if !strings.Contains(s, "Hint: try harder") {
		t.Errorf("expected hint in output, got %q", s)
	}
}

// TestUserFriendlyErrorUnwrap verifies that Unwrap returns the original error.
func TestUserFriendlyErrorUnwrap(t *testing.T) {
	orig := errors.New("root cause")
	ufe := &UserFriendlyError{Err: orig, Action: "test", Hint: "hint"}
	if !errors.Is(ufe, orig) {
		t.Error("errors.Is should find the original error")
	}
}

// TestClassifyOverflowError verifies that overflow errors get a compaction hint.
func TestClassifyOverflowError(t *testing.T) {
	pe := &llm.ProviderError{
		StatusCode: 400,
		ErrorType:  llm.ErrTypeOverflow,
		Provider:   "test",
		Message:    "context_length_exceeded",
	}
	ufe := classifyError(pe)
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError")
	}
	if !strings.Contains(strings.ToLower(ufe.Action), "context") {
		t.Errorf("expected action to mention 'context', got %q", ufe.Action)
	}
	if !strings.Contains(strings.ToLower(ufe.Hint), "compact") {
		t.Errorf("expected hint to mention 'compact', got %q", ufe.Hint)
	}
}

// TestClassifyProviderNetworkError verifies that ProviderError with
// ErrTypeNetwork is classified as a network error.
func TestClassifyProviderNetworkError(t *testing.T) {
	pe := &llm.ProviderError{
		StatusCode: 0,
		ErrorType:  llm.ErrTypeNetwork,
		Provider:   "test",
		Message:    "dial tcp: connection reset",
	}
	ufe := classifyError(pe)
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError")
	}
	if !strings.Contains(strings.ToLower(ufe.Action), "network") {
		t.Errorf("expected action to mention 'network', got %q", ufe.Action)
	}
}

// Ensure timeoutError satisfies net.Error at compile time.
var _ net.Error = (*timeoutError)(nil)

// TestClassifyConnectionRefusedString verifies the string-based fallback for
// "connection refused" in a plain error (not net.OpError).
func TestClassifyConnectionRefusedString(t *testing.T) {
	ufe := classifyError(errors.New("dial tcp 127.0.0.1:443: connect: connection refused"))
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError")
	}
	if !strings.Contains(strings.ToLower(ufe.Action), "connection refused") {
		t.Errorf("expected 'connection refused' in action, got %q", ufe.Action)
	}
}

// TestClassifyFileNotFoundString verifies the string-based fallback for
// "no such file or directory".
func TestClassifyFileNotFoundString(t *testing.T) {
	ufe := classifyError(errors.New("open config.yaml: no such file or directory"))
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError")
	}
	if !strings.Contains(strings.ToLower(ufe.Hint), "doctor") {
		t.Errorf("expected 'doctor' in hint, got %q", ufe.Hint)
	}
}

// TestClassifyDNSErrorString verifies the string-based fallback for "no such host".
func TestClassifyDNSErrorString(t *testing.T) {
	ufe := classifyError(errors.New("lookup nonexistent.example.com: no such host"))
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError")
	}
	if !strings.Contains(strings.ToLower(ufe.Action), "dns") {
		t.Errorf("expected 'dns' in action, got %q", ufe.Action)
	}
}

// TestClassifyTimeoutNetError verifies a net.Error with Timeout()=true.
func TestClassifyTimeoutNetError(t *testing.T) {
	// Use a real net error that implements Timeout().
	ufe := classifyError(&timeoutError{msg: "read tcp: i/o timeout"})
	if ufe == nil {
		t.Fatal("expected non-nil UserFriendlyError")
	}
	if !strings.Contains(strings.ToLower(ufe.Action), "timed out") {
		t.Errorf("expected 'timed out' in action, got %q", ufe.Action)
	}
}

// dummy to avoid unused import warning if time is not otherwise used.
var _ = time.Second
