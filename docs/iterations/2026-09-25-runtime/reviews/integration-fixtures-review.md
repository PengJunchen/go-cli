# Integration fixture review

## Decision: REJECT (one P2 test reliability defect)

Reviewed commit `9bb30f36584c311427e47b7079816f5665fd88d7` in the independent worktree `/Users/pengjunchen/.codex/worktrees/integration-fixtures/go-cli`, based on `525007e914692665a7c490eae662c3dfb2e2ebbd`. The commit changes five test files only and contains no product-source changes.

The retry expectation updates are correct: `MaxAttempts` includes the initial dispatch, so a limit of three means three total calls. The Phase 3 fixture also now uses a typed HTTP 503 transient error instead of categorizing `context.DeadlineExceeded` as transient. That preserves the production contract that cancellation and deadlines stop retries while retaining the intended retry-audit assertion.

### Finding

**P2 — `TestACP_StdioAdapterConnectDisconnect` can accept a disconnect frame as the connect frame and then hang.** The peer checks `strings.Contains(frame, "connect")`; a JSON `TypeDisconnect` frame contains that substring, so the first assertion can pass on the wrong protocol message. On the peer's later expected-message path, it closes its inbound writer, but if the false match occurs first it waits for another line. The test then waits on `peerErrCh` without a deadline. The adapter cannot observe EOF while the peer writer remains open, and test cleanup cannot run while the goroutine is blocked. This makes the assertion ambiguous and can hang the test process.

Compare the decoded ACP message type exactly to `TypeConnect` / `TypeDisconnect`, and bound the peer-result wait with a context or timer. Ensure the peer closes every pipe end it owns on all exits. Treat failed Connect/Disconnect assertions as fatal so the test does not continue with an invalid protocol state.

### Evidence

The prior integration race failure log supplied by the parent task recorded the old retry-count and Phase 3 deadline-fixture failures. The corrected targeted tests below pass. That verifies the fixture corrections, but does not resolve the stdio finding above.

Commands run in the independent worktree; each test-list command exited 0 and listed the requested test names, and each race command exited 0:

- `rtk proxy go test -list '^Test(Production_RetryPolicy_TransientErrors|Boundary_RetryMaxAttemptsBoundary|E2E_Phase21_RetryExhaustedReturnsError|ACP_StdioAdapterConnectDisconnect)$' ./tests/e2e_20260802`
- `rtk proxy go test -list '^TestPhase3EndToEnd$' ./tests`
- `rtk proxy go test -list '^Test(ShouldRetryRespectsMaxAttempts|ShouldRetryRefusesCanceledContextAndErrors|ClassifyStringMatching)$' ./internal/production`
- `rtk env GOMAXPROCS=2 go test -race -p 2 ./tests/e2e_20260802 -run '^(TestProduction_RetryPolicy_TransientErrors|TestBoundary_RetryMaxAttemptsBoundary|TestE2E_Phase21_RetryExhaustedReturnsError|TestACP_StdioAdapterConnectDisconnect)$' -count=1`
- `rtk env GOMAXPROCS=2 go test -race -p 2 ./tests -run '^TestPhase3EndToEnd$' -count=1`
- `rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/production -run '^(TestClassifyStringMatching|TestShouldRetryRespectsMaxAttempts|TestShouldRetryRefusesCanceledContextAndErrors)$' -count=1`
- `rtk git status --short --branch && rtk git rev-parse HEAD && rtk git diff --check` — clean worktree at the reviewed commit; diff check passed.

This is a test-fixture review only. No source files were edited and no changes were staged or committed.

## Independent delta review — `f42634815ad1324b0b34426896f497acf3c750df`

**Decision: ACCEPT for the stdio fixture delta.** This review is limited to the test-only follow-up atop `9bb30f3`; the earlier rejection remains the historical verdict for `9bb30f3` itself.

The peer now decodes each JSON frame into `acp.ACPMessage` and compares `Type` exactly against `TypeConnect` and `TypeDisconnect`, removing the substring ambiguity. Its buffered result channel lets the peer report success or failure without waiting for the test goroutine to receive; the peer defers closing both pipe ends it owns on every return. The test bounds the protocol exchange with a three-second context and selects on both the peer result and context deadline. `assert.NoError` is nonfatal, but its failure remains recorded by testify; the test continues into the bounded peer check, so neither a Connect nor Disconnect error can be silently accepted.

On the successful path, the peer closes the pipe writer feeding the adapter after observing the exact disconnect frame. The adapter's reader sees real EOF, exits through `readLoop`'s deferred `workerDone`, and cleanup waits for `session.joined` before closing `cleanupDone`; `Disconnect` waits on that cleanup completion. This exercises actual reader-worker joining rather than simulating completion. The failure paths also remain bounded by the same deadline and pipe cleanup.

Validation in the independent worktree `/Users/pengjunchen/.codex/worktrees/integration-fixtures/go-cli` at `f42634815ad1324b0b34426896f497acf3c750df`:

- `rtk proxy go test -list '^TestACP_StdioAdapterConnectDisconnect$' ./tests/e2e_20260802` — listed the target test.
- `rtk env GOMAXPROCS=2 go test -race -p 2 -count=10 -run '^TestACP_StdioAdapterConnectDisconnect$' ./tests/e2e_20260802` — passed all ten runs.

No product source was changed. This delta review accepts the stdio fixture correction; it does not expand the scope of the earlier review to other integration fixtures.
