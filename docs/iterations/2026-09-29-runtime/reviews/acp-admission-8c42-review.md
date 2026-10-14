# ACP HTTP dispatch admission review

## Verdict

**ACCEPT, limited to the local ACP HTTP dispatch-admission contract.** I found no correctness issue in the reviewed source/test commit `8c4287db48f51fe37ceac97f50d87275dcfc3b0f` (parent `fb4d0cc216b3e121aa5afdec8597c7bd94e51a23`). The follow-up builder report is `50e62bdf4e1132a60f23d44b793b8c3d43a75bdc`.

## Review

- `httpServerGeneration.dispatchSlots` is created per generation at `maxConcurrency` capacity, separate from the HTTP request semaphore (`internal/acp/http_server.go:164-169,341-351`). This bounds detached processing after `/send` handlers release HTTP capacity.
- `/send` checks that the current generation is still admitting, reserves a dispatch slot, and increments `dispatches` while holding the same `s.mu` used by `beginStop` to close admission (`739-755`). A full slot channel returns 503 before starting `ProcessMessage`; the 200 response is written after scheduling (`762-789`). `CoreHandler.ProcessMessage` reaches the subagent factory only inside that worker (`core_handler.go:426-435,481`).
- The worker releases its slot and decrements the generation wait group only after `ProcessMessage` returns or its panic is recovered (`762-772`). Stop closes admission under `s.mu`; cleanup waits for request handlers and dispatch workers before closing `closeDone`, and `Start` refuses to create a new generation until that join is complete (`409-441,468-486,297-310`).
- The focused tests exercise over-capacity rejection with no additional factory call, slot release after completion and panic, Stop waiting for a blocked dispatcher, fresh-generation restart, and concurrent seal/send admission (`http_dispatch_admission_test.go:17-184`).

The lock ordering closes the admission/stop race: an admitted send records its dispatch before releasing `s.mu`, while a stop that wins the lock makes later sends fail before reservation. The generation wait cannot race a positive `WaitGroup.Add`, because stop first seals admission and the close owner waits for all request handlers before waiting for dispatches.

## Independent evidence

Reviewed in a clean detached worktree at `8c4287db48f51fe37ceac97f50d87275dcfc3b0f`; the builder worktree remained untouched. I ran:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=25 -run '^(TestACPDispatchAdmissionBounded|TestACPDispatchAdmissionStopJoinsAndRestartsFreshGeneration|TestACPDispatchAdmissionSealRacesWithSendsWithoutOversubscription|TestACPDispatchAdmissionSlotReleasesAfterWorkerPanic)$' ./internal/acp
ok github.com/pengjunchen/go-cli/internal/acp 2.545s
```

This is a focused race run; I did not repeat the full package or claim external ACP interoperability. No actionable findings.
