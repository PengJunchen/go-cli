# R8 guarded-output PTY evidence at d5052

This is a targeted baseline acceptance run against source commit `d5052d5612907f9fe073942ea7a7004b27c39958`, built in the isolated worktree. It is not the final integrated release gate; rerun the same three PTY journeys after the R2, R3, and R6 integrations.

The default output guard is whole-response based. While a response is held, the TUI shows only “Assistant response is being generated and checked”; the candidate text remains absent. After the guard completes, the CLI publishes the sanitized answer once. This run found no R8 defect requiring a production change.

## Acceptance results

- **Held success and redaction:** the loopback provider flushed the candidate in deltas, splitting a synthetic AWS-key-shaped canary across two chunks. Before release, neither candidate text nor canary appeared in the PTY frame or session receipt. After release, the visible and persisted answer was `R8-GUARDED-PREFIX [REDACTED] R8-GUARDED-SUFFIX`, with no raw canary. The current frame contained one sanitized assistant row; the status cleared; `/exit` returned 0 and restored `ICANON`, `ECHO`, and `ISIG` (flags 392 before and after).
- **Esc cancellation:** standalone `Esc` canceled the guarded turn. The loopback provider observed the client socket close. The durable-session CLI stayed alive, returned to the REPL prompt, cleared status, and persisted neither the candidate nor a final answer. `/exit` returned 0 and restored the same terminal flags.
- **Long transcript navigation:** after 24 completed turns, the next provider response was held before any content. `PageUp` moved from the tail to `R8-ANSWER-18`; the visible anchor survived resize at 60, 80, 120, and 200 columns. `End` returned to completed `R8-ANSWER-23` while the held answer remained unreleased. The answer arrived and persisted only after the viewport checks.

The cancellation check reports process state and the provider-observed socket close; it does not treat process exit as proof of Go goroutine cleanup. The separate existing interruption E2E test below uses `AssertNoGoroutineLeak` for the handler lifecycle.

## Source and binary identity

| Item | Value |
| --- | --- |
| Source commit | `d5052d5612907f9fe073942ea7a7004b27c39958` |
| Isolated worktree | `/Users/pengjunchen/.codex/worktrees/r8-pty-acceptance/go-cli` |
| Go runtime | `go1.26.4 darwin/arm64` |
| CLI binary SHA-256 | `be9c088591dee4eae80ce832dec9516cdbdd14e7245d8f1c7b3f23862adf9b33` |
| PTY runner SHA-256 | `7f5715911dcfd567824bc60db25755123e90768e0fd26a515875fcd43f059612` |
| Full external run results SHA-256 | `4462e6c50fa694286f1126de0726802508e8f9edc2ef89650fbd45ea146e1cbe` |

Relevant source file hashes are in [`source-manifest.json`](source-manifest.json). The complete frames, sanitized PTY capture, session receipts, provider event trace, and result JSON are stored outside the repository at `/Users/pengjunchen/.codex/go-cli-validation/autonomous-20261001/r8/d5052-pty-acceptance/repro-run/`. No binary or raw run log is checked into this evidence candidate.

## Reproduce

Run from the repository root. Build the binary outside the checkout, install the PTY decoder outside the checkout, then pass both paths to the runner. The provider uses loopback HTTP and synthetic fixture values only.

```bash
rtk go build -trimpath -o /tmp/go-cli-r8-d5052 ./cmd/cli
rtk proxy python3 -m pip install --target /tmp/go-cli-r8-pty-deps pyte==0.8.2
rtk proxy env PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=/tmp/go-cli-r8-pty-deps python3 docs/iterations/2026-10-01-completion/evidence/r8-guarded-output-pty-d5052/r8_guarded_pty.py --binary /tmp/go-cli-r8-d5052 --source-commit d5052d5612907f9fe073942ea7a7004b27c39958 --evidence-dir /tmp/go-cli-r8-pty-evidence
```

The focused source tests also passed:

```bash
rtk go test ./internal/cli -run 'TestOutputGuardRunsBeforeVisibleDelta|TestDefaultAssemblyShowsGenerationStatusWithoutProviderContent' -count=1
rtk go test -tags=e2e ./tests/e2e_20260802 -run '^TestET_Phase20_Interrupt$' -count=1
```
