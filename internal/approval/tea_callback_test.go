package approval

import (
	"context"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/tui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTeaApprovalCallback_SendsRequest verifies that RequestApproval sends an
// ApprovalRequest on the channel with the correct tool name and args.
func TestTeaApprovalCallback_SendsRequest(t *testing.T) {
	ch := make(chan tui.ApprovalRequest, 1)
	cb := NewTeaApprovalCallback(ch)
	ctx := context.Background()

	go func() {
		_, _ = cb.RequestApproval(ctx, "write", map[string]any{"path": "/tmp/test"})
	}()

	select {
	case req := <-ch:
		assert.Equal(t, "write", req.ToolName)
		assert.Equal(t, "/tmp/test", req.Args["path"])
		assert.NotNil(t, req.ResponseCh)
	case <-time.After(time.Second):
		t.Fatal("did not receive approval request")
	}
}

// TestTeaApprovalCallback_ReceivesAllow verifies that sending ApprovalAllow
// via the response channel results in ApprovalAllow from RequestApproval.
func TestTeaApprovalCallback_ReceivesAllow(t *testing.T) {
	ch := make(chan tui.ApprovalRequest, 1)
	cb := NewTeaApprovalCallback(ch)
	ctx := context.Background()

	type result struct {
		r   ApprovalResult
		err error
	}
	resultCh := make(chan result, 1)
	go func() {
		r, err := cb.RequestApproval(ctx, "bash", map[string]any{"command": "ls"})
		resultCh <- result{r, err}
	}()

	req := <-ch
	req.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalAllow}

	select {
	case res := <-resultCh:
		assert.Equal(t, ApprovalAllow, res.r)
		assert.NoError(t, res.err)
	case <-time.After(time.Second):
		t.Fatal("did not receive result")
	}
}

// TestTeaApprovalCallback_ReceivesDeny verifies the deny path.
func TestTeaApprovalCallback_ReceivesDeny(t *testing.T) {
	ch := make(chan tui.ApprovalRequest, 1)
	cb := NewTeaApprovalCallback(ch)
	ctx := context.Background()

	resultCh := make(chan ApprovalResult, 1)
	go func() {
		r, _ := cb.RequestApproval(ctx, "bash", map[string]any{"command": "rm"})
		resultCh <- r
	}()

	req := <-ch
	req.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalDeny}

	select {
	case r := <-resultCh:
		assert.Equal(t, ApprovalDeny, r)
	case <-time.After(time.Second):
		t.Fatal("did not receive result")
	}
}

// TestTeaApprovalCallback_ReceivesAlwaysAllow verifies the always-allow path.
func TestTeaApprovalCallback_ReceivesAlwaysAllow(t *testing.T) {
	ch := make(chan tui.ApprovalRequest, 1)
	cb := NewTeaApprovalCallback(ch)
	ctx := context.Background()

	resultCh := make(chan ApprovalResult, 1)
	go func() {
		r, _ := cb.RequestApproval(ctx, "git", map[string]any{"command": "status"})
		resultCh <- r
	}()

	req := <-ch
	req.ResponseCh <- tui.ApprovalResponse{Decision: tui.ApprovalAlwaysAllow}

	select {
	case r := <-resultCh:
		assert.Equal(t, ApprovalAlwaysAllow, r)
	case <-time.After(time.Second):
		t.Fatal("did not receive result")
	}
}

// TestTeaApprovalCallback_ContextCanceled verifies that a canceled context
// results in ApprovalDeny and the context error.
func TestTeaApprovalCallback_ContextCanceled(t *testing.T) {
	ch := make(chan tui.ApprovalRequest) // unbuffered, no consumer
	cb := NewTeaApprovalCallback(ch)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r, err := cb.RequestApproval(ctx, "bash", nil)
	assert.Equal(t, ApprovalDeny, r)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestTeaApprovalCallback_ImplementsInterface is a compile-time check that
// TeaApprovalCallback satisfies the ApprovalCallback interface.
func TestTeaApprovalCallback_ImplementsInterface(t *testing.T) {
	var _ ApprovalCallback = (*TeaApprovalCallback)(nil)
}

// TestTeaApprovalCallback_DiffPreview verifies that a wired DiffPreviewFunc
// populates the DiffPreview field in the request.
func TestTeaApprovalCallback_DiffPreview(t *testing.T) {
	ch := make(chan tui.ApprovalRequest, 1)
	cb := NewTeaApprovalCallback(ch,
		WithDiffPreviewFunc(func(toolName string, args map[string]any) string {
			if toolName == "write" {
				return "+added line"
			}
			return ""
		}),
	)
	ctx := context.Background()

	go func() {
		_, _ = cb.RequestApproval(ctx, "write", map[string]any{"path": "/tmp/test"})
	}()

	select {
	case req := <-ch:
		assert.Equal(t, "+added line", req.DiffPreview)
	case <-time.After(time.Second):
		t.Fatal("did not receive approval request")
	}
}

// TestTeaApprovalCallback_NoDiffPreviewByDefault verifies that without a
// DiffPreviewFunc, the DiffPreview field is empty.
func TestTeaApprovalCallback_NoDiffPreviewByDefault(t *testing.T) {
	ch := make(chan tui.ApprovalRequest, 1)
	cb := NewTeaApprovalCallback(ch)
	ctx := context.Background()

	go func() {
		_, _ = cb.RequestApproval(ctx, "write", map[string]any{"path": "/tmp/test"})
	}()

	select {
	case req := <-ch:
		assert.Empty(t, req.DiffPreview)
	case <-time.After(time.Second):
		t.Fatal("did not receive approval request")
	}
}

// TestTeaApprovalCallback_ResponseContextCanceled verifies that canceling the
// context while waiting for a response results in ApprovalDeny.
func TestTeaApprovalCallback_ResponseContextCanceled(t *testing.T) {
	ch := make(chan tui.ApprovalRequest, 1)
	cb := NewTeaApprovalCallback(ch)
	ctx, cancel := context.WithCancel(context.Background())

	resultCh := make(chan error, 1)
	go func() {
		_, err := cb.RequestApproval(ctx, "bash", nil)
		resultCh <- err
	}()

	// Wait for the request to arrive, then cancel the context before responding.
	req := <-ch
	require.NotNil(t, req.ResponseCh)
	cancel()

	select {
	case err := <-resultCh:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("did not receive result after context cancel")
	}
}
