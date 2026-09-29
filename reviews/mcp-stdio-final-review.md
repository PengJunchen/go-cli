# MCP stdio final review

**Verdict: REJECT the current implementation for two P2 lifecycle/API findings.** The review covers the implementation at /Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-mcp-stdio, branch codex/repair-20260929-mcp-stdio, HEAD c060a91c6995097adcadd857ea87c5299aed39e3, based on 1144e1ebc9f339a04d502f0bdca04069c9d30ff5. The reviewed source commits are 4c73ad49f070327a48d46619a3efacf2c85dd691 (owned process lifecycle) and 81a3eed1848249b14ad8a3d2457b2551ccd3ca04 (bounded response frames). The worktree was clean; no source, index, or commit was changed for this review.

## Findings

### [P2] Connect reports success while retaining a stopped stdio owner

At internal/mcp/adapter.go:577-580, Connect returns nil immediately whenever c.connected is true. An owned request cancellation or response-budget failure calls owner.startStop() (adapter.go:188-205) but does not clear adapterCore.connected. After the process and active I/O finish, a later explicit Connect still takes the connected fast path before reaching the retained-owner cleanup at adapter.go:591-609. It reports a healthy session without starting a replacement. A subsequent request is rejected by stdioProcessOwner.beginIO because the old owner is stopping.

This is reproducible after a canceled silent tools/call: wait for its exact ExecutionDone, then call Connect(context.Background()). The call returns nil while the same, already-stopped owner remains attached. Treat the stopped owner as unavailable, join that exact generation, and only clear it if it is still the current owner before starting a new child. This must not replay the uncertain tools/call; recovery is for a later explicit operation.

### [P2] Request cancellation drops a caller-supplied cause

The pre-dispatch branch at internal/mcp/adapter.go:162-168 and the post-dispatch ctx.Done() branch at adapter.go:188-194 return ctx.Err(). For contexts made with context.WithCancelCause or context.WithDeadlineCause, this drops the supplied cause. Neighboring owned write/read cancellation paths already use contextCause(ctx) (adapter.go:177-179, 213-217), and the HTTP adapter uses the same helper to preserve both the standard context error and the caller's cause.

A deterministic post-dispatch probe replaces the connected transport writer with a writer that cancels the request with a sentinel cause while reporting a successful write. The resulting typed pending error retains context.Canceled but errors.Is(err, sentinelCause) is false. Use contextCause(ctx) for pre- and post-dispatch cancellation exits so the cause remains reachable through the pending error's unwrap chain. The probe below directly covers the post-dispatch branch; the pre-dispatch branch should receive equivalent coverage.

## Reproduction probes

The following temporary tests use the committed newOwnedStdioHelperAdapter helper and can be placed at /tmp/stdio_review_probe_test.go. Each was injected and run separately without modifying the worktree using a Go test overlay; both fail on the reviewed source for the reasons above. The combined command below reruns both probes together.

~~~go
package mcp

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type reviewerCancelWriter struct {
	cancel context.CancelCauseFunc
	cause  error
}

func (w reviewerCancelWriter) Write(data []byte) (int, error) {
	w.cancel(w.cause)
	return len(data), nil
}

func TestReviewerConnectAfterCanceledCallDoesNotClaimStaleOwner(t *testing.T) {
	adapter, _, wireFile := newOwnedStdioHelperAdapter(t, "silent-call")
	require.NoError(t, adapter.Connect(context.Background()))
	owner := adapter.adapterCore.procOwner
	require.NotNil(t, owner)

	callCtx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, err := adapter.CallTool(callCtx, "silent", nil)
		callDone <- err
	}()
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(wireFile)
		return err == nil && strings.Contains(string(data), "tools/call")
	}, 2*time.Second, 5*time.Millisecond)
	cancel()

	var callErr error
	select {
	case callErr = <-callDone:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled call did not return")
	}
	require.Error(t, callErr)
	var pending interface{ ExecutionDone() <-chan struct{} }
	require.ErrorAs(t, callErr, &pending)
	select {
	case <-pending.ExecutionDone():
	case <-time.After(2 * time.Second):
		t.Fatal("owned process did not finish")
	}

	connectErr := adapter.Connect(context.Background())
	currentOwner := adapter.adapterCore.procOwner
	if connectErr == nil && currentOwner == owner {
		t.Errorf("Connect returned nil while retaining the already-stopped stdio owner")
	}
}

func TestReviewerOwnedRequestPreservesCancelCauseAfterDispatch(t *testing.T) {
	adapter, _, _ := newOwnedStdioHelperAdapter(t, "silent-call")
	require.NoError(t, adapter.Connect(context.Background()))

	cause := errors.New("reviewer cancellation cause")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	adapter.adapterCore.conn.out = reviewerCancelWriter{cancel: cancel, cause: cause}

	_, err := adapter.adapterCore.conn.Request(ctx, "tools/call", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, cause)
	var pending interface{ ExecutionDone() <-chan struct{} }
	require.ErrorAs(t, err, &pending)
	select {
	case <-pending.ExecutionDone():
	case <-time.After(2 * time.Second):
		t.Fatal("owned process did not finish")
	}
}
~~~

Use this overlay mapping, adjusted if the worktree path changes:

~~~json
{
  "Replace": {
    "/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-mcp-stdio/internal/mcp/stdio_review_probe_test.go": "/tmp/stdio_review_probe_test.go"
  }
}
~~~

Then run:

~~~sh
rtk env GOMAXPROCS=2 go test -race ./internal/mcp -run '^TestReviewer(ConnectAfterCanceledCallDoesNotClaimStaleOwner|OwnedRequestPreservesCancelCauseAfterDispatch)$' -count=1 -timeout=30s -overlay=/tmp/stdio_review_overlay.json
~~~

Observed on the reviewed source: the stale-owner test fails with “Connect returned nil while retaining the already-stopped stdio owner”; the cause test fails because the chain contains only “mcp: stdio request pending: context canceled” and “context canceled”, not the sentinel cause.

## Verification

These independent checks passed on the reviewed worktree:

- rtk env GOMAXPROCS=2 go test -race ./internal/mcp -run '^TestOwnedStdio' -count=5 -timeout=180s
- rtk env GOMAXPROCS=2 go test -race ./internal/mcp -count=1 -timeout=180s
- rtk env GOMAXPROCS=2 go test -race ./internal/cli -run '^(TestInteractiveCmd_RegisterMCPTools_(NilConfig|EmptyServers|StdioServer|StreamableHTTPServer)|TestNewMCPClientForDoctorSelectsStreamableHTTPAdapter|TestToMCPConfigSelectsStreamableHTTPForURL)$' -count=1 -timeout=120s
- rtk git diff --check 1144e1ebc9f339a04d502f0bdca04069c9d30ff5..HEAD

The committed helper tests spawn a real child process and exercise MCP stdio framing, cancellation, joining, and size limits. They do not prove orphan behavior for the production CLI process tree; production CLI E2E and make verify were not run in this review. The existing pending-error integration exposes ExecutionDone to run ownership, and the completed owned-stdio tests passed.
