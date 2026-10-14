# R5 real-PTY review

**Result: blocked at fixed source 84324fe7a55ea852583f5082fe5d76ac0dd0608a.** The actual interactive binary exposed two input correctness failures: the first key after submitting a REPL line is lost when the TUI starts, and spaces disappear from free-text ask_user answers. The journey could continue only after retrying approval keys; those retries are diagnostic and do not count as a passing user flow.

## Snapshot and method

- Detached source worktree: /Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r5-pty-review-84324fe, HEAD 84324fe7a55ea852583f5082fe5d76ac0dd0608a, clean at review end.
- Built the real ./cmd/cli executable from that worktree with:

  rtk env GOMAXPROCS=2 go build -o /tmp/go-cli-r5-pty-84324 ./cmd/cli

- Ran the driver from the main checkout with:

  rtk env PYTHONDONTWRITEBYTECODE=1 python3 docs/iterations/2026-09-25-runtime/evidence/r5-pty-driver.py --binary /tmp/go-cli-r5-pty-84324 --evidence-dir docs/iterations/2026-09-25-runtime/evidence

- The driver allocates a real Unix PTY, starts the CLI with no subcommand, runs an ephemeral fake OpenAI-compatible SSE server bound only to 127.0.0.1, and isolates HOME, all XDG directories, CWD, TMPDIR, and PATH. The only configured API credential is a synthetic canary. The fake provider received 8 requests, all on /v1/chat/completions, all canary-authenticated; it records only an auth_ok boolean, never the credential.
- The temporary config restricts allowed paths to the temporary workspace. Denied and canceled writes left no files; the once-approved write exactly matched its synthetic payload. The temporary home, config, and workspace were deleted when the run ended.
- The driver exits 1 intentionally when the observed input defects occur. The CLI itself exited 0 through /exit.

## Confirmed findings

**PTY-01 — The first key after each submitted REPL line does not reach the TUI.** After the fake provider raised two concurrent write approvals, the real 60×6 screen showed local interaction #1 · 1 waiting. The first q produced no visible state change during the bounded wait; a second q canceled the active and queued approvals and printed [interrupted]. In separate turns, the first n had no effect and the second denied the write; the first y on the ask_user tool approval had no effect and the second opened the question. The TUI termios state was raw (ICANON, ECHO, and ISIG all off), ruling out canonical line buffering.

The source has a strong matching cause: [line_editor.go](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r5-pty-review-84324fe/internal/cli/line_editor.go:559) starts a goroutine that blocks in le.in.Read(buf); after Enter returns, the deferred closeDone only closes a channel and does not interrupt that read ([line_editor.go](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r5-pty-review-84324fe/internal/cli/line_editor.go:563), [line_editor.go](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r5-pty-review-84324fe/internal/cli/line_editor.go:567), [line_editor.go](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r5-pty-review-84324fe/internal/cli/line_editor.go:586)). The interactive Bubble Tea program also reads os.Stdin when no alternate input reader is supplied ([app.go](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r5-pty-review-84324fe/internal/tui/app.go:348), [app.go](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r5-pty-review-84324fe/internal/tui/app.go:361)). That leaves a prior line-editor read competing with Bubble Tea for the next byte. The PTY behavior is confirmed; the exact goroutine that wins the read is inferred from this source chain.

Impact: approval, cancellation, and HITL controls can ignore the first user key after a REPL submission. Repeating a decision key is not an acceptable recovery contract because it can make the user's first intended decision appear lost.

**PTY-02 — Free-text ask_user drops ordinary spaces.** The PTY sent X保留队列：交互通过 ✅ 🙂. The HITL view and the subsequent provider tool receipt both contained X保留队列：交互通过✅🙂; both ASCII spaces disappeared. The ask_user tool itself first displayed an approval request, which needed a once approval before the question appeared. The no-options input handler in [app.go](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r5-pty-review-84324fe/internal/tui/app.go:1952) handles Enter, backspace, and tea.KeyRunes, but has no space-key case at [app.go](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r5-pty-review-84324fe/internal/tui/app.go:1961). The wire-level mismatch is confirmed by the fake provider's receipt; the handler branch is the source-level evidence.

Impact: free-text answers silently change before the agent receives them, including natural-language instructions and code/config fragments containing spaces. The relevant fixed-snapshot handler is [app.go](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r5-pty-review-84324fe/internal/tui/app.go:1952), with the runes-only branch at [app.go](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r5-pty-review-84324fe/internal/tui/app.go:1961).

## Interaction checks observed

- **Queue and cancellation:** queue count was visible at 60 columns. After retrying the first q, cancellation returned to the REPL and both pending write files remained absent.
- **Deny:** at 20×4, the compact modal displayed its locked decision row. After retrying the first n, the denied file remained absent.
- **Narrow-screen review gate:** at 20×4, two early y attempts did not create the file or trigger another provider request. One attempt was consumed by the first-key issue; the next reached the still-locked modal, so this is evidence that the gate remained closed in that state, not a clean single-key journey.
- **Resize and long details:** resizing the active modal from 20×4 to 60×12 displayed the wrapped payload. Actual down-arrow events reached the last page (details 23-29/29); only then did y create once.txt, whose contents matched the approved synthetic payload byte-for-byte.
- **Unicode caret:** the real line editor transformed 测试🙂B with left, insert X, left, backspace into the provider input 测试XB. The CLI warned that CJK input methods may not work in raw mode; this check covers Unicode key input and grapheme deletion, not OS IME composition.
- **TTY restore:** /exit returned status 0. PTY cooked-state flags matched the original terminal: before and after masked ICANON|ECHO|ISIG were both 392; active raw mode was 67.

## Evidence and limits

- [r5-pty-driver.py](../evidence/r5-pty-driver.py) is the re-runnable real-PTY fixture.
- [r5-pty-transcript.raw](../evidence/r5-pty-transcript.raw) preserves terminal control sequences and raw interaction bytes.
- [r5-pty-transcript.txt](../evidence/r5-pty-transcript.txt) is the ANSI-stripped text stream; it is not a terminal-screen emulator.
- [r5-pty-summary.txt](../evidence/r5-pty-summary.txt) records the fixed hash, fake-provider requests, file outcomes, key retries, terminal state, and deliberate failure result.

This review covers one Darwin/arm64 PTY and the default interactive CLI path at the fixed hash. It does not claim Windows/Linux behavior, IME composition, durable session recovery, crash recovery, or R2 CLI wiring. The two input findings were handed to the UI builder; this report is the independent pre-fix evidence and must remain blocked until a new fixed hash passes the same real-binary interaction.
