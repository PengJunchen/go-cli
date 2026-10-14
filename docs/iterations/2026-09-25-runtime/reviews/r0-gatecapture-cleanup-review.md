# R0 gatecapture cleanup review

Status: **rejected at `d5e27d3f17934ebbee23fc7a4a6b3eccaa621107`**. Root independently reviewed the process-group implementation and gate callers. The supported-platform ownership checks and post-exit cleanup improve the previous descendant leak, but the following two regressions require a follow-up commit.

## Findings

1. **P1: output overflow does not start bounded escalation while the leader remains alive.** `Run` replaces the overflow callback with group TERM, then waits in `cmd.Run()` before calling `group.cleanup()`. `exec.Cmd.WaitDelay` starts on context cancellation or process exit; output overflow alone triggers neither. A command ignoring TERM can therefore continue until the unrelated command deadline (15 minutes by default for test-gate). An overlay regression using a 2-second command context, `sh -c "trap '' TERM; printf 'abcdefghijklmnopqrstuvwxyz'; sleep 30"`, an 8-byte stdout cap, and a 100 ms WaitDelay returned after 2.219 seconds with the context deadline exceeded. Overflow must start one owned, bounded TERM-to-KILL cleanup sequence immediately, and that sequence must be joined rather than detached.

2. **P2: a plain `exec.Command` is now rejected by Go before start.** `Run` assigns non-nil `cmd.Cancel` unconditionally. `exec.Cmd.Start` rejects this when the command was not constructed with `CommandContext`. An overlay regression calling `Run(exec.Command("true"), ...)` fails with `exec: command with a non-nil Cancel was not created with CommandContext`. Retain support for plain commands while providing bounded overflow cleanup; the current production gate callers happen to use CommandContext.

## Evidence and boundary

The two temporary package-internal overlay tests asserted the desired behavior and both **failed** at the reviewed hash under `go test -race -p 2 -count=1`; this is defect reproduction, not passing implementation evidence. The output is in [r0-gatecapture-d5-review-probes.log](../evidence/r0-gatecapture-d5-review-probes.log). Temporary overlay source was removed; no product source was edited. The command's owned descendants were terminated by the reviewed cleanup path after its context expired.

The builder's three-package race, vet, lint and cross-compilation results remain valid for their selected tests. They do not cover these two cases. Linux runtime behavior has not been executed on this Darwin host. The supplemental protected test-gate patch remains unapplied and outside the prior authorization.

## Root delta review at `4633544`

Root independently reviewed the follow-up at `6335ebc` line by line and found no additional blockers. The two findings above were addressed: output overflow now starts a single bounded TERM-to-KILL cleanup owner that is joined before return, and plain `exec.Command` retains a nil `Cancel` callback while context commands share the bounded cleanup owner. Root then ran the `gatecapture`, `test-gate`, and `scanner-gate` race selections with `-count=3`; all passed, taking 3.947s, 7.442s, and 5.083s respectively. The implementation chain was merged as `d5→e70118d`, then `6335→4633544`.

This delta does not change the historical rejection of `d5`, nor does it establish Linux runtime behavior; runtime execution remains unverified on Linux. The supplemental protected test-gate patch remains unapplied and outside the prior authorization.
