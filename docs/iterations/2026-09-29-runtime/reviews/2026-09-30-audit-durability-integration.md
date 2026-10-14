# Audit durability integration review

Source commits `71a6304`, `406fe8e`, `e5bdc37`, and `4b2aab2` were
independently read before integration as `e6e95d5`..`55f0c26`.

The approval and production audit writers now sanitize payloads before
hashing, append a complete JSONL record, sync it, and only then advance the
in-memory hash. Failed writes roll back to the prior size; an uncertain
rollback quarantines the writer. A stable private sidecar lock serializes
read/verify/append across processes. Legacy unhashed or malformed records
fail closed before a new append. The verified-tail cursor only skips a full
rescan when the file identity, size, and ctime are unchanged under that lock;
readback always verifies the complete chain. Darwin and Linux provide that
fingerprint; unsupported platforms fall back to scanning or fail closed at
the OS lock layer.

The prepared approval path records the classifier proposal separately from
the actual callback choice and resolution. It does not yet record the final
tool dispatch outcome; that requires the prepared terminal observer and
remains outside this accepted slice. The existing production log rotates to
one `.1` generation, so this change is not an indefinite audit retention
policy.

After integration, `go test -race -count=1 ./internal/privatefile
./internal/approval ./internal/production` passed on `55f0c26`. The tests
include multi-process serialization, same-size historical tampering,
rollback poisoning, and lock contention. This is a focused gate; full
`make verify`, tagged E2E, coverage, CLI/PTY, and Dockerfile validation still
need a final fixed-SHA run after the remaining source lands.

The subsequent broader `make verify` at `c6f7b82` **failed**. The new
private-parent requirement for audit sidecar locks rejects multiple existing
CLI and E2E audit paths whose immediate parent is accessible by group or
others; tool calls then stop before dispatch. This is an integration
regression in the accepted narrow audit slice, not a passing release gate.
The exact log is `/tmp/go-cli-verify-c6f7b82.log`. A repair must preserve
private trust-file policy and cross-process audit serialization while
supporting the configured/default audit locations. The same run also found
independent MCP and R6 fixture failures; those are owned by their respective
lanes. Audit integration acceptance is held open until the broader gate is
green on a fixed source snapshot.
