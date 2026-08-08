package approval

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tui"
)

// Compile-time assertion that TeaApprovalCallback satisfies ApprovalCallback.
// This complements the assertion in callback.go and guarantees the contract
// is honored from the test side as well.
var _ ApprovalCallback = (*TeaApprovalCallback)(nil)

// requestResult carries the outcome of an asynchronous RequestApproval call so
// tests can inspect both the result and error without races.
type requestResult struct {
	res ApprovalResult
	err error
}

// TestTeaApprovalCallback_ImplementsInterface verifies at runtime that a
// *TeaApprovalCallback can be assigned to the ApprovalCallback interface.
// (The compile-time check is the package-level var above.)
func TestTeaApprovalCallback_ImplementsInterface(t *testing.T) {
	ch := make(chan tui.ApprovalRequest, 1)
	var cb ApprovalCallback = NewTeaApprovalCallback(ch)
	assert.NotNil(t, cb)
}

// TestTeaApprovalCallback_SendsRequest verifies that RequestApproval sends an
// ApprovalRequest carrying the correct ToolName and Args through the channel,
// and that the returned result mirrors the user's response.
func TestTeaApprovalCallback_SendsRequest(t *testing.T) {
	ch := make(chan tui.ApprovalRequest, 1)
	cb := NewTeaApprovalCallback(ch)

	args := map[string]any{"path": "/tmp/test", "mode": "w"}

	done := make(chan requestResult, 1)
	go func() {
		res, err := cb.RequestApproval(context.Background(), "write", args)
		done <- requestResult{res, err}
	}()

	// Receive the request forwarded by the callback and inspect its fields.
	select {
	case req := <-ch:
		assert.Equal(t, "write", req.ToolName)
		assert.Equal(t, args, req.Args)
		assert.NotNil(t, req.ResponseCh, "ResponseCh must be initialized")
		// Unblock the goroutine by replying with Allow.
		req.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalAllow}
	case <-time.After(time.Second):
		t.Fatal("did not receive approval request on channel")
	}

	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, ApprovalAllow, r.res)
	case <-time.After(time.Second):
		t.Fatal("RequestApproval did not return")
	}
}

// TestTeaApprovalCallback_ReceivesAllow verifies that when the user sends an
// ApprovalAllow decision, the callback returns ApprovalAllow with no error.
func TestTeaApprovalCallback_ReceivesAllow(t *testing.T) {
	ch := make(chan tui.ApprovalRequest)
	cb := NewTeaApprovalCallback(ch)

	done := make(chan requestResult, 1)
	go func() {
		res, err := cb.RequestApproval(context.Background(), "bash", nil)
		done <- requestResult{res, err}
	}()

	// Receive the request and respond with Allow.
	select {
	case req := <-ch:
		req.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalAllow}
	case <-time.After(time.Second):
		t.Fatal("did not receive approval request on channel")
	}

	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, ApprovalAllow, r.res)
	case <-time.After(time.Second):
		t.Fatal("RequestApproval did not return")
	}
}

// TestTeaApprovalCallback_ReceivesDeny verifies that when the user sends an
// ApprovalDeny decision, the callback returns ApprovalDeny with no error.
func TestTeaApprovalCallback_ReceivesDeny(t *testing.T) {
	ch := make(chan tui.ApprovalRequest)
	cb := NewTeaApprovalCallback(ch)

	done := make(chan requestResult, 1)
	go func() {
		res, err := cb.RequestApproval(context.Background(), "bash", nil)
		done <- requestResult{res, err}
	}()

	// Receive the request and respond with Deny.
	select {
	case req := <-ch:
		req.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalDeny}
	case <-time.After(time.Second):
		t.Fatal("did not receive approval request on channel")
	}

	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, ApprovalDeny, r.res)
	case <-time.After(time.Second):
		t.Fatal("RequestApproval did not return")
	}
}

// TestTeaApprovalCallback_ReceivesAlwaysAllow verifies that when the user sends
// an ApprovalAlwaysAllow decision, the callback returns ApprovalAlwaysAllow
// with no error.
func TestTeaApprovalCallback_ReceivesAlwaysAllow(t *testing.T) {
	ch := make(chan tui.ApprovalRequest)
	cb := NewTeaApprovalCallback(ch)

	done := make(chan requestResult, 1)
	go func() {
		res, err := cb.RequestApproval(context.Background(), "git", nil)
		done <- requestResult{res, err}
	}()

	// Receive the request and respond with AlwaysAllow.
	select {
	case req := <-ch:
		req.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalAlwaysAllow}
	case <-time.After(time.Second):
		t.Fatal("did not receive approval request on channel")
	}

	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, ApprovalAlwaysAllow, r.res)
	case <-time.After(time.Second):
		t.Fatal("RequestApproval did not return")
	}
}

// TestTeaApprovalCallback_ContextCanceled verifies that when the context is
// canceled while the callback is blocked waiting for a user response, the
// callback returns ApprovalDeny and the context's error.
func TestTeaApprovalCallback_ContextCanceled(t *testing.T) {
	ch := make(chan tui.ApprovalRequest)
	cb := NewTeaApprovalCallback(ch)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan requestResult, 1)
	go func() {
		res, err := cb.RequestApproval(ctx, "bash", nil)
		done <- requestResult{res, err}
	}()

	// Receive the request so the goroutine advances past the channel send and
	// into the response-wait select. This guarantees the cancel below races
	// only against the response-wait, not the send.
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("did not receive approval request on channel")
	}

	// Cancel the context while the goroutine is blocked waiting for a response.
	cancel()

	select {
	case r := <-done:
		assert.Equal(t, ApprovalDeny, r.res, "canceled context should deny")
		require.Error(t, r.err, "canceled context should return an error")
		assert.True(t, errors.Is(r.err, context.Canceled),
			"error should wrap context.Canceled, got %v", r.err)
	case <-time.After(time.Second):
		t.Fatal("RequestApproval did not return after context cancel")
	}
}
