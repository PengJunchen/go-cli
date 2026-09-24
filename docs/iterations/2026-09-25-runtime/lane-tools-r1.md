# R1 tools process ownership and remote output lane

Branch: `codex/runtime-r1-process`
Worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r1-process`
Base: `764ca0c`
Status: implementation, focused regressions, and Darwin/Linux package race runs passed; awaiting final independent review and commit.

This lane implements the tools-owned part of R1-BLD-03: local Bash and
StreamingBash process-group cleanup, plus RemoteBash output bounds and remote
completion classification. It does not change `internal/core` lifecycle
ownership. See [r1-contract.md](r1-contract.md) for the iteration contract.

## Implementation

Bash, StreamingBash, and the local command behind DefaultSSHClient use one
process-group supervisor. It starts each command in a fresh group, owns both
pipe ends, observes `cmd.Wait` independently of reader EOF, and joins the
readers after bounded group cleanup. A nonzero `Pgid` supplied by a caller is
rejected so this helper cannot signal a process group it did not create. It
closes both read descriptors after normal EOF and forced cleanup. Pending local
execution closes its `ExecutionDone` channel only after the leader and readers
join and the owned process group is observed absent. A read error affects the
reported result but does not keep the resource pending after those resources
stop.

`containment_scope=process_group` is attached to local Bash and StreamingBash
results. This is an explicit scope label, not proof of a general process tree
or sandbox. Darwin and Linux process groups cannot detect an arbitrary child
that calls `setsid`/`setpgid` and closes inherited pipes; such deliberately
escaped work is outside the guarantee and cannot be observed by this helper.
The implementation does not claim that every escape is detected. If an owned
group or reader is observed to remain live, the tool reports unknown execution
with pending resources; if the platform cannot configure the group, the tool
fails before start.

DefaultSSHClient bounds local stdout and stderr capture with a shared source
byte budget. OpenSSH exit 255 has no trusted remote completion receipt, so it
remains unknown and pending. Password mode uses `sshpass`; its status values
1–7 overlap its own handshake/authentication errors and valid remote command
statuses. Those statuses are unknown outcomes after the local `sshpass`
process/readers have joined, without a pending-resource marker. Cancellation,
transport errors, or any missing remote terminal receipt remain pending:
stopping the local ssh/sshpass process group cannot prove that remote work
stopped. `local_ssh_process_group_scope=process_group` describes only the local
group. `sshpass` starts its `ssh` child in a separate session, so this field does
not claim containment of the complete local SSH process tree.

RemoteBash passes its configured output budget to DefaultSSHClient through the
optional `BoundedSSHClient` extension. Legacy SSHClient results are clipped
before the returned metadata is materialized and cloned to avoid retaining a
large backing string. stdout and stderr share the configured combined
source-byte budget. Metadata reports `truncated`; the truncation marker is
outside the budget. A completed nonzero remote exit is a known tool error and
sets `ToolResult.IsError`; missing/ambiguous receipts are unknown.

StreamingBash preserves raw LF, CRLF, and EOF-tail bytes in its capture and
counts those source bytes against the combined cap. Its display output may add
one synthetic newline between nonempty stdout/stderr plus the fixed truncation
marker; these presentation bytes are outside the source-byte budget. It
continues draining after truncation and after scanner errors, while surfacing
reader failures.

## Verification evidence

The audited-baseline and follow-up red runs are recorded in
[r1-tools-process-red.log](r1-tools-process-red.log). The final exact test list,
targeted regressions, platform package-race runs, and cross-compilation results
are recorded in [r1-tools-process-green.log](r1-tools-process-green.log).

Linux runtime evidence is specific to the `golang:1.26` arm64 Docker image with
`--init`; it does not establish behavior in a no-init container. The earlier
no-init attempt stalled while downloading dependencies and was stopped without
a test result. A no-init PID 1 may leave orphaned killed descendants as
zombies, keeping a process group observable and therefore quarantined; do not
generalize the `--init` result to every Docker setup. Cross-compilation is
reported separately from runtime testing.

The upstream [sshpass 1.10 source](https://downloads.sourceforge.net/project/sshpass/sshpass/1.10/sshpass-1.10.tar.gz)
shows that it starts its SSH child in a new session and waits for that child;
the [sshpass manual](https://manpages.ubuntu.com/manpages/noble/man1/sshpass.1.html)
documents its reserved exit values and forwarding of the child status. These
constraints explain the password-mode unknown classifications above.
