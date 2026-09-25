# R2 workspace index review

**Verdict: accepted for the workspace-index and recovery-authority slice at `b7208953de6f05242fce988fd1d6ad6262ff6bba`.** This accepts the storage seam only; it does not certify R2 CLI/Loop integration or the separate process-recovery gate.

The earlier bootstrap durability finding on `c32be4e` is closed by this snapshot. `EnsureWorkspace` now re-syncs the index directory and its existing parent chain through `mkdirDurablyWithSync`; post-genesis rename and locked index operations use the same retryable directory-sync path. A visible directory/index left by a failed sync therefore cannot be treated as durable on retry until the required syncs succeed. The injected regressions cover a failed parent sync after `MkdirAll`, a failed parent sync after the genesis rename, and a direct dispatch attempt before successful bootstrap retry. `mkdirDurably` is shared with session and sidecar setup, and its retry test exercises the existing helper itself.

The index contract is otherwise fail-closed at this seam:

- `EnsureWorkspace` is the explicit initializer. It creates private `0700` directories and `0600` lock/index files, refuses an index without its lock witness, and refuses an existing lock whose authoritative index is missing. `InspectDispatchMarkers`, `ApplyDurableResolutions`, and ordinary locked operations do not create a missing index. File opens reject symlinks and validate regular-file type and mode.
- The session preparer acquires the selected session-file lock before reading/recovery. It validates local resolution evidence before calling `ApplyDurableResolutions`, then inspects pending markers while retaining the session lock. The index implementation never opens or calls back into session storage; its lock only covers index read/check/append/sync. This preserves session-file → workspace-index lock order.
- `BeginDispatchBatch` syncs the current index before admission, checks unresolved markers and appends under one flock, and reports `CommittedNow` only after a newly appended marker sync succeeds. Errors return `NotCommitted`; an identical batch, including an already-resolved batch, returns `AlreadyExists` and never grants a replay permit. Without R4 scope proof, every unresolved marker blocks every new action, including reads.
- Marker inspection and resolution use the complete `PendingActionRef`; a mismatched or unreserved occurrence with any conflicting marker is unverifiable. Resolution retries compare the exact checkpoint ID/sequence and typed resolution. The no-marker `NeverDispatched` no-op remains safe only through the validated session-loader caller path.

I independently ran the focused Darwin/arm64 race selection below on the detached `b7208953` snapshot; it passed. It includes all `TestWorkspaceIndex*` cases, the three directory-sync retry regressions, the recovery ordering/validation tests, and a temporary reviewer probe that invokes the real `WriteTool` only when the index returns `CommittedNow`. The probe observed one file write across initial dispatch, durable resolution, reopen, and duplicate retry; it was removed after the run.

```text
rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/session \
  -run '^(TestWorkspaceIndex.*|TestMkdirDurablyRetriesExistingParentChainAfterFailure|TestPrepareResumeAppliesValidatedResolutionBeforeInspectingMarkers|TestPrepareResumeRejectsInvalidLocalResolutionBeforeAuthority|TestPrepareResumeRequiresAuthorityBeforeExposingOpenActionHistory|TestPrepareResumeAuthorityErrorKeepsSessionClosed|TestPrepareResumeLocksBeforeLoadingTheSelectedFile|TestPrepareResumeLockWaitHonorsContextCancellation|TestJSONL_OpenLocksBeforeLoadingAndHonorsContext|TestReviewWorkspaceIndexRetryDoesNotRepeatWriteSideEffect)$' \
  -count=1
```

`git diff --check HEAD^ HEAD` passed. The builder reports that the full `internal/session` race suite also passed on this snapshot; I did not repeat that full suite.

One non-blocking canonicalization edge remains for follow-up before release: `stableWorkspaceRecordID` joins variable string parts with NUL, while the index API and durable row validator require `RunID`/`ActionBatchID` to be non-empty but do not reject NUL. The accepted pair `(RunID="run\x00part", ActionBatchID="tail")` and `(RunID="run", ActionBatchID="part\x00tail")` produces the same dispatch record ID. A temporary boundary test confirmed the second distinct batch is rejected before append (`NotCommitted`) because the duplicate record ID is detected; it does not map to or execute the first batch. The normal run-ID generator emits hex or `claim-<UnixNano>`, and physical session IDs are syntax-checked, so this is not a fail-open finding at this storage seam. Before R2 ships, either reject NUL in all stable-ID components or use an unambiguous length-framed encoding.

The R2 runtime queue, actual CLI/Loop dispatch wiring, child/process recovery, and SIGKILL restart matrix remain outside this review and require their own implementation and evidence.
