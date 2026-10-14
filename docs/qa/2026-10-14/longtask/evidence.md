# Long-task verification evidence

An isolated Darwin run built `./cmd/cli` with `-race` and exercised the real
`interactive` command against a local OpenAI-compatible HTTP server. The
initial focused pass was followed by a full-tagged-suite failure that exposed
a follow-up synchronization race: PTY text was visible before the production
callback confirmed queue acceptance. The updated focused race run waits for
the production `core.turn.runner.followup.sent` event before releasing Bash
and passed. Its raw event stream is in
[`rawlogs/e2e-binary-longtask-darwin-queue-ack.jsonl`](rawlogs/e2e-binary-longtask-darwin-queue-ack.jsonl).
The full tagged suite still awaits the parent runner's rerun.

The latest focused run completed 120 streamed HTTP requests through the CLI.
Its final request contained 29 messages, retained turn 120, omitted turn 1,
and the CLI emitted `compaction.truncating.done`. The follow-up PTY run observed
queue acceptance before releasing Bash; the second HTTP request matched the
full `FOLLOW-UP-CANARY include the shell result in your completion` user
message and Bash result, and the expected side-effect file was present. The
Escape case returned `[interrupted]` in 16ms, left the delayed side effect
absent after five seconds, and sent only the original provider request.

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
immediately while the descendant inherits the pipes. The first red run did not
reach the PID marker before the short timeout. A later instrumented run proved
the marker was present but showed the 150ms pipe `WaitDelay` ended the test
before its 500ms deadline. With a 750ms `WaitDelay`, a focused race run passed
once and confirmed the marker; a repeated run overlapped the repository-wide
verification, reported both timeout and residual-descendant diagnostics, and
took 1.007s. The former one-second assertion was too tight for the documented
500ms command timeout, 750ms pipe wait bound, and up-to-500ms process-group
cleanup budget. The fixture now allows two seconds while retaining the 500ms
timeout, actual descendant marker, timeout result, and process-stopped
assertions. A focused race run passed in 1.18s, observed the descendant PID
marker, returned expected timeout 124, and confirmed the descendant stopped.
The residual-descendant diagnostic is expected because this synthetic parent
deliberately exits while its child holds the pipes; it shows the gate rejects
the timed-out command, not a failure in the repository's own verification run.
The runs are preserved in
[`rawlogs/test-gate-descendant-startup-red.jsonl`](rawlogs/test-gate-descendant-startup-red.jsonl),
[`rawlogs/test-gate-descendant-waitdelay-red.jsonl`](rawlogs/test-gate-descendant-waitdelay-red.jsonl),
[`rawlogs/test-gate-descendant-concurrent-red.jsonl`](rawlogs/test-gate-descendant-concurrent-red.jsonl),
and [`rawlogs/test-gate-descendant-green.jsonl`](rawlogs/test-gate-descendant-green.jsonl).

The SSH ambiguous-status fixture also received a focused race run. Its original
one-second tool deadline expired once for fake sshpass exit 5 during loaded
verification; the isolated retry finished in 161ms. The fixture's own deadline
is now five seconds to tolerate race-instrumented startup and package CPU
contention. All seven fake sshpass statuses passed as unknown outcomes after
the local process and readers stopped. Red, isolated, and green logs are in
[`rawlogs/sshpass-local-exit-codes-red.jsonl`](rawlogs/sshpass-local-exit-codes-red.jsonl),
[`rawlogs/sshpass-exit-5-isolation.jsonl`](rawlogs/sshpass-exit-5-isolation.jsonl),
and [`rawlogs/sshpass-local-exit-codes-green.jsonl`](rawlogs/sshpass-local-exit-codes-green.jsonl).

The Linux-tagged E2E test package cross-compiled successfully from Darwin. No
Linux runtime result is claimed. The Linux PTY helper attempts native PTY setup
and only skips on an actual allocation or ioctl failure; it does not skip
merely because `CI` is set.

The PTY child invocations include `--no-sandbox`. Their evidence covers TUI
approval, Bash side-effect files, cancellation, and the local HTTP requests;
it does not cover OS sandbox isolation or enforcement. All results here are
local fixture and source evidence. No test used a real model provider, and no
result is a production model acceptance claim.
