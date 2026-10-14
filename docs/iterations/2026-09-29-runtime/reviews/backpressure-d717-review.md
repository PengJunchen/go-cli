# Independent review: TUI send backpressure `d7175f2`

Reviewed commit `d7175f2f34199b191ccc75c4376a2e3634d04547` (`fix(tui): report send backpressure outcomes`) from base `17ab7b76ef0b7636abff1e5a055de7acf6f6bb31` in fresh detached worktree `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260929-backpressure-review-d717`.

## Decision

**ACCEPT for the `BubbleteaApp` local send-queue contract.** No P1/P2 source defect was found in the reviewed queue admission, bounded wait, shutdown, and run-generation behavior. This does not claim the default CLI bridge was wired to the new API or that queue admission is a processing/durable receipt.

## Contract review

- `Send` serializes admission with `Quit` under `sendMu`. A successful send means the message entered `msgCh`; queue-full and closed outcomes are typed errors. `Quit` sets `quitRequested` and closes `quitCh` under the same mutex, so a concurrent send is either admitted before Quit or rejected after it.
- `CriticalSendContext` checks cancellation before and under the admission lock, returns immediately on a full pre-Run queue, and waits synchronously on context cancellation, the captured active run's `Done`, Quit, or the coalesced `queueSpace` notification. It does not start a waiter goroutine. After a wake it loops through the old captured run channel check before attempting another enqueue, so a sender blocked in an ended generation cannot use a newly available slot in a later generation.
- The temporary probe made the old run's done channel and queue-space notification ready together while the sender was held from reacquiring `sendMu`, then installed a new active generation. The old sender returned `ErrAppClosed` and did not enqueue into the new generation. The app's normal restart test also verifies that a fresh critical sender can be accepted after a natural run completion.
- Repeated pre-canceled calls with both empty and full queues never enqueued a message. The committed quit-before-Run tests reject `Send` and `CriticalSendContext` even when space is available. The concurrent Send/Quit test accepts only the two linearized outcomes.
- The queue and `queueSpace` notification channel have no production close site, so the app does not send to a closed `msgCh`. `waitForMsg` exits on the run's `Done` channel; critical senders unblock on their own context, Quit, or the captured generation ending.
- Rejected attempts increment `localSendGapCount`, and the rendered status identifies a local TUI queue gap. The counter is separate from Core `EventGapCount` and is not cleared by a status event. Successful admission is not described as processing or persistence acknowledgment.
- The source search found no production `BubbleteaApp.Send` callsite. `internal/cli/interactive.go` calls Bubble Tea's separate `Program.Send` for HITL messages. Therefore this commit does not establish that a production bridge now retries or routes critical status/interaction messages through `CriticalSendContext`.

The reviewed boundary matches R5's requirement that a full TUI queue produce a visible error/gap (`r5-ui-design.md:113-126`) while keeping R5 UI projection separate from Runtime and durable receipt authority (`r5-r8-contract-review.md:46-49`).

## Independent verification

- `go test -list '^(TestTUISend|TestTUICriticalSend)' ./internal/tui` confirmed the 13 focused tests.
- `go test -race -count=1 ./internal/tui -run '^(TestTUISend|TestTUICriticalSend)'`: passed all 13.
- Temporary pre-canceled empty/full queue and old-generation/done-plus-space probes passed under race; the probe file was removed.
- `go test -race -count=1 ./internal/tui`: passed all 563 tests.
- `go vet ./...`: passed, exit 0.
- `git diff --check HEAD^ HEAD`: passed.

The builder report additionally records a scoped `golangci-lint` pass for `./internal/tui ./internal/cli`; lint was not rerun independently here. No source files were changed, and no app-bridge or receipt guarantee is claimed.
