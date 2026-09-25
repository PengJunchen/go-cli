# R6 ACP stdio lifecycle review

## Verdict

**Rejected pending four source fixes.** The frozen stdio slice covers most of the intended lifecycle mechanics, but clean input EOF leaves a dead transport writable, outbound frames bypass the stated 1 MiB bound, and two malformed/outcome cases are reported or delivered as valid.

## Fixed snapshot and scope

- Reviewed commit: `4e96a92c89c92dbe58ed3452b12d94ddf307c14d`, in a separate detached worktree at `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r6-acp-stdio-review`.
- Reviewed the changes to `internal/acp/client.go`, `internal/acp/stdio_adapter.go`, and their stdio tests against the approved `r6-adapter-lifecycle-handoff.md`.
- No production source was changed. Temporary review probes were removed before the package-wide test run; this worktree contains only this unstaged review report.

## Findings

### R6-STDIO-1 [P1] Fence the session after clean reader EOF

At [`stdio_adapter.go:473`](../../../../internal/acp/stdio_adapter.go), `readLoop` returns immediately on `io.EOF`. Its defer closes the inbound channel and decrements the worker count, but does not mark the adapter closing or disconnected. A peer that cleanly closes the input can therefore leave `Connect` idempotently successful and `SendMessage` accepted after the receive stream has ended.

A temporary regression probe used a real `io.Pipe`: it connected, closed the peer writer, waited for `ReceiveMessages` to close, and then expected `SendMessage` to fail. The send returned nil. The exact RED command was `rtk proxy go test -race ./internal/acp -run '^TestReviewStdio' -count=1` (exit 1).

### R6-STDIO-2 [P1] Apply the frame cap to outbound writes

The 1 MiB limit is enforced by `readBoundedFrame`, but [`writeLine` at `stdio_adapter.go:641`](../../../../internal/acp/stdio_adapter.go) marshals and writes without checking the encoded payload length. A caller can send a JSON frame larger than the configured cap; the writer accepts it. This leaves the transport asymmetric and bypasses the approved frame bound on every outbound message path.

A temporary probe sent content of `stdioMaxFrameBytes+1` and expected `errors.Is(err, errStdioTooLarge)`; `SendMessage` returned nil. This failed in the same exact command recorded for R6-STDIO-1.

### R6-STDIO-3 [P2] Report a started disconnect write as unknown delivery

Both pending returns in [`Disconnect` at `stdio_adapter.go:236,251`](../../../../internal/acp/stdio_adapter.go) call `pendingFromChannel(..., false)`. When `startCleanup` acquires the writer gate, [`stdio_adapter.go:566-573`](../../../../internal/acp/stdio_adapter.go) starts a disconnect-frame write. If that caller-owned `io.Pipe` write blocks past the deadline, delivery may be partial or complete, yet the pending error reports `UnknownDelivery()==false`.

The temporary `TestReviewStdioDisconnectPendingWriteUnknown` probe drained the connect frame, left the real pipe unread, and called `Disconnect` with a 30 ms deadline. It returned pending with `UnknownDelivery()==false` while the disconnect write was still blocked; expecting true failed. The existing `TestStdioAdapterDisconnectUnreadPipeReturns` also exercises this blocked-write path but asserts false.

### R6-STDIO-4 [P2] Reject non-object and empty-type private-profile frames

[`readLoop` at `stdio_adapter.go:484-489`](../../../../internal/acp/stdio_adapter.go) only checks whether `json.Unmarshal` returns a syntax/type error. JSON `null` and `{}` both unmarshal successfully into a zero-value `ACPMessage` and are sent to `ReceiveMessages`. This admits frames that are not ACP message objects and have no message type, despite the private profile's strict malformed-frame boundary.

The temporary `TestReviewStdioRequiresACPObjectEnvelope` probe sent `null\n` and `{}\n`; each was delivered as a zero-value message. The exact command `rtk proxy go test -race ./internal/acp -run '^TestReviewStdioRequiresACPObjectEnvelope$' -count=1` exited 1. Reject non-object/empty-type envelopes using the existing ACP message vocabulary. This finding does not require a version field on every frame or mandatory sender identity.

## Contract checks that passed

- `client.go` defines `ACPJSONProfileVersionV1` as `go-cli-acp-json/1`, and the first Connect frame includes it; `TestStdioConnectDeclaresPrivateProfileVersion` passed.
- `readBoundedFrame` uses one bounded `bufio.Reader`/reader worker, excludes LF and one preceding CR from payload size, accepts an exact-limit CRLF frame, and rejects an over-limit frame. Syntactically malformed JSON and truncated non-LF EOF terminate the receive stream.
- All outbound paths use the same per-session writer gate. A canceled send before dispatch is known-not-dispatched; a started `SendMessage` write returns pending/unknown with a `Done` channel that closes only after the underlying write returns. The started message is not replayed.
- Caller-owned transports are not closed without `WithStdioOwnedTransportCloser`. An explicitly owned closer is invoked once; if its `Close` blocks, `Disconnect` and repeat `Disconnect` retain and wait on the same cleanup owner. `Connect` during that pending cleanup returns the same pending completion and emits no replacement Connect frame.

## Validation

The relevant test names were enumerated before execution. The stdio subset passed:

```text
rtk proxy go test -race ./internal/acp -run '^TestStdio' -count=1
ok github.com/pengjunchen/go-cli/internal/acp 1.621s
```

After removing the temporary review probes, the full ACP package race suite passed:

```text
rtk proxy go test -race -count=1 ./internal/acp
ok github.com/pengjunchen/go-cli/internal/acp 6.517s
```

These existing tests passing does not close the four findings above; each was reproduced by a focused temporary behavioral probe against the frozen commit.

## Follow-up review: 4b8c732

### Verdict

**Rejected pending one additional state-arbitration fix.** The four findings above are fixed in `4b8c7322cc8f0c6b813e67508e82093a239fc036` relative to base `4e96a92c89c92dbe58ed3452b12d94ddf307c14d`. The follow-up still allows a queued best-effort disconnect frame to start after `Disconnect` has returned a pending result that says delivery is known not to have started.

### Resolution checks

- Clean EOF now enters `startCleanup` before `readLoop` returns; `TestStdioCleanEOFClosesReceiveAndFencesSend` confirms the receive channel closes and future sends are rejected.
- `marshalFrame` enforces the 1 MiB payload bound before the writer gate and before `Write`. `TestStdioOutboundFrameLimitRejectsBeforeWriteWithoutPoisoning` accepts an exact-limit frame, rejects the next byte without writing a prefix, and confirms the session remains usable.
- `readLoop` now rejects `null`, `{}`, and unknown message types. `TestStdioRejectsNonObjectAndUnknownTypeFrames` covers these cases without requiring a per-frame profile version or sender ID.
- A started disconnect-frame write and an active ordinary send are reported as pending with unknown delivery. `TestStdioAdapterDisconnectUnreadPipeReturns` and `TestStdioDisconnectWithActiveSendReportsUnknownDelivery` exercise both paths with blocking pipes/writers. Repeated Disconnect still joins the same cleanup operation; no second frame is appended while an ordinary send owns the gate.

### R6-STDIO-5 [P2] Arbitrate queued disconnect writes against pending return

In `startCleanup` at [`stdio_adapter.go:556-575`](../../../../internal/acp/stdio_adapter.go), the disconnect frame gets the writer gate and a tracked worker, but `session.activeWrite` is not set until the newly launched goroutine runs. `pendingDisconnect` at [`stdio_adapter.go:682-693`](../../../../internal/acp/stdio_adapter.go) can therefore observe no active write, return `StdioPendingError{UnknownDelivery:false}`, and leave the cleanup worker free to begin `Write` afterward. The `Done` channel remains correctly tracked, but the delivery snapshot says “not dispatched” while the same operation later dispatches.

The review-only reproducer was `TestReviewCanceledDisconnectDoesNotDispatchAfterKnownPending` in the detached review worktree at `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r6-acp-stdio-review-4b8c732/internal/acp/stdio_review_probe_test.go` (temporary file removed after validation). It uses real `io.Pipe` endpoints, drains the Connect frame, cancels a context before calling Disconnect, captures its pending error, then waits for the disconnect writer to enter the pipe. The test fails because `UnknownDelivery()` is false at return even though the writer starts afterward. Reproduce with:

```text
rtk proxy go test -race ./internal/acp -run '^TestReviewCanceledDisconnectDoesNotDispatchAfterKnownPending$' -count=1
```

The probe exited 1 with `Disconnect returned pending as known-not-dispatched, then started its disconnect Write`. The minimal setup is: wrap the `io.PipeWriter` with the existing `signalWriteAt{blockAt: 2}` helper (Connect is write 1); cancel a context before calling `Disconnect`; assert the returned pending result is not allowed to remain “known not dispatched” if write 2 subsequently enters. Release both pipe ends and wait for `Done` so the real reader/writer workers join.

Resolve this with one atomic queued/started/canceled arbitration shared by `pendingDisconnect` and the cleanup worker. Before a pending result reports known-not-dispatched, it must cancel the queued disconnect write and the worker must honor that state; if the worker commits dispatch first, the pending result must report unknown delivery. Preserve the blocked-reader-only known outcome, the already-started ordinary-send unknown outcome, and the existing retained cleanup owner.

### Follow-up validation

The changed test names were enumerated before execution. Repeated stdio race tests passed:

```text
rtk proxy go test -race ./internal/acp -run '^TestStdio' -count=20
ok github.com/pengjunchen/go-cli/internal/acp 9.333s
```

After removing the temporary probe, the full package race suite passed, and vet/diff checks were clean:

```text
rtk proxy go test -race -count=1 ./internal/acp
ok github.com/pengjunchen/go-cli/internal/acp 6.318s
rtk go vet ./internal/acp
rtk git diff --check 4e96a92c89c92dbe58ed3452b12d94ddf307c14d 4b8c7322cc8f0c6b813e67508e82093a239fc036
```

The new queued-disconnect probe is a separate exit-1 result against the fixed delta; it is not part of the full package run above.

## Fixed-chain follow-up: 9e06e2d → 22cc6e7 → d094307

**Reviewed source:** `d094307b1c79373ba099de5d06e2bb33f79f1619`, independently checked in a detached worktree. The reviewed lifecycle chain is `4e96a92c89c92dbe58ed3452b12d94ddf307c14d` → `4b8c7322cc8f0c6b813e67508e82093a239fc036` → `9e06e2d999fa3b713ebfb25c36802cc69ba6ff3c` → `22cc6e7fce5066a85ef58dd7d32a1af52be3b0d1` → `d094307b1c79373ba099de5d06e2bb33f79f1619`.

**Verdict:** accept the bounded stdio library lifecycle slice. The original four findings and R6-STDIO-5 are closed in this chain. This does not claim an inbound ACP stdio process command or clean shutdown for arbitrary caller-owned I/O.

R6-STDIO-5 is closed by arbitrating a queued disconnect write with the same adapter/result locks used to commit dispatch. An already-canceled Disconnect enters cleanup with `sendDisconnect=false`, so it still fences and cleans up but cannot enqueue a frame ([`stdio_adapter.go`](../../../../internal/acp/stdio_adapter.go:258)). For a deadline/cancellation while cleanup is pending, `pendingDisconnect` cancels a queued write before it can return known-not-dispatched; if the writer commits first, it reports unknown delivery ([`stdio_adapter.go`](../../../../internal/acp/stdio_adapter.go:785)). The tests cover pre-canceled disconnect producing no extra frame, active disconnect write reporting unknown, and queued cancellation being honored by the worker ([`stdio_adapter_test.go`](../../../../internal/acp/stdio_adapter_test.go:164), [`stdio_adapter_test.go`](../../../../internal/acp/stdio_adapter_test.go:190), [`stdio_lifecycle_test.go`](../../../../internal/acp/stdio_lifecycle_test.go:459)).

The d094 panic containment change is correctly bounded around the external I/O callbacks. Reader panic is converted to a static `StdioReaderPanicError`, starts cleanup, fences the session, and is surfaced after the reader worker unwinds ([`stdio_adapter.go`](../../../../internal/acp/stdio_adapter.go:515), [`stdio_adapter.go`](../../../../internal/acp/stdio_adapter.go:567)). Writer panic becomes a static `StdioWritePanicError` implementing `UnknownDelivery`; the send path records it, fences later sends, and closes its `Done` only after `Write` unwinds and the worker releases the gate ([`stdio_adapter.go`](../../../../internal/acp/stdio_adapter.go:406), [`stdio_adapter.go`](../../../../internal/acp/stdio_adapter.go:424), [`stdio_adapter.go`](../../../../internal/acp/stdio_adapter.go:836)). Owned closer panic is converted to a static `StdioCloserPanicError`; cleanup records that error only after `Close` unwinds, then waits for the actual session workers before closing the cleanup channel ([`stdio_adapter.go`](../../../../internal/acp/stdio_adapter.go:646), [`stdio_adapter.go`](../../../../internal/acp/stdio_adapter.go:659)). The exported pending-error contract explicitly says that a recovered closer panic does not prove the underlying resource closed ([`stdio_adapter.go`](../../../../internal/acp/stdio_adapter.go:59)). Panic values and stacks are not interpolated into any of these returned errors.

The committed subprocess tests verify that reader/writer/closer panics do not crash the test process, that the writer is fenced and reported as unknown, that repeated Disconnect returns the same static terminal error without retrying `Close`, and that the receive stream closes on reader panic ([`stdio_panic_test.go`](../../../../internal/acp/stdio_panic_test.go:20)). I also used temporary barrier probes that blocked inside each callback before panicking. Under the race detector, each pending `Done` stayed open while its callback was blocked; after a closer panic unwound, cleanup still stayed pending while an independent caller-owned pipe read had not returned. The probe source and output are preserved at [`r6-acp-stdio-d094-blocking-probes.go.txt`](../evidence/r6-acp-stdio-d094-blocking-probes.go.txt) and [`r6-acp-stdio-d094-blocking-probes-race.txt`](../evidence/r6-acp-stdio-d094-blocking-probes-race.txt). This distinguishes “callback goroutine returned” from “owned session workers joined” and makes no resource-closure claim for a panicking closer.

Caller-owned readers and writers remain open until their owner releases them. The committed pipe tests return a pending handle for a blocked read/write, preserve unknown delivery for a started write, avoid replay, and close `Done` only after the peer action makes the actual I/O return ([`stdio_lifecycle_test.go`](../../../../internal/acp/stdio_lifecycle_test.go:202), [`stdio_lifecycle_test.go`](../../../../internal/acp/stdio_lifecycle_test.go:341)). The owned-pipe test demonstrates that an explicitly supplied closer can interrupt and join the worker. The adapter has one bounded reader and one writer gate shared by ordinary and disconnect frames.

Validation on the fixed snapshot passed:

- Test names were enumerated before execution: `rtk env GOMAXPROCS=2 go test -list '^TestStdio' ./internal/acp`; the list is preserved at [`r6-acp-stdio-d094-test-list.txt`](../evidence/r6-acp-stdio-d094-test-list.txt). All stdio tests, including panic coverage, passed under `-race -count=3`: [`r6-acp-stdio-d094-target-race.txt`](../evidence/r6-acp-stdio-d094-target-race.txt).
- The three independent blocking-callback probes passed under race `-count=3`: [`r6-acp-stdio-d094-blocking-test-list.txt`](../evidence/r6-acp-stdio-d094-blocking-test-list.txt), [`r6-acp-stdio-d094-blocking-probes-race.txt`](../evidence/r6-acp-stdio-d094-blocking-probes-race.txt).
- Full `rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/acp -count=1` passed: [`r6-acp-stdio-d094-full-acp-race.txt`](../evidence/r6-acp-stdio-d094-full-acp-race.txt).
- `rtk env GOMAXPROCS=2 go vet ./internal/acp` and `rtk golangci-lint run ./internal/acp` passed: [`r6-acp-stdio-d094-vet.txt`](../evidence/r6-acp-stdio-d094-vet.txt), [`r6-acp-stdio-d094-lint.txt`](../evidence/r6-acp-stdio-d094-lint.txt).
- `rtk git diff --check 4e96a92..HEAD` passed.

The standard ACP `StdioAdapter` remains a library client over caller-provided `io.Reader`/`io.Writer`. These results do not establish an ACP stdio server, an OS-level process transport, or that a generic caller-owned callback can be interrupted by context alone.
