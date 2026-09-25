# R2 workspace-index NUL identity follow-up

**Verdict: accepted for the NUL-identity delta at `a9fedcb7f484013227c69b7e58497f7bb85fa775`** (parent `b7208953de6f05242fce988fd1d6ad6262ff6bba`).

The formerly ambiguous tuple components are rejected consistently. Dispatch normalization and action validation cover `WorkspaceID`, `SessionID`, `RunID`, `ActionBatchID`, and `ActionID`; marker inspection and resolution admission validate the same stable-reference fields. Persisted genesis, dispatch, and resolution rows also reject NUL-bearing workspace, record, and action identities before rebuilding index state. The stable-ID encoding itself is unchanged, so ordinary IDs keep their existing record IDs and readable indexes.

The regression reproduces both dispatch-ID and resolution-ID ambiguity using `RunID="run\x00part"` / `ActionBatchID="tail"` and `RunID="run"` / `ActionBatchID="part\x00tail"`. It verifies rejection through the public index operations and row validation, then confirms invalid API attempts leave the index at its genesis row and an ordinary-ID batch still receives `CommittedNow`.

I enumerated and ran the exact new test under race instrumentation; it passed on Darwin/arm64:

```text
rtk env GOMAXPROCS=2 go test -list 'TestWorkspaceIndexRejectsNULInStableRecordIdentityParts' ./internal/session
rtk env GOMAXPROCS=2 go test -race -p 2 -run '^TestWorkspaceIndexRejectsNULInStableRecordIdentityParts$' ./internal/session -count=1
```

`git diff --check HEAD^ HEAD` passed. This supplements the workspace-index review; it does not extend the R2 runtime-integration or process-recovery acceptance scope.
