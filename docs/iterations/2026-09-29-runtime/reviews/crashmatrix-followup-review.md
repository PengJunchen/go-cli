# Durable CLI live-orphan crash matrix follow-up review

**Decision: ACCEPT** test-only crash matrix commit `ba6af18c12de9bbe317d91f8e65f818471476e62`, whose direct parent is `584d57b491c2af5222c6d67c67918930b1256abb`. The previous P1 replay-observability gap and P2 diff-check issue are resolved in this candidate. No P1 or P2 findings remain in this bounded stage.

## Exact candidate and verification

The shared checkout was already ahead at `deefa15a3b8eed0d14f29f3d6b395aec27f1491a`, so I reviewed and tested the exact candidate in a detached temporary worktree at `ba6af18c12de9bbe317d91f8e65f818471476e62`, parent `584d57b`. The temporary worktree was removed after verification. The candidate changes only the subprocess test and its builder report.

The focused actual-binary test built `./cmd/cli` from the candidate and passed on Darwin in 32.476s:

```text
rtk proxy go test -v -count=1 ./cmd/cli -run '^TestDurableCLIBinaryQuarantinesLiveOrphanedBashAndNeverReplays$'
```

The focused race run passed in 33.356s, the full `cmd/cli` race package passed in 129.429s, `rtk go vet ./cmd/cli` passed, and `rtk proxy git diff --check 584d57b491c2af5222c6d67c67918930b1256abb ba6af18c12de9bbe317d91f8e65f818471476e62` passed with no output.

## Reviewed behavior

The revised Bash command appends an invocation ID containing the fixture nonce, shell PID, and Bash random value to a start ledger, then appends that same ID to the effect ledger. It runs `sync` after each append and exits on a failed append or sync. The test reads both ledgers and checks exact counts at the gated start, before and after parent kill, after each exact reopen, after the distinct-session rejection, and after child exit. Each expected count remains one start and one effect; any second Bash execution or second visible effect adds a ledger record and fails those assertions. The heartbeat also carries the original invocation ID, so a different command instance cannot masquerade as the surviving child.

The fixture builds and runs the actual CLI with its registered Bash builtin. It waits for the real session JSONL intent and synced workspace `dispatch_ready` marker before opening the effect gate. The run-commit path syncs the intent before it records the dispatch marker, and the workspace index append syncs that marker before granting dispatch. The test kills only the CLI process through its owned `exec.Cmd.Process`; the observed parent exit is `signal: killed` / `-1`, while the original invocation's heartbeat advances afterward. It checks no receipt or terminal before exact reopen, then verifies the same physical ID gains one `unknown`/`dispatched` receipt without another effect or dispatch.

A second physical session in the same workspace attempts a write and fails closed: its effect file is absent, its durable intent has no receipt or terminal, and the workspace index has no dispatch marker for it. Exact reopening of that session produces one `interrupted`/`not_dispatched` receipt from the marker-absent recovery path. The test reads these records from the real session JSONL; the fixture writes only its control and effect files, so it does not fabricate session receipts or terminals.

The normal release path opens the nonce-bound effect and release gates, waits for exit proofs matching every recorded start-ledger entry, and verifies the heartbeat becomes stable. Failure cleanup also opens both gates and waits for all recorded exit proofs with a deadline; when a heartbeat exists, it checks that it has stopped advancing. The subprocess cleanup uses the owned CLI process handle if it must stop a still-running parent; it never signals a PID recovered from data or kills a guessed process group.

The prior P1 is resolved by append-only invocation/effect records with count readback throughout the recovery sequence. The prior P2 is resolved: the exact candidate diff check is clean.

## Remaining boundary

The live SafeRead matrix is still not covered. This test configures `bash` and `write`; it does not perform a real safe read while the orphan marker is unresolved or prove the older mutation marker remains after a permitted read resolves. This acceptance does not claim those SafeRead behaviors are validated.
