# R2 resume durability sync review

## Verdict

Approved for the assigned resume durability fix at `49f7c79ba46d24664d3f724a88a18ce805ab99a2`, based on adapter-design parent `4bb628f737a08a8bf6a31764c8b5a733fb7e1e3e`. The fixed worktree was clean; the commit changes only `internal/session/durable_jsonl.go` and its two session test files. No blocker found in this critical fix. This is not approval of the session adapter or the full R2 integration.

## Review evidence

- [`PrepareResume`](../../../../internal/session/durable_jsonl.go:115) acquires the exclusive file lock before reading. It then reads/repairs JSONL, validates loaded records, active branch and projected view, and analyzes the complete action journal. The new `syncDurableFile(file, p.io)` call is after those local checks and before `ApplyDurableResolutions` or `InspectDispatchMarkers`. Recovery receipts are constructed and appended only after both authority steps. Thus a failed validation sync returns before either authority call, store construction, or receipt recovery.
- The failure path leaves `failed` true, so the existing defer unlocks and closes the file; the function returns a nil `PreparedSession`. The new [`TestPrepareResumeSyncFailureStopsBeforeAuthorityAndReceiptRecovery`](../../../../internal/session/durable_recovery_test.go:99) injects failure through `durableJSONLIO.sync`, verifies the wrapped error with `errors.Is`, exactly one pre-authority sync attempt, zero `Apply`/`Inspect` calls, nil handle, and no synthesized receipt. Its fixture is complete LF-terminated data, so the existing JSONL reader has no tail repair to perform before the injected failure.
- The original complete-LF retry hole described in the [adapter handoff](../r2-session-run-commit-adapter-handoff.md:79) is closed by the updated [`TestDurableWriterPoisonsAfterSyncFailure`](../../../../internal/session/durable_jsonl_test.go:249): after a failed append sync poisons the writer and the handle is closed, resume invokes the injected sync exactly once before returning a writer; retrying the exact entry is idempotent and does not add another sync call. The production default path falls through to `file.Sync()`; the injection is on the actual `PrepareResume` call path.
- Existing tail recovery is preserved. A valid unterminated final record is still delimited and file-synced; only a truncated final JSON suffix is copied to a synced sidecar, followed by truncation and source-file sync; complete malformed rows remain errors. Those branches are unchanged in `readDurableJSONL` and were included in the targeted run. The added re-sync is for the already-existing session file after validation; it does not alter directory-entry state or replace the existing sidecar parent-directory sync.
- Failure cleanup is source-confirmed through the common `PrepareResume` defer; the focused new test checks the returned handle and pre-authority effects, while lock release follows the same `flockUnlock` plus `Close` path for every failed return.

## Validation

The exact selector was enumerated first with `go test -list`; all 15 requested target names matched. The targeted race run on the fixed snapshot passed:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 ./internal/session -run '^(TestPrepareResumeSyncFailureStopsBeforeAuthorityAndReceiptRecovery|TestPrepareResumeReconcilesAbsentReservedMarkerAsInterrupted|TestPrepareResumeReconcilesPresentMarkerAsUnknownWithoutReplay|TestPrepareResumeRequiresAuthorityBeforeExposingOpenActionHistory|TestPrepareResumeAppliesValidatedResolutionBeforeInspectingMarkers|TestPrepareResumeRejectsInvalidLocalResolutionBeforeAuthority|TestPrepareResumeAuthorityErrorKeepsSessionClosed|TestPrepareResumeRejectsInvalidRowsBeforeCallingAuthority|TestPrepareResumeRejectsProjectedHistoryBeforeCallingAuthority|TestDurableWriterPoisonsAfterSyncFailure|TestReadDurableJSONLPreservesValidUnterminatedFinalRecord|TestReadDurableJSONLQuarantinesOnlyTruncatedFinalSuffix|TestReadDurableJSONLRejectsCompleteMalformedFinalRow|TestPrepareResumeLocksBeforeLoadingTheSelectedFile|TestPrepareResumeLockWaitHonorsContextCancellation)$'
ok   github.com/pengjunchen/go-cli/internal/session  1.551s
```

`git diff-tree --check` passed for the reviewed commit. I did not rerun the full session race suite; the builder reports that full suite already passed.
