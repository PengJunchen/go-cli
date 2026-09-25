# R5 TTY handoff and PTY follow-up review

**Verdict: accept this four-commit slice on the fixed snapshot.** I found no remaining blocker in the reviewed input handoff, approval interaction, cooked-mode handoff, or read-error propagation paths. This is a scoped review of the R5 interaction fixes; it does not certify all R5/R8 work, durable-session recovery, or Windows interactive TTY support.

## Fixed source and review method

Reviewed an independent detached worktree at `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r5-pty-final-review-05f6902`, HEAD `05f6902c6d017210024a053e1754ff153607614d`, clean after source review. The reviewed stack is:

- `7ab3735f637534b51ca0c1080c059e320c7e2e2b` — hand input ownership between the raw TTY reader and the next input owner.
- `c5e3380893f897324d6348e9b9de0d62f80ab5e0` — preserve space keys in free-text `ask_user` answers.
- `c48ec42152cdbc306c4a3de5ecc680cc8cfe8eae` — avoid duplicate cancel readers and join active reads before restoring terminal ownership.
- `05f6902c6d017210024a053e1754ff153607614d` — propagate non-normal REPL input errors after cleanup.

I reviewed the source diffs and tests, ran targeted and package race checks, built the fixed CLI binary, and exercised its real interactive path under a Darwin PTY against a loopback-only synthetic OpenAI-compatible provider. The driver removes `CI`, isolates `HOME`, XDG paths, `CWD`, `TMPDIR`, and `PATH`, and uses a synthetic credential; the fixture records only `auth_ok`, never the credential value.

## Source findings

`newTTYByteReader` checks that input is a terminal file and that the platform helper allows it, then creates exactly one `cancelreader.NewReader` (`internal/cli/line_editor.go:543-561`). This removes the old probe-reader/real-reader double construction. On Windows the helper rejects TTY use before `NewReader` can run (`internal/cli/line_editor_cancel_windows.go:7-10`), avoiding the backend’s console-input flush side effect. Linux and Darwin have explicit platform helpers; Darwin additionally bounds `/dev/tty` descriptors to `FD_SETSIZE`.

The reader marks a read admitted under `stateMu` immediately before entering `Read`, and clears that state after `Read` returns (`internal/cli/line_editor.go:580-628`). `stop` closes admission, cancels only an active read, waits for a non-interruptible admitted read to return, then joins the worker and closes the cancel reader (`:657-683`). This closes the race where shutdown used to restore terminal state while a reader could still consume a byte. Idle stop does not call `Cancel`.

Ctrl+\\ stops and joins the raw reader before restoring the saved terminal state and passing subsequent input to the cooked scanner (`internal/cli/line_editor.go:701-728`, with the toggle at `:810-813`). The `ask_user` model now appends `tea.KeySpace` as an actual space (`internal/tui/app.go:1952-1965`). In the REPL, EOF and context cancellation remain normal exits, while other reader errors return; `Run` cleans up and returns that error (`internal/cli/repl_session.go:126-139`, `:521-535`). `runCLI` writes non-usage errors to stderr and returns status 1 (`cmd/cli/main.go:149-160`). The source chain is sound. Existing tests exercise the REPL error and cleanup boundary; there is no single end-to-end test that injects an unsupported-platform TTY error through `runCLI` itself.

## Verification

The exact targeted test names were listed before execution. The CLI race filter covered `TestNewTTYByteReaderRejectsUncancelableInput`, all three `TestTTYByteReaderStop...` cases, `TestReadLineTTYReleasesFirstKeyForNextOwner`, `TestReadLineTTYContextCancelReleasesReaderAndRestoresMode`, `TestReadLineTTYCookedToggleJoinsRawReaderBeforeScanner`, `TestInteractiveRunReturnsTTYInputSetupErrorAfterCleanup`, and the three `TestREPLReadInput...` cases. The TUI target was `TestTUIAskUserFreeTextPreservesSpaceKeys`.

Commands run from the fixed review snapshot:

```text
rtk env -u CI GOMAXPROCS=2 go test -p 2 -race -count=10 ./internal/cli -run '^(TestNewTTYByteReaderRejectsUncancelableInput|TestTTYByteReaderStopSkipsCancelWhenIdle|TestTTYByteReaderStopCancelsAndJoinsActiveRead|TestTTYByteReaderStopCancelsBeforeUnderlyingReadEntry|TestReadLineTTYReleasesFirstKeyForNextOwner|TestReadLineTTYContextCancelReleasesReaderAndRestoresMode|TestReadLineTTYCookedToggleJoinsRawReaderBeforeScanner|TestInteractiveRunReturnsTTYInputSetupErrorAfterCleanup|TestREPLReadInputPropagatesLineEditorError|TestREPLReadInputKeepsContextCancellationAsNormalExit|TestREPLReadInputKeepsInterruptRetryAndEOFBehavior)$'
rtk env -u CI GOMAXPROCS=2 go test -p 2 -race -count=10 ./internal/tui -run '^TestTUIAskUserFreeTextPreservesSpaceKeys$'
rtk env -u CI GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/cli ./internal/tui
rtk env -u CI GOMAXPROCS=2 go test -p 2 -race -count=1 ./cmd/cli -run '^(TestRunSuccessNoArgs|TestRunUsageErrorExitCode2)$'
rtk env GOMAXPROCS=2 go build -o /tmp/go-cli-r5-pty-05f6902 ./cmd/cli
rtk env -u CI PYTHONDONTWRITEBYTECODE=1 python3 docs/iterations/2026-09-25-runtime/evidence/r5-pty-driver-05f6902.py --binary /tmp/go-cli-r5-pty-05f6902 --evidence-dir docs/iterations/2026-09-25-runtime/evidence
rtk git diff --check 7d28146..05f6902c6d017210024a053e1754ff153607614d
```

All targeted races, the full `internal/cli` and `internal/tui` race run, the selected `cmd/cli` caller checks, build, and diff check passed.

The real PTY driver passed with exit status 0 and nine loopback provider requests. It observed the first `q` cancel active plus queued approvals (neither file was written), the first `n` reject without writing, an early `y` remain locked at 20x4, and approval unlock only after scrolling through wrapped detail to row 29/29. The approved write exactly matched the synthetic payload. `ask_user` delivered `X保留队列：交互通过 ✅ 🙂` including both spaces; UTF-8 cursor movement/backspace submitted exactly `测试XB`. Ctrl+\\ restored canonical terminal flags before cooked scanner input, and the following `PTY cooked handoff` line reached the fake provider. `/exit` returned 0 and `ICANON`, `ECHO`, and `ISIG` matched their initial PTY values.

The Windows cross-compile was attempted with:

```text
rtk env GOOS=windows GOARCH=amd64 GOMAXPROCS=2 go test -p 2 -c -o /tmp/go-cli-r5-pty-05f6902-windows.test.exe ./internal/cli
```

It stops before reaching this slice because existing `internal/approval/callback.go:245` and `internal/approval/trust.go:178` refer to undefined `syscall.Stat_t`. Therefore Windows compilation/runtime is unverified here. The R5 code explicitly disables interactive TTY input on Windows rather than claiming bounded cancellation there. The release workflow currently targets Linux and Darwin.

## Evidence and boundaries

The rerunnable driver and its fixed-hash output are new files, preserving the earlier 84324 pre-fix failure evidence:

- `docs/iterations/2026-09-25-runtime/evidence/r5-pty-driver-05f6902.py`
- `docs/iterations/2026-09-25-runtime/evidence/r5-pty-05f6902-summary.txt`
- `docs/iterations/2026-09-25-runtime/evidence/r5-pty-05f6902-transcript.raw`
- `docs/iterations/2026-09-25-runtime/evidence/r5-pty-05f6902-transcript.txt`

The PTY evidence is one Darwin/arm64 run with a local fake provider. It does not establish Windows cancellation, non-Darwin terminal behavior, IME composition correctness, crash/restart recovery, or full R5/R8 closure.
