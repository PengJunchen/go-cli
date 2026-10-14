# R1 process-owner review

Review date: 2026-09-25  
Review target: `7fb1f7d2fb3e51566fe56842370a4fc735e7b406` (`fix(tools): contain local process groups and bound remote output`) on `codex/runtime-r1-process`.  
Review type: independent source review and focused race run; no product implementation changes.

## Decision

**Accepted for the R1 process-owner slice with the documented containment limits.** I found no blocker in the frozen implementation for the reviewed scope. This accepts the Bash/SSH process and output-boundary slice only; it is not a full R1 or release acceptance.

## Findings

The implementation creates a fresh process group per local invocation on Darwin and Linux, reaps the command leader independently of pipe readers, and cleans remaining members when the leader exits or the context is canceled. It reports local quiescence only after the leader and both readers join and the owned process group is positively absent. If cleanup cannot be confirmed, the returned state stays pending; a reader error does not by itself keep a resource pending after the reader joins and the group is gone. Unsupported operating systems fail before starting the command. This matches the R1 contract's process-group boundary.

This is not a general process-tree sandbox. A descendant that deliberately calls `setsid`/`setpgid` and closes inherited pipes can escape the observed group and become unobservable; the implementation and metadata comments do not claim detection or containment of that case. Password mode adds a concrete instance: sshpass starts its `ssh` child in a new session. The escaped-child regression confirms that cancellation leaves the remote receipt unknown and pending, and that `ExecutionDone` does not close merely because the finite local fixture later exits. OpenSSH documents that exit 255 means an error while other statuses are the remote command status, so treating 255 as lacking a trusted remote receipt is conservative. The upstream sshpass 1.10 source archive is available [here](https://sourceforge.net/projects/sshpass/files/sshpass/1.10/sshpass-1.10.tar.gz/download); OpenSSH's [exit-status contract](https://man.openbsd.org/ssh#EXIT_STATUS) supports the 255 classification.

`RemoteBash` distinguishes pre-dispatch failures from dispatched uncertainty. Cancellation detected before invoking the client returns without dispatch; `DefaultSSHClient` pre-start cancellation and start failure carry an explicit `not_dispatched` marker. After dispatch, cancellation/deadline remains unknown and pending even when a client reports only an unknown outcome. OpenSSH 255 and transport/read failures remain pending. sshpass statuses 1–7 are treated as ambiguous between sshpass's own failure codes and forwarded remote statuses, but only unknown—not pending—once the local wrapper, readers, and child have joined. This preserves the distinction between an unresolved remote receipt and a still-observed local resource without retrying or claiming that remote work stopped.

The default SSH capture and Bash/Streaming Bash share one stdout-plus-stderr source-byte budget and continue draining after the cap. Streaming Bash preserves source LF/CRLF and unterminated EOF-tail bytes for accounting; framing and the fixed truncation marker are outside the source-byte budget. Tests cover cap exhaustion, concurrent streams, long-line scanner failure with continued drain, injected non-EOF read errors, CRLF accounting, and EOF-tail handling. The bounded buffers and metadata remain finite.

## Validation

I verified that all nine selected cancellation/remote-classification test names were listed, then independently ran them with `-race -count=1`; all passed in 2.693 seconds:

`rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tools -run '^(TestRemoteBashCancellationReportsUnknownPendingExecution|TestRemoteBashDeadlinePromotesUnknownOnlyClientErrorToPending|TestRemoteBashSSHStartFailureIsNotPendingExecution|TestDefaultSSHClientPreCanceledExecutionIsNotDispatched|TestRemoteBashSSHExit255IsUnknownPendingExecution|TestRemoteBashSSHPassLocalExitCodesAreUnknownButQuiescent|TestRemoteBashSSHPassEscapedChildRemainsPending|TestSSHPassEscapedChildHelper|TestDefaultSSHClientCancellationStopsLocalTransportDescendants)$' -count=1 -timeout 90s -v`

The frozen-snapshot evidence in [`r1-tools-process-green.log`](../r1-tools-process-green.log) records the exact 35-test focused run and full `internal/tools` race runs on Darwin/arm64 and Linux/arm64 using `golang:1.26 --init`. Linux evidence is limited to that container/init configuration; the earlier no-init attempt did not produce a test result. The Windows compile probe failed in unchanged `internal/approval` files using `syscall.Stat_t`; no Windows runtime support is claimed. Those limits do not invalidate the supported Darwin/Linux process-group slice.
