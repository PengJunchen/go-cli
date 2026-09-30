# R8 native PTY evidence

This native terminal journey used source commit `587be07e6d0961e72bf7261fb6148b0caae8f963`. The exact CLI binary was built from that commit with `go build -o /tmp/go-cli-r8-pty-587be07 ./cmd/cli`; its SHA-256 is `6f45cca23a012ec9243002a186627754be19d0213e97af683a6a53abd1411ce0`.

The fixture ran on Darwin through a native `openpty` with `TERM=xterm-256color`, `NO_COLOR=1`, and an 80×24 startup terminal. It used a local loopback streaming provider with a fixture-only canary key, 24 deterministic user turns including a real Read tool call, and a 25th assistant response held open at the provider. The driver sent PageUp while Bubble Tea was active and resized the PTY at 60, 80, 120, and 200 columns. The scrolled body anchor remained visible at each width; 120 and 200 columns activated the split tool pane. End was sent before the provider completed the turn. After stream completion, the final visible frame showed the new assistant answer and preserved the read-tool pane. `/exit` returned status 0 and restored ICANON, ECHO, and ISIG.

The pyte-decoded final viewport is [`final-visible-frame.txt`](final-visible-frame.txt). Its visible evidence is:

- Line 1 has the active REPL prompt; line 2 reports 52 loaded rows.
- Line 4 shows `R8-ANSWER-20` and `R8_TOOL_PANE_MARKER` in the split panes.
- Line 6 visibly contains `[receipt: success]`.
- Line 20 shows `R8-ACTIVE-PARTIAL` and `R8-ANSWER-APPEND`; lines 22–24 show the turn footer, token usage, and next prompt.
- There is no `[saved: success]` badge in this final viewport. The raw PTY stream contains earlier status frames, which are not evidence that the badge is visible in the final frame.

The test passed with 26 authenticated local streaming requests, one Read tool cycle, and an exact source SHA recorded in [`pty-summary.txt`](pty-summary.txt). The final visible frame is 4,627 bytes with SHA-256 `c73f82396b033d13156ce992ef9cf42b07a845e93b3ac56b1f514abcfa1abcf9`. The raw PTY transcript is 36,485 bytes with SHA-256 `67de7810589f1281385220421d500b4c0a9d04f2072678e9453a685165af05a7`. [`resize-frames.txt`](resize-frames.txt) preserves each width’s decoded screen, and [`r8-pty-driver.py`](r8-pty-driver.py) is the bounded local fixture driver.

This validates the captured active scroll, width changes, final follow-tail frame, read-pane visibility, and terminal restoration journey. It does not exercise R8-BLD-02 capability/help/doctor/restart behavior, so no claim is made for those items. The final saved-publication badge is also absent from the captured final viewport.
