# R5 TUI interaction review

## Verdict

**Not approved at `6921b4bb1c0de9896590317527eef8d68df74224`.** The bounded approval modal, request identity, terminal-safe rendering, and editor caret still have visible correctness gaps. I reviewed only the R5 interaction slice; this is not a judgment on R5 CLI wiring, PTY behavior, durable transcript projection, or other iteration contracts.

## Fixed snapshot and scope

- Reviewed commit: `6921b4bb1c0de9896590317527eef8d68df74224` (`codex/runtime-r5-ui`). The four changed files are `internal/tui/app.go`, `internal/tui/approval_entry.go`, `internal/tui/approval_entry_test.go`, and `internal/tui/steer_statusbar_test.go`.
- Design authority: `docs/iterations/2026-09-25-runtime/r5-ui-design.md`, especially the approval display contract (line 57), truncation rule (line 71), modal bindings (line 84), resize requirements (line 94), and caret requirement (line 119).
- Product source was not changed. A temporary probe test was removed after use; the R5 worktree is clean.

## Findings

### R5-1 — A clipped approval can be granted without reviewing its arguments

`renderApprovalRequestPage` marks details as truncated and renders only the current page, but `handleApprovalKeyLocked` accepts `y` regardless of the current detail offset or whether the user has reached the end. In an actual `teaModel.Update`/`View` probe at 20×4, the modal showed `view truncated · details 1-1/12`; pressing `y` immediately returned `ApprovalAllow`. This contradicts the design rule that Allow must not be visible or valid while the complete prepared arguments cannot be inspected. Track review of all pages before enabling Allow, or make the request non-approvable until the user can inspect the complete bounded details.

### R5-2 — Untrusted argument controls pass through the terminal view

`walkApprovalTextLines` splits only on LF and `wrapApprovalLine` sends the remaining text to Lipgloss. An 80×30 actual view probe confirmed that argument bytes `ESC[2J`, `ESC]52;c;Zm9v`, BEL, and CR are present unchanged in the returned view. This establishes propagation into the terminal output path; I did not run a PTY or assert a particular terminal emulator's clipboard behavior. Sanitize or visibly escape C0/ESC/OSC controls from untrusted argument and diff text so they cannot issue terminal commands or alter neighboring output.

### R5-3 — Small windows overflow the modal in both dimensions

At 20×4, the actual view contained six logical rows, with measured widths `[16, 26, 51, 49, 7, 45]`. The fixed header, scope, truncation status, and decision footer are not constrained to the window width, and the minimum one-row details viewport means the modal can exceed a very short terminal height. Keep the decision row reachable within the available dimensions, wrapping or compacting fixed text and defining a safe minimum-height layout; when dimensions cannot support review, disable approval until resize makes it possible.

### R5-4 — A supplied source `RequestID` is dropped from the approval UI

`ApprovalRequest` carries `RequestID`, and the probe supplied `source-request-42`, but the modal displayed only `TUI local interaction #1`. The design requires the approval's request identity to be visible. Render the supplied source ID when present; use a clearly labeled local token only when no source ID exists, and keep that token as local queue-cancellation identity rather than implying it is the request's identity.

### R5-5 — The rendered caret does not follow the insertion position

On the actual model path, `Tab` → type `你好` → `Left` leaves the byte cursor at 3 (between the two runes), but `View()` still renders `> steer> 你好█`, with the block at the end. `renderViewLocked` appends the block after the complete input for both steer and follow-up modes. Render the caret at the actual UTF-8 insertion boundary for both editors; retain the existing insertion/backspace semantics.

## Validation evidence

The focused race command was:

```text
rtk go test -race ./internal/tui -run '^(TestRenderApprovalRequest|TestTeaModel_Approval|TestApprovalRequestMsg_UpdateFlow|TestTUI.*(Approval|Interaction)|TestCanceledApprovalCannotBeApprovedBeforeCancelMessage|TestAnsweredInteractionsStopContextWatchers|TestSteerEditorCaretTracksInsertion|TestSteerInputChinese|TestFollowUpInputChinese)' -count=1
```

It passed 30 matching tests in `internal/tui`. The exercised set includes `TestTUIConcurrentApprovalRequestsRemainResolvable`, `TestTUIApprovalAndHITLModalsFollowArrivalOrder`, `TestTUICancelResolvesEveryQueuedInteraction`, `TestTUIClosedApprovalChannelResolvesQueuedRequests`, `TestTUIQueuedApprovalCancellationKeepsOtherRequestResolvable`, `TestTUIApprovalDetailsScrollKeepsDecisionRowVisible`, `TestTUIApprovalQueueCancellationKeepsRemainingFIFOOrder`, `TestCanceledApprovalCannotBeApprovedBeforeCancelMessage`, and `TestSteerEditorCaretTracksInsertion`. These green tests do not close the findings above; the temporary actual Update/View probe supplied the counterexamples.

The source-level review also confirmed that argument keys are sorted for display without mutating the caller's `Args` map, and that `a` remains inert when no trusted reusable scope is supplied. No new issue was found in the covered FIFO, queued-cancel, closed-channel, or duplicate-resolution paths.

## Limits

This review does not claim R5 CLI default-path integration, PTY terminal restoration, real emulator clipboard effects, durable transcript identity, canonical branch projection, or R8's full Unicode/cell-width/platform matrix. Those were outside this fixed slice review. The findings above are sufficient to keep this snapshot unapproved pending a targeted fix and delta review.

## Fixed-snapshot delta review — `9342cdef77bf60b694cfc89ead71ed32ce19c5fb`

**Verdict: the five original findings are closed by this delta, but the updated slice is still not approved because of R5-6 below.** This review used a clean detached worktree at the exact commit above and exercised the real `teaModel.Update` and `View` path, not only the page-rendering helper. The original rejection and red evidence above remain intact.

| Original finding | Delta result | Evidence |
| --- | --- | --- |
| R5-1 approval before all details are reviewed | Closed | `y` stays locked until every rendered detail row has been visited. A page-navigation key alone does not count until `View()` renders it. Reviewing all pages enables `y`; resize and `d` diff-visibility changes reset review and lock it again. |
| R5-2 terminal controls reach returned view | Terminal-execution part closed; see R5-6 | CSI clear-screen, OSC 52, BEL, and CR are absent from the view, but are silently deleted from approval text. |
| R5-3 modal overflows small windows | Closed for tested cell/row bounds | Actual modal probes at 20×4 and at widths 1, 2, 3, 10, and 20 with heights 1–4 stayed within supplied cell and row limits; `y` stayed locked whenever details could not be shown. |
| R5-4 source RequestID is lost | Closed | A supplied `RequestID` is displayed, while existing no-ID queue tests still use the distinct local interaction token. |
| R5-5 UTF-8 caret is detached from insertion point | Closed | Steer and follow-up input put the caret between `你` and `好` after Left, insert `X` at that position (`你X好`), then move Right by a complete UTF-8 rune. |

### R5-6 — Silent control removal makes approval text differ from the payload

The new sanitizer prevents untrusted terminal commands, but drops those bytes instead of displaying a faithful escaped representation. A real `Update(approvalRequestMsg{...})` followed by `View()` with this request:

```go
tui.ApprovalRequest{
    ToolName:    "tool\x1b[2J",
    RequestID:   "id\x1b[2J",
    Args:        map[string]any{"command": "cmd:before\x1b[2Jafter\r\x1b]52;c;Zm9v\a"},
    DiffPreview: "+safe\x1b[2Jdanger",
}
```

rendered the relevant text as `tool`, `id`, `cmd:beforeafter`, and `+safedanger`; the source `Args` and `DiffPreview` remained unchanged, and the request could still be approved with `y`. The distinct payload `cmd:beforeafter` and the payload containing removed control sequences therefore become indistinguishable in the approval UI. This is an authorization-integrity issue even though terminal injection is blocked.

Keep argument and diff newlines that carry content as layout, but render other untrusted control bytes as visible literal escapes (for example `\x1b[2J`, `\r`, and `\a`). Apply that representation consistently to tool name, source request ID, argument keys and values, and diff text; do not retain untrusted SGR or OSC 8 formatting. Keep raw tool arguments and diff intact. Preserve invalid UTF-8 bytes visibly unless the input contract guarantees valid UTF-8 and states that boundary. Recompute wrapped detail rows and the review gate from the final escaped display text so expansion cannot leave an old row count authorizing unseen content.

The reproducible temporary probes were named `TestReviewProbeR5UpdateView`, `TestR5ControlTextCanStillBeApprovedProbe`, and `TestReviewProbeR5FollowUpUTF8CaretInsertion` in package `tui`. The approval probes sent a `WindowSizeMsg`, an `approvalRequestMsg` with the values above, then called `View()`; the second also sent `y` and confirmed `ApprovalAllow`. It compared that modal byte-for-byte with a second request whose raw text was already `tool`, `id`, `cmd:beforeafter`, and `+safedanger`: the distinct source payloads produced identical rendered modals. The follow-up probe drove `f`, typed `你好`, sent Left, checked `View()`, inserted `X`, and sent Right. All temporary tests were removed; the fixed detached source worktree is clean.

### Delta validation

The exact relevant test names were confirmed with `go test -list` before execution. The focused race set passed five repetitions:

```text
rtk proxy go test -race -p 2 -count=5 ./internal/tui -run '^(TestTUIApprovalDisplaysAuthoritativeRequestID|TestTUIApprovalModalFitsSmallTerminalAndSanitizesUntrustedDetails|TestTUIApprovalVisibleDetailsSanitizeTerminalControls|TestTUIConcurrentApprovalRequestsRemainResolvable|TestTUIApprovalAndHITLModalsFollowArrivalOrder|TestTUICancelResolvesEveryQueuedInteraction|TestTUIClosedApprovalChannelResolvesQueuedRequests|TestTUIQueuedApprovalCancellationKeepsOtherRequestResolvable|TestTUIApprovalDetailsScrollKeepsDecisionRowVisible|TestTUIApprovalQueueCancellationKeepsRemainingFIFOOrder|TestCanceledApprovalCannotBeApprovedBeforeCancelMessage|TestAnsweredInteractionsStopContextWatchers|TestSteerEditorCaretTracksInsertion|TestFollowUpInputChineseBackspace|TestFollowUpInputChineseCtrlW|TestReviewProbeR5UpdateView|TestReviewProbeR5FollowUpUTF8CaretInsertion)$'
```

The full package race gate also passed on the fixed snapshot:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -count=1
ok   github.com/pengjunchen/go-cli/internal/tui  2.139s
```

The green package gate does not close R5-6; current tests assert that controls are absent, but do not assert that the removed bytes remain legible as part of the approval decision.


## Fixed-snapshot delta review — `01404e636938eea0beaca92f61cb663b407b1bc8`

**Verdict: still not approved.** The original R5-1 through R5-5 findings remain closed by the `9342cde` changes and are confirmed on this fixed head. `01404e6` closes terminal-command execution and silent control-byte deletion, but the approval display encoding is not injective; the control-removal integrity finding therefore remains open. A second approval-integrity finding remains for invisible/bidirectional Unicode format characters.

| Prior finding | Result at `01404e6` | Evidence |
| --- | --- | --- |
| R5-1 Allow before every detail is reviewed | Closed | `renderApprovalInteractionLocked` computes review rows through the same detail walker used for display; a keypress or scroll alone does not mark rows reviewed. |
| R5-2 untrusted terminal controls reach the terminal | Terminal-execution part closed | C0/C1 and invalid UTF-8 bytes are escaped before wrapping/rendering. The ambiguity documented as R5-6 remains. |
| R5-3 modal overflows small windows | Closed for tested bounds | Actual 20×4 compact view is within 20 cells and four rows, visibly locks `y`, and does not return a decision. At 20×10, full long tool name and RequestID are available in wrapped scrollable details before approval unlocks. |
| R5-4 source RequestID is absent | Closed | In normal layouts the source ID is displayed; the local interaction sequence remains the fallback when no source ID exists. In 20×4 compact mode the ID is clipped, but the layout is locked against approval until the user resizes. |
| R5-5 UTF-8 caret does not follow insertion | Closed | Existing steer/follow-up caret regressions remain in the focused race selection and full package race gate. |
| R5-6 distinct control-bearing payloads collapse after silent deletion | Partially closed; still blocking | Controls are now rendered as escapes, but an original backslash is not escaped, so a raw control and text that resembles its escape still render identically and both can be approved. |

### R5-6 — Escaped preview is ambiguous with literal source text

`sanitizeApprovalText` emits printable escape notation for controls and invalid UTF-8 at `internal/tui/approval_entry.go:108-149`, but its ordinary-rune path copies backslashes unchanged (`:143-145`). `approvalSingleLine` also maps a real newline to printable `\n` while leaving a source backslash untouched (`:151-152`). Consequently the renderer is not a reversible display encoding.

I exercised `teaModel.Update` with an `approvalRequestMsg`, then called the real `View()` and compared complete modal strings. These two distinct command arguments produced byte-identical views and both returned `ApprovalAllow` after `y`:

```go
// Raw ESC byte followed by CSI text:
"cmd\x1b[2J"
// Six literal source characters after cmd: backslash, x, 1, b, [, 2, J:
`cmd\x1b[2J`
```

The same collision was reproduced independently in `ToolName`, `RequestID`, `Args["command"]`, and `DiffPreview`. Actual tab and invalid byte `0xff` also collided with the corresponding literal `\t` and `\xff` source text. This leaves a user able to approve a payload whose visible representation does not tell them whether the escape was present in the original command or was introduced by the renderer.

The smallest safe change is to escape an original backslash as `\\` in the display encoding, in addition to rendering control bytes as their `\xNN`/named escapes. Keep the raw argument and diff unchanged. Add red/green actual `Update`/`View` regressions that compare raw ESC vs literal `\x1b`, tab vs literal `\t`, invalid byte vs literal `\xff`, and actual newline vs literal `\n`; assert distinct final views and preserve the `y` gate until every final wrapped row is reviewed.

### R5-7 — Invisible Unicode format characters remain in approval text

The sanitizer handles `unicode.IsControl` only. Unicode format characters such as U+200B ZERO WIDTH SPACE and U+202E RIGHT-TO-LEFT OVERRIDE are not in that category and pass through unchanged at `approval_entry.go:143-145`. The actual `View()` contains those raw runes, not a visible escape. The probe confirmed `cellWidth("leftRightXYZ") == cellWidth("left\u200bRight\u202eXYZ")`, so U+200B can be inserted into an action value without changing its terminal-cell footprint or appearing as a visible marker; U+202E can reorder text on bidi-aware renderers. This can hide or rearrange parts of the exact command/diff the user is approving.

Escape bidi-control and zero-width/invisible format characters (conservatively, the relevant Unicode `Cf` characters) as visible code-point notation in untrusted tool name, RequestID, argument keys/values, and diff text. Keep ordinary Unicode text and do not broaden the fix into general homoglyph normalization. Compute wrapping and review coverage only after this encoding, so escaped code points add reviewable rows just like escaped C0 controls.

### Pagination and narrow-layout recheck

The pagination gate is correctly based on the final wrapped representation: `walkApprovalDetailLines` sanitizes argument and diff text before `wrapApprovalLine` (`approval_entry.go:155-224`), and `renderApprovalInteractionLocked` uses that walker both for `total` and the rows credited by `View()` (`app.go:847-880`). Window resize clears coverage (`app.go:652-660`); toggling diff visibility resets it (`app.go:1889-1895`). The permanent long-escaped-detail regression verifies that scrolling without rendering does not unlock approval, and that resize/diff changes require re-review (`approval_entry_test.go:310-363`).

At 20×4, the actual compact view was: header prefix `[approval] dangerous`, RequestID prefix `request: request-r`, clipped scope instruction, and `[y] locked [n] no`. The narrow layout stays within supplied dimensions and rejects `y`; it does not authorize hidden details. At 20×10 with long values, the clipped header is supplemented by full scrollable tool-name and RequestID rows; actual scrolling/rendering of all rows is required before `[y] once` appears. I found no new cancellation/FIFO or caret regression in the fixed snapshot.

The temporary probes were `TestReviewProbeR5EscapingCollisionsAndUnicodeFormat`, `TestReviewProbeR5AllApprovalFieldsHaveLiteralEscapeCollisions`, and `TestReviewProbeR5NarrowHeaderDetailsRemainReviewable`. They passed while asserting the counterexamples above, then were removed. The independent source worktree is clean and still fixed at `01404e6`.

### `01404e6` verification

- The exact permanent target names were listed before execution. The approval identity, short-screen bounds, control escape, final-row gating, resize/diff reset, scroll/FIFO/cancel, and UTF-8 caret selection passed under race for three repetitions: `rtk env GOMAXPROCS=2 go test -race -p 2 -count=3 ./internal/tui -run '^(TestTUIApprovalDisplaysAuthoritativeRequestID|TestTUIApprovalModalFitsSmallTerminalAndSanitizesUntrustedDetails|TestTUIApprovalVisibleDetailsSanitizeTerminalControls|TestSanitizeApprovalTextEscapesControlsAndInvalidUTF8WithoutLoss|TestTUIApprovalEscapedLongDetailsMustBeReviewedAfterResizeAndDiffChange|TestTUIConcurrentApprovalRequestsRemainResolvable|TestTUIApprovalAndHITLModalsFollowArrivalOrder|TestTUIQueuedApprovalCancellationKeepsOtherRequestResolvable|TestTUIClosedApprovalChannelResolvesQueuedRequests|TestTUIApprovalQueueCancellationKeepsRemainingFIFOOrder|TestCanceledApprovalCannotBeApprovedBeforeCancelMessage|TestTUIApprovalDetailsScrollKeepsDecisionRowVisible|TestAnsweredInteractionsStopContextWatchers|TestSteerEditorCaretTracksInsertion|TestFollowUpInputChineseBackspace|TestFollowUpInputChineseCtrlW)$'` — passed.
- Actual Update/View counterexample probes for the argument collision, all four approval fields, and narrow 20×10 layout passed under race; these probe assertions intentionally confirm the collision and confirm narrow layout remains locked until all wrapped rows are displayed.
- Full fixed-source TUI race gate passed: `rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -count=1` — `internal/tui` passed. This was not a repository-wide test run.


## Fixed-snapshot follow-up — `d3ac6b292da1b87392a5adb3a431c2d53b1f948e`

**Verdict: approve the bounded R5 interaction slice at `d3ac6b2`.** The fix makes the display encoding distinguish literal backslashes from renderer-introduced escapes and visibly escapes Unicode format characters. The red `01404e6` counterexamples remain recorded above as historical evidence; neither R5-6 nor R5-7 reproduces on this fixed head. This approval does not close R5 CLI default-path, PTY behavior, or broader runtime/UI work outside the reviewed interaction slice.

At `internal/tui/approval_entry.go:108-152`, the sanitizer now doubles every input backslash before emitting control escapes and escapes Unicode `Cf` along with control runes. Therefore a source ESC becomes `\x1b`, while source text containing the six characters `\x1b` becomes `\\x1b`; zero-width/bidi format runes such as U+200B and U+202E are rendered as `\u200b` and `\u202e`. The original payload remains untouched; the escaping is a display projection.

The new actual `teaModel.Update`/`View` regression, `TestTUIApprovalUntrustedTextEscapingIsInjectiveAndVisible` (`approval_entry_test.go:310-413`), checks ESC vs literal escape spelling, tab vs literal `\t`, invalid byte vs literal `\xff`, tool name, source request ID, argument value, diff, U+200B, and U+202E. Each case asserts a visible representation and distinct full modal output before confirming the normal once-approval response. This closes both fixed-014 approval-integrity findings without introducing general homoglyph normalization.

The prior line-coverage finding is closed on this head. `walkApprovalDetailLines` sanitizes before wrapping, and the same walk produces the final row count and the rows credited only when rendered (`approval_entry.go:159-224`; `app.go:847-880`). Resize and diff toggles continue to clear review state (`app.go:652-660, 1889-1895`). The long escaped-content regression verifies that page movement without `View()` does not unlock approval and that changing viewport or diff resets coverage. The additional actual narrow-layout probe at 20×10 confirmed that long tool name and request ID are available as full scrollable rows and `y` only unlocks after every final wrapped row has been displayed. At 20×4 the compact display remains bounded and locked, so clipped details cannot be approved.

The 014 collision/format probes were re-run against `d3ac6b2` through the committed field-by-field regression. The exact target names were listed before execution. The approval identity, terminal sanitization, injective display, escaped final-row gate, resize/diff, scroll/FIFO/cancellation, and UTF-8 caret set passed under race for three repetitions:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=3 ./internal/tui -run '^(TestTUIApprovalDisplaysAuthoritativeRequestID|TestTUIApprovalModalFitsSmallTerminalAndSanitizesUntrustedDetails|TestTUIApprovalVisibleDetailsSanitizeTerminalControls|TestSanitizeApprovalTextEscapesControlsAndInvalidUTF8WithoutLoss|TestTUIApprovalUntrustedTextEscapingIsInjectiveAndVisible|TestTUIApprovalEscapedLongDetailsMustBeReviewedAfterResizeAndDiffChange|TestTUIConcurrentApprovalRequestsRemainResolvable|TestTUIApprovalAndHITLModalsFollowArrivalOrder|TestTUIClosedApprovalChannelResolvesQueuedRequests|TestTUIQueuedApprovalCancellationKeepsOtherRequestResolvable|TestTUIApprovalDetailsScrollKeepsDecisionRowVisible|TestTUIApprovalQueueCancellationKeepsRemainingFIFOOrder|TestCanceledApprovalCannotBeApprovedBeforeCancelMessage|TestAnsweredInteractionsStopContextWatchers|TestSteerEditorCaretTracksInsertion|TestFollowUpInputChineseBackspace|TestFollowUpInputChineseCtrlW)$'
```

Full fixed-snapshot TUI race gate passed independently: `rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -count=1` — `internal/tui` passed. The detached review worktree stayed clean at `d3ac6b2`; no product source was changed during review.
