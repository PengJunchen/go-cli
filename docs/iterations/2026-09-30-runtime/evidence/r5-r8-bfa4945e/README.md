# R5 native PTY final-frame evidence

The source under test is commit `bfa4945e30ca50549abda89c4c86d6d2415600c0`. The binary was built from that commit with `go build -o /tmp/go-cli-r5-pty-bfa4945 ./cmd/cli`; the driver recorded the same full SHA in its summary. The driver exited 0 with nine authenticated requests to its local fake provider.

The Darwin PTY journey covered denied and once-only write approval, the unseen-details gate, resize through 20x4, 80x24, and 120x30, full approval-detail scrolling, an actual `ask_user` response, UTF-8 caret editing, multiline paste at 120x30, cancellation in a second PTY, durable transcript inspection, and `/exit`. The full raw stream and readable summary are preserved alongside the driver.

After the approved write, the driver captured the PTY bytes after the durable receipt and returned REPL prompt were observed. `r5-pty-success-frame.raw` is that stream prefix. Decoding it with pyte at 60x12 produced `r5-pty-final-visible-viewport.txt`, which shows the canonical successful receipt, the final assistant entry, and the prompt, with no live row. This final viewport does not show the `[saved: success]` badge, so it is not claimed here; the raw transcript records that status text during the turn, and the badge eligibility is separately covered by `TestTranscriptRefreshSavedRequiresExactQuiescentTerminalProof`.

The render-order regression is also covered in `internal/cli/repl_session_lifecycle_test.go`: cancel-before-wait fails because the context is canceled before `App.Done`; graceful `Quit` → `Done` → `cancelUI` passes. `go test -race ./internal/tui ./internal/cli -count=1` passed 1,388 tests before the source commit. `make check` passed on the same source content: formatting, `go vet ./...`, `golangci-lint run ./...` (0 issues), build, and full `go test -race -count=1 ./...`.

This evidence is one native PTY on macOS with a local deterministic fake provider. It does not claim crash/restart or cross-platform validation.
