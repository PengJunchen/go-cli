# R8 terminal cell rendering review

Reviewed fixed commit `d03963b627951a807b556af55838beb3365a3b81` in a detached worktree. Scope is the approved Unicode/cell and terminal-sequence slice; this is source and Go-test evidence, not a PTY or performance claim.

## Decision

**Not accepted yet.** Two narrow contract gaps remain.

1. **Some valid SGR color sequences are returned without a reset.** `sgrStyleActive` sets the whole style state from each numeric field (`internal/tui/cells.go:190-211`). Thus the final palette/RGB color component `0` is mistaken for an SGR reset: review-only cases `\x1b[38;5;0mblack` and `\x1b[38;2;0;0;0mblack` pass through `safeTerminalText` without a trailing `\x1b[0m`. The current test expected a normal color then reset (`cells_test.go:48`) but does not cover zero-valued color components. This leaks terminal style state into subsequent content through actual consumers such as `BoxRenderer.Render` (`box.go:24-39`) and `AccordionModel.RenderView` → `truncateLine` (`accordion.go:284-316`). Track SGR reset semantics conservatively; any non-reset SGR that may leave a style active must ensure a final reset, including extended color parameters.

2. **The OSC-8 validator accepts a URL with no hostname.** `safeHyperlinkURL` checks only `u.Host != ""` (`cells.go:171-187`), so `http://:80/path` is accepted although `u.Hostname()` is empty. This contradicts the required nonempty-host rule. Require a nonempty hostname in addition to the existing scheme/userinfo/control checks.

## Verified behavior

- The requested render paths call the cell helpers: wrapping and `stripANSI` use `wrapCellText`/sanitization (`renderers.go:34-46`); code and diff wrap with `wrapWidth` (`renderers.go:145-148,197-212`); box measures and sanitizes terminal cells (`box.go:24-39`); accordion clipping uses `truncateLine` (`accordion.go:293-316`). The original no-ellipsis behavior remains.
- Follow-up multi-line OSC-8 fixture: `BoxRenderer` splits first and sanitizes each row, so an unclosed link is closed before that row's padding/right border, and the next row does not inherit it. `CodeRenderer` has no generated border, label, or line-number gutter; it wraps and theme-styles the provided content, and the input link closes after its last payload text. No current chrome-under-link defect was reproduced in either path. The generic `wrapCellText` does keep a supplied hyperlink open across explicit newlines until it returns; current consumers that add row chrome sanitize per row or add no per-row chrome.
- CJK, combining marks, variation selectors, skin-tone modifiers, ZWJ emoji, and flags are covered by cell/wrap tests. SGR/OSC-8 width, clipping closure, unsafe scheme/userinfo/raw-control rejection, incomplete OSC removal, and tab expansion at 8-column stops across CJK/ANSI are covered. Backslash authority input is rejected by `url.Parse` through the current validator.
- Review-only partial-UTF-8 and multi-line OSC-8/chrome tests passed: malformed/truncated UTF-8 and a partial escape yield valid UTF-8 output after sanitization and clipping; box chrome stays outside link spans; CodeRenderer adds no visible chrome inside the input link. Those temporary tests were removed.
- Independent focused race tests passed: `rtk go test -race ./internal/tui -run '^(TestWrapWidth.*|TestCellWidth.*|TestTabs.*|TestTerminalText.*|TestBoxRenderer.*|TestCodeAndDiff.*|TestTruncateLine.*|TestRenderView_.*)$' -count=1` (62 passed). Review-only adversarial checks reproduced the two findings above; those temporary tests were removed. No full package/PTY/performance claim is made here.

## Independent follow-up: fixed cells slice

Re-reviewed fixed commit `5f94e6788bde1cb84933709033f60ad9cd7dcc1a`, whose parent is the original reviewed snapshot `d03963b627951a807b556af55838beb3365a3b81`.

**The two findings above are closed in this fixed slice; the cell/OSC-8 change is accepted.** The SGR sanitizer now records that an SGR was seen and appends a terminal reset unless the output already ends in a recognized reset suffix (`internal/tui/cells.go:48-139`). It no longer interprets zero-valued indexed or RGB color parameters as a reset. Clipping returns the ANSI-aware cut directly, and the regression covers sanitized, clipped, and wrapped black-color sequences (`internal/tui/cells.go:20-33`, `internal/tui/cells_test.go:71-99`).

The OSC-8 URL validator now requires both a nonempty authority and hostname, rejects an empty trailing port and non-decimal/out-of-range ports, and continues to accept valid hostname and IPv6 links (`internal/tui/cells.go:176-198`, `internal/tui/cells_test.go:151-181`). The new regression confirms a no-host URL is emitted only as plain label text, without an OSC-8 link.

Independent fixed-snapshot evidence: exact target listing confirmed all six tests were selected. `rtk proxy env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -run '^(TestCellHelpersResetBlackSGRAfterClipAndWrap|TestTerminalTextDropsUnallowedControlsAndClosesLinks|TestTerminalTextRejectsOSC8WithoutHostname|TestCellWidthAndClipUnicodeAndANSI|TestWrapWidthPreservesAndClosesAllowedANSI|TestTruncateLineUsesTerminalCellAndGraphemeWidths)$' -count=10` passed. The original findings remain above as the record of the pre-fix baseline. This follow-up makes no full-package, PTY, or performance claim.
