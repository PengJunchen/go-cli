# R6 ACP replay/live subscription review

Reviewed fixed snapshot: `6586e7e4e75b07f09bc2040c9da92200fa23fcbf` (`internal/acp/core_handler.go`, `http_server.go`, and their focused tests). This review covers the single-session `SubscribeAfter` replay/live slice only. It does not approve full ACP admission, durable task ownership, R1 run integration, or serve lifecycle.

## Findings — changes required

1. **P1: Overflow gap can be omitted when the session closes while the SSE writer is blocked.** Overflow closes `subscriber.done` and records a gap but leaves `subscriber.events` open (`core_handler.go:221-241`). `Session.Close` subsequently closes both channels (`:150-164`). In the HTTP loop, the `Events()` case returns immediately when it reads a closed channel (`http_server.go:609-612`), while only the `Done()` case drains the prefix and writes `event_gap` (`:616-628`). If the writer is blocked on a live event while overflow occurs and the session then closes, both select cases become ready. Once buffered entries are drained, the select is permitted to choose the closed `Events()` case and return without reporting the already-recorded gap.

   **Probe and limits:** a temporary package-internal overlay drove the actual `handleEvents` path with a blocked SSE writer, induced overflow, then called `Session.Close` before releasing the writer. With the production buffer size of 64, 64 repetitions did not hit the rare all-buffered-events-then-closed-channel selection path; a separate 1,000-iteration select probe confirmed that Go can choose the closed `Events()` receive when `Done()` is also closed. To make the end-to-end branch reproducible, a test-only overlay changed only `eventSubscriberBufferSize` from 64 to 1; the same HTTP sequence then observed missing `event_gap` frames in repeated iterations. This is a probability-amplified reproduction, not a claim that the default-capacity case was observed. The smallest safe fix is to route `Events()` closure through the same final drain/gap emission logic as `Done()` (or otherwise ensure the recorded gap is emitted before return); do not rely on select preference.

   The test-only reproduction passed because it asserted the current lost-gap behavior, not because it validated a repair:

   ```text
   rtk env GOMAXPROCS=2 go test -overlay=/tmp/r6_acp_review_overlay.json -p 2 -race -count=1 -run '^TestReviewSSEOverflowThenSessionCloseGap$' -v ./internal/acp
   ```

2. **P1: A future numeric `Last-Event-ID` is accepted and the stream later moves backwards.** The HTTP handler rejects malformed or overflowing decimal input, then calls `SubscribeAfter(afterID)` (`http_server.go:568-583`). The subscription snapshot does not check whether `afterID` exceeds `eventSeq` (`core_handler.go:273-327`). A real handler probe with one existing event and `Last-Event-ID: 500` returned HTTP 200 with no reset/gap, then emitted the next live event as `id: 2`. That silently violates the cursor's ordering for this session and can make a reconnecting client discard or replay the wrong suffix.

   Reject the ahead cursor explicitly (for example, typed `ErrCursorAhead` surfaced as HTTP 400), or return an explicit reset/gap state. Check it under the same `Session.mu` critical section as snapshot and subscriber registration; a separate sequence read followed by `SubscribeAfter` would introduce a check/cutover race. The current behavior is reproduced by this temporary overlay test against the unmodified production buffer/source; it passes because it asserts the bad response:

   ```text
   rtk env GOMAXPROCS=2 go test -overlay=/tmp/r6_acp_review_overlay_default.json -p 2 -race -count=1 -run '^TestReviewHTTPFutureLastEventIDProducesLowerLiveID$' -v ./internal/acp
   ```

## Verified behavior

- `SubscribeAfter` takes the same session mutex as `PublishEvent`, copies the retained suffix and `LastSeq`, and registers that subscriber before unlocking (`core_handler.go:268-298`). Publication assigns one ID and offers the same `EventEntry` to history and each subscriber under that lock. The focused concurrent cutover test checks a contiguous, duplicate-free replay/live union.
- Each subscription has its own bounded channel. Overflow does not block publishers, records the first missing ID, closes `Done`, and subsequent publishes continue to the ring without being offered to that subscriber. Multiple subscribers and separate sessions are isolated by the per-session subscriber map; the HTTP route no longer reads a process-wide `EventBus`.
- `EventSubscription.Close` unregisters under the session mutex and is safe against concurrent publish/close. The subscription adds no goroutine; request cancellation returns from the HTTP select and deferred `Close` detaches it.
- Retention gaps carry the requested cursor and available/latest range. Events continue to be appended to the bounded 256-entry session ring after one subscriber overflows, so a client can resume the missing suffix from the emitted gap cursor while that suffix remains retained. This slice does not create a separate priority terminal slot or claim durable task-status retention; the later task/run owner remains outside scope.

## Validation

I enumerated and independently ran these 20 focused tests with `-race`; all passed:

1. `TestSessionSubscribeAfterAtomicReplayLiveCutover`
2. `TestSessionSubscribeAfterCutoverPublishesNextEntryOnlyLive`
3. `TestSessionSubscribeAfterReportsRetentionGapAndPreservesIDs`
4. `TestSessionSubscribersAreIsolatedAndSlowOverflowDoesNotBlockPublish`
5. `TestSessionSubscriptionsDoNotCrossSessions`
6. `TestSessionSubscriptionCloseRacesWithPublish`
7. `TestSSEStream_OverflowReportsGapAfterContiguousPrefix`
8. `TestSSEWriter_WriteEvent`
9. `TestSSEWriter_WriteComment`
10. `TestSSEStream`
11. `TestSSEStream_MethodNotAllowed`
12. `TestSSEStream_MissingSenderID`
13. `TestSSEStream_SessionNotFound`
14. `TestSession_PublishEventAfterClose`
15. `TestLastEventID`
16. `TestSSEStream_ExpiredCursorAnnouncesEventGap`
17. `TestHTTPServer_StreamAllSessions`
18. `TestHTTPServer_AuthStreamBoundToSubject`
19. `TestHTTPServer_AuthEventsBoundToSubject`
20. `TestHTTPServer_SessionTTL`

Command:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^(TestSessionSubscribeAfterAtomicReplayLiveCutover|TestSessionSubscribeAfterCutoverPublishesNextEntryOnlyLive|TestSessionSubscribeAfterReportsRetentionGapAndPreservesIDs|TestSessionSubscribersAreIsolatedAndSlowOverflowDoesNotBlockPublish|TestSessionSubscriptionsDoNotCrossSessions|TestSessionSubscriptionCloseRacesWithPublish|TestSSEStream_OverflowReportsGapAfterContiguousPrefix|TestSSEWriter_WriteEvent|TestSSEWriter_WriteComment|TestSSEStream|TestSSEStream_MethodNotAllowed|TestSSEStream_MissingSenderID|TestSSEStream_SessionNotFound|TestSession_PublishEventAfterClose|TestLastEventID|TestSSEStream_ExpiredCursorAnnouncesEventGap|TestHTTPServer_StreamAllSessions|TestHTTPServer_AuthStreamBoundToSubject|TestHTTPServer_AuthEventsBoundToSubject|TestHTTPServer_SessionTTL)$' ./internal/acp
```

The future-cursor probe passed against the production buffer and source; the overflow-close probe passed only with the test-only capacity reduction described above. No full ACP suite was repeated.

**Verdict: reject `6586e7e4e75b07f09bc2040c9da92200fa23fcbf` pending the two findings above.**


## Root independent delta review at `3c7dcbec8d3548dedc37543a35949a642646e963`

Accepted for this bounded replay slice. The source now validates a future cursor under the same Session mutex as publication and subscriber registration, returning typed ErrEventCursorAhead and HTTP 400 without registering. Both closed Events and Done branches use one detach/drain/frozen-gap function, so select choice cannot suppress the overflow gap. Root reviewed the four-file delta and enumerated the relevant tests before execution.

Production capacity 64: targeted SubscribeAfter and HTTP overflow/future-cursor tests passed under race with count 20 (1.186s). A temporary Go overlay reduced only the subscriber capacity to 1 to amplify the prior close/overflow select branch; the overflow-then-close regression passed under race with count 40 (1.025s). This overlay is test-only probability amplification, not a claim that the original defect was reproduced at capacity 64. No source files were modified by the probe. Evidence: [root delta race log](../evidence/r6-acp-3c7dcb-root-review.log).

The builder separately passed the full ACP race suite and ACP lint on this hash. Task identity/admission, remote approval, serve lifecycle, and durable session integration remain outside this acceptance.
