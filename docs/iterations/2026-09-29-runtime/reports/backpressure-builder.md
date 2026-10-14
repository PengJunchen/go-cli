# TUI send backpressure builder report

## Scope

Implemented the bounded-message-queue backpressure contract for `BubbleteaApp` on branch `codex/repair-20260929-backpressure`, based on `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31`.

`Send(Msg) error` now reports queue-full and closed-app failures through typed errors (`ErrSendQueueFull` and `ErrAppClosed`). A successful return means only that the message was admitted to the local TUI queue; it is not a processing acknowledgment or a durable receipt. `CriticalSendContext` waits for queue space only while its captured Run generation is active, and exits on context cancellation, Quit, or that generation ending. A full queue before Run starts returns immediately. Send admission and Quit are serialized, Quit-before-Run permanently rejects sends, and a natural Run completion remains restartable.

Rejected message attempts increment a TUI-local gap counter and the rendered view labels it as a local queue gap. This count is observational UI state; it is separate from Core `EventGapCount` and makes no claim about event receipts, canonical state, or persistence. Existing modal interaction request/response and cancellation channels were left intact.

The initial behavior-first checks failed against the baseline because `Send` had no result and the critical context API was absent. Regression coverage now includes queue saturation and burst counts, closed sends, pre-canceled contexts, pre-Run full-queue behavior, concurrent Send/Quit admission, and an old blocked sender racing Run completion, queue-space release, and restart. The restart test verifies the old sender cannot leak its message into a new generation while a fresh sender can use the restarted app.

## Callsite audit

The source audit found no production callsites of `BubbleteaApp.Send`; its direct callsites are tests. `internal/cli/interactive.go` uses Bubble Tea's `Program.Send` for HITL messages, which is a separate API and was not changed. Consequently this API change does not establish that a production bridge or CLI caller now retries critical snapshots, status, or interaction lifecycle messages. No shared CLI files were edited.

## Validation

- `rtk proxy go test ./internal/tui -run 'TestTUISend|TestTUICriticalSend' -count=1 -v` — all 13 backpressure tests passed.
- `rtk go test -race ./internal/tui -count=1` — passed, 563 tests.
- `rtk go test -race ./internal/cli -run 'TestPhase39_REPLSessionCompleteTurn|TestPhase39_HITLApprovalChannel|TestHITLQuestionCancellationRemovesRealTUIWait|TestInteractiveCmd_RunConversation|TestE2E_NonTTYApprovalDenied' -count=1` — passed, 5 selected tests.
- `rtk go vet ./internal/tui ./internal/cli` — passed.
- `rtk proxy golangci-lint run --allow-parallel-runners --concurrency 2 ./internal/tui ./internal/cli` — exit 0, `0 issues` (scoped run; integrated lint remains for the root integration gate).
- `rtk git diff --check` — passed.

Full integrated lint/verify and any production bridge wiring remain outside this scoped change.
