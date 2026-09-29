# ACP HTTP dispatch admission builder report

## Snapshot and result

- Base: `fb4d0cc216b3e121aa5afdec8597c7bd94e51a23`.
- Source and focused-test commit: `8c4287db48f51fe37ceac97f50d87275dcfc3b0f` (`fix(acp): bound HTTP dispatch admission`).
- Worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-acp-admission`.
- Source scope: `internal/acp/http_server.go`; test scope: new `internal/acp/http_dispatch_admission_test.go`.

Each HTTP server generation now owns a bounded dispatch-slot channel sized from the existing `maxConcurrency` value (default 64). The channel is separate from the request-handler semaphore. `/send` reserves a slot and increments the generation dispatch wait group while holding the same server mutex used by `BeginStop`, `SealAdmission`, and `Stop` to close admission. If no slot is available, `/send` returns HTTP 503 before it creates the detached `ProcessMessage` worker. An admitted slot is released only after `ProcessMessage` returns or its panic has been contained; the wait-group completion follows slot release. A restart creates a new generation with its own slots and is still blocked until the old generation's admitted work joins.

This bounds local admitted dispatch work. It does not establish a remote side-effect outcome or claim official ACP interoperability.

## RED/GREEN evidence

The RED fixture sends multiple real HTTP `/send` requests through `HTTPServer`, `CoreHandler`, and `DefaultSubagentDispatcher` to a counted factory and gate-held subagents. At capacity two, the first two requests held their dispatches. With `http_server.go` restored to the base implementation, the third request returned 200 instead of 503 and the factory count reached three. The baseline command exited 1:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -p 2 -count=1 -run '^TestACPDispatchAdmissionBounded$' -v ./internal/acp
over-capacity send status=200 factory creates=3
FAIL: expected 503, actual 200
```

After implementation, the same bounded-admission fixture logged `over-capacity send status=503 factory creates=2`; focused release, stop/join/restart, seal-race, and panic-release tests passed. Repeating them ten times under the race detector also passed:

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -p 2 -count=1 -run '^(TestACPDispatchAdmissionBounded|TestACPDispatchAdmissionStopJoinsAndRestartsFreshGeneration|TestACPDispatchAdmissionSealRacesWithSendsWithoutOversubscription|TestACPDispatchAdmissionSlotReleasesAfterWorkerPanic)$' -v ./internal/acp
PASS

rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=10 -run '^(TestACPDispatchAdmissionBounded|TestACPDispatchAdmissionStopJoinsAndRestartsFreshGeneration|TestACPDispatchAdmissionSealRacesWithSendsWithoutOversubscription|TestACPDispatchAdmissionSlotReleasesAfterWorkerPanic)$' ./internal/acp
ok github.com/pengjunchen/go-cli/internal/acp 1.726s
```

## Validation

```text
rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -p 2 -race -count=1 ./internal/acp
ok github.com/pengjunchen/go-cli/internal/acp 27.451s

rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -p 2 -race -count=1 -run '^(TestServe.*|TestAssemblyCloseConsumesRealACPPendingCleanupSignal|TestAssemblyCloseJoinsACPServerDispatchBeforeClosingEventBus|TestAssemblyCloseSealsRegisteredACPServerBeforeStart)$' ./internal/cli
ok github.com/pengjunchen/go-cli/internal/cli 1.943s

rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go vet ./internal/acp ./internal/cli
# exit 0

rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb golangci-lint run --allow-serial-runners ./internal/acp/...
0 issues.

rtk git diff --cached --check
# exit 0
```

The lint flag only permits waiting on golangci-lint's existing lock; it changes no project lint rules. No CLI, core, MCP, or prior ledger source was modified. No public serving journey, remote ACP peer, or official ACP interoperability was exercised or claimed.
