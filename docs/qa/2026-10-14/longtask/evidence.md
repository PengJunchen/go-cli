# Long-task verification evidence

> Retention note (2026-10-24): raw `.jsonl` runs and cross-compile output were removed after this report retained the red/green sequence, measured outcomes, and caveats. Paths to those files below identify historical evidence only.


An isolated Darwin run built `./cmd/cli` with `-race` and exercised the real
`interactive` command against a local OpenAI-compatible HTTP server. The
initial focused pass was followed by a full-tagged-suite failure that exposed
a follow-up synchronization race: PTY text was visible before the production
callback confirmed queue acceptance. The updated focused race run waits for
the production `core.turn.runner.followup.sent` event before releasing Bash
and passed. Its raw event stream is in
[`rawlogs/e2e-binary-longtask-darwin-queue-ack.jsonl`](rawlogs/e2e-binary-longtask-darwin-queue-ack.jsonl).
A later integrated full-tagged-suite run exposed a separate Escape cancel
lifecycle race, detailed below.

An earlier focused run completed 120 streamed HTTP requests through the CLI.
Its final request contained 29 messages, retained turn 120, omitted turn 1,
and the CLI emitted `compaction.truncating.done`. The follow-up PTY run observed
queue acceptance before releasing Bash; the second HTTP request matched the
full `FOLLOW-UP-CANARY include the shell result in your completion` user
message and Bash result, and the expected side-effect file was present. The
earlier focused Escape case returned `[interrupted]` in 16ms and left its
delayed side effect absent after five seconds. An earlier integrated tagged
run failed the Escape case: after the PTY showed `[quitting...]`, the app ended
before `[interrupted]` and reported `TUI exited before its event source and
durable refresh completed`. The full raw run and SHA-backed excerpt are
preserved in [`../gates/test-e2e.log`](../gates/test-e2e.log) and
[`../gates/test-e2e-224-cancel-red.json`](../gates/test-e2e-224-cancel-red.json).

The latest focused tagged run of `TestE2EBinaryLongTask` passed in 16.02s
(`go test` package elapsed 17.627s); its raw JSONL is
[`rawlogs/pty-childgroup-cleanup-green.jsonl`](rawlogs/pty-childgroup-cleanup-green.jsonl).
This run captured the source at commit 89 plus the final fixture patch; that
patch was subsequently committed as `6aa6f9318f3d3c40822c368e489c9284cd932072`.
It rebuilt the real race-enabled binary, completed all 120 streamed HTTP turns
with compaction, verified the follow-up user message and Bash result in the
second request after the queue-acceptance event, and passed Escape cancellation
in 18ms with no delayed side effect after five seconds or automatic continuation
request. This focused test is distinct from the repository-wide tagged suite;
the final repository-wide result is recorded separately in the gate report.

Final integrated validation on commit `6aa6f9318f3d3c40822c368e489c9284cd932072`
passed both gates. `make verify` exited 0 with 8,707 passes, 0 failures, and
6 skips across 28 packages; lint and scan reported no errors. The serial tagged
`test-e2e` run exited 0 with 4,781 passes, 0 failures, and 6 skips across 7
packages. The source started and ended at the same commit, with an empty
before/after source hash. Gate summaries are
[`../gates/verify.json`](../gates/verify.json) and
[`../gates/test-e2e.json`](../gates/test-e2e.json). The Linux test package
cross-compiled from Darwin for Linux/amd64 on this commit; no Linux runtime
result is claimed. The compile log is
[`rawlogs/linux-e2e-test-cross-compile-final.txt`](rawlogs/linux-e2e-test-cross-compile-final.txt).

The regression now exercises the same `waitForTUICompletion` decision called
by `REPLSession.waitForTurnCompletion`. Its red log forces app exit before the
turn, event bridge, and model event-source barriers; the previous behavior
reported a premature exit after Escape. The fix treats an already-cancelled
legacy turn as an expected Escape exit, while still joining the turn and event
bridge before returning control. Uncancelled early exits and durable apps that
exit before refresh acknowledgement remain errors. The red/green unit logs are
[`rawlogs/repl-cancel-lifecycle-red.jsonl`](rawlogs/repl-cancel-lifecycle-red.jsonl),
[`rawlogs/repl-cancel-lifecycle-green.jsonl`](rawlogs/repl-cancel-lifecycle-green.jsonl),
and the final no-timeout race rerun at
[`rawlogs/repl-cancel-no-timeout-green.jsonl`](rawlogs/repl-cancel-no-timeout-green.jsonl).

An experimental two-second timeout probe was rejected: `executeTurn` and
`stopTUI` still join the turn and event bridge after a helper timeout, so that
probe did not establish a bounded caller and could not safely promise one. Its
raw logs remain for history at
[`rawlogs/repl-cancel-lifecycle-bounded-green.jsonl`](rawlogs/repl-cancel-lifecycle-bounded-green.jsonl)
and [`rawlogs/repl-cancel-owner-boundary-green.jsonl`](rawlogs/repl-cancel-owner-boundary-green.jsonl),
but they do not describe final behavior. The final change keeps the existing
owner joins and only fixes the expected legacy Escape classification; the
separate durable refresh wait retains its existing bound.

An earlier race-enabled PTY Escape check returned `[interrupted]` in 21ms,
left the delayed side effect absent after five seconds, issued no automatic
continuation request, and then completed a later explicit prompt as a fresh
turn. Its raw event stream is
[`rawlogs/pty-cancel-continue-green.jsonl`](rawlogs/pty-cancel-continue-green.jsonl).
The final focused tagged run also repeated the complete cancellation scenario;
the repository-wide tagged suite result remains separate.

The follow-up and cancellation PTY tests wait for shell-written PID and start
files after approval. The previous wait could match command text in the
approval preview, which is not evidence that Bash had started. Adding a unique
cleanup token made the preview no longer contain the full substring and
exposed that false positive; the failed attempt is preserved at
[`rawlogs/pty-preview-marker-false-positive-red.jsonl`](rawlogs/pty-preview-marker-false-positive-red.jsonl).
The final run proves execution through those files, then checks the production
queue acknowledgement, actual HTTP request contents, and side-effect file. A
tagged race test confirms cleanup kills a live Bash group only for a matching
fixture token and leaves it running after a mismatched token; see
[`rawlogs/pty-bash-group-cleanup-final-green.jsonl`](rawlogs/pty-bash-group-cleanup-final-green.jsonl).
If Escape has ended the Bash leader while its foreground five-second sleep is
still running, that child is bounded by the fixture delay. It cannot write the
side-effect file without its Bash parent, and the test waits through that delay
before checking the file.

The follow-up test holds Bash on the release file until the isolated CLI log
contains `core.turn.runner.followup.sent`. That production event is emitted
after `TurnRunner.FollowUp` has accepted the message into its channel and turn
record. The test then releases the shell and asserts the actual second HTTP
request.

The initial failing TUI regression log records `helloworld` after a
`tea.KeySpace` message in both follow-up and steering input. The green test
drives `teaModel.Update` and `BubbleteaApp.View`, then checks the exact callback
submission. The test fixture now inserts the space at the active cursor in both
input modes; see [`rawlogs/tui-space-red.jsonl`](rawlogs/tui-space-red.jsonl)
and [`rawlogs/tui-space-green.jsonl`](rawlogs/tui-space-green.jsonl).

The Grep fixture's initial red run timed out before its helper could write the
PID marker. The revised test uses a controlled deadline and does not expire it
until the helper marker proves the escaped writer is running. Its race-enabled
focused run passed; see
[`rawlogs/grep-escaped-pipe-startup-red.jsonl`](rawlogs/grep-escaped-pipe-startup-red.jsonl)
and [`rawlogs/grep-escaped-pipe-green.jsonl`](rawlogs/grep-escaped-pipe-green.jsonl).

The test-gate fixture must preserve the original process relationship: its
shell parent starts a descendant, writes that descendant's PID, and exits
immediately while the descendant inherits the pipes. Earlier 500ms attempts
sometimes timed out before the shell had been scheduled to write the marker; a
150ms `WaitDelay` also ended one run before its 500ms command deadline. A
repeated 500ms/750ms attempt under repository-wide load took 1.007s, exceeding
the old one-second assertion. The final fixture uses a 2s command timeout, 3s
`WaitDelay`, and an 8s outer bound: 2s timeout + 3s pipe wait + up to 500ms
process-group cleanup + 2.5s scheduling margin. The fake parent still exits
immediately after launching the pipe-holding descendant. Its focused race run
passed in 3.43s with marker present, expected timeout 124 after 2s, and the
descendant stopped. The expected residual-descendant diagnostic reflects the
synthetic parent exiting while its child holds the pipes; the test verifies the
gate rejects the timed-out command and stops the child. Evidence is at
[`rawlogs/test-gate-descendant-seconds-green.jsonl`](rawlogs/test-gate-descendant-seconds-green.jsonl).
Earlier 500ms attempts remain available for history at
[`rawlogs/test-gate-descendant-startup-red.jsonl`](rawlogs/test-gate-descendant-startup-red.jsonl),
[`rawlogs/test-gate-descendant-waitdelay-red.jsonl`](rawlogs/test-gate-descendant-waitdelay-red.jsonl),
[`rawlogs/test-gate-descendant-concurrent-red.jsonl`](rawlogs/test-gate-descendant-concurrent-red.jsonl),
and [`rawlogs/test-gate-descendant-green.jsonl`](rawlogs/test-gate-descendant-green.jsonl), the earlier one-off pass.

The SSH ambiguous-status fixture also received a focused race run. Its original
one-second tool deadline expired once for fake sshpass exit 5 during loaded
verification; the isolated retry finished in 161ms. The fixture's own deadline
is now five seconds to tolerate race-instrumented startup and package CPU
contention. All seven fake sshpass statuses passed as unknown outcomes after
the local process and readers stopped. Red, isolated, and green logs are in
[`rawlogs/sshpass-local-exit-codes-red.jsonl`](rawlogs/sshpass-local-exit-codes-red.jsonl),
[`rawlogs/sshpass-exit-5-isolation.jsonl`](rawlogs/sshpass-exit-5-isolation.jsonl),
and [`rawlogs/sshpass-local-exit-codes-green.jsonl`](rawlogs/sshpass-local-exit-codes-green.jsonl).

The Linux-tagged E2E test package cross-compiled successfully from Darwin for
Linux/amd64 on commit 6aa. No Linux runtime result is claimed. The cross-compile log is
[`rawlogs/linux-e2e-test-cross-compile-final.txt`](rawlogs/linux-e2e-test-cross-compile-final.txt).
The Linux PTY helper attempts native PTY setup
and only skips on an actual allocation or ioctl failure; it does not skip
merely because `CI` is set.

The PTY child invocations include `--no-sandbox`. Their evidence covers TUI
approval, Bash side-effect files, cancellation, and the local HTTP requests;
it does not cover OS sandbox isolation or enforcement. All results here are
local fixture and source evidence. No test used a real model provider, and no
result is a production model acceptance claim.
