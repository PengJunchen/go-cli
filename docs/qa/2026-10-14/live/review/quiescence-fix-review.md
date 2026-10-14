# Independent review: scoped search completion proof

Review date: 2026-10-14  
Reviewed base: `e0493e5fe92f98ee460a0e25740a97852272ddf2` plus the uncommitted source-owner diff visible during this review.  
Reviewer scope: read-only inspection of the Go patch and its tests; no Go source or test files were changed by this review.

## Decision

**Accept the proof design for source commit, with one coverage note below.** The patch resolves the observed Grep/Find completion gap without treating generic executor return, a successful receipt, or the internal scoped-search registration mode as positive completion proof. This is a source-patch review only; it does not pass or replace the live Magpie/PTy acceptance run.

## Evidence and safety properties

- `internal/tools/trusted_registry.go` assigns the private `completionScopedSearchJoined` mode only to concrete `*GrepTool` and `*FindTool` registrations with a valid non-empty workspace-read scope. Unscoped or invalid registrations do not receive this proof mode.
- `internal/tools/prepared.go` carries that mode into both synchronous and streaming prepared dispatch. Each dispatch binds a reporter to that exact invocation and calls `finish` only while unwinding the concrete executor call. The mode is not itself a valid final proof.
- `internal/tools/grep.go` and `internal/tools/find.go` report one concrete path from a defer: the managed process lifecycle when a helper started, or a closed synchronous no-process signal when it did not. A forced pipe close does not get reclassified as the no-process path.
- `internal/tools/prepared_completion.go` publishes only `CompletionOwnedProcessGroupJoined` after the helper signal closes, or `CompletionInlinePipelineJoined` after the explicitly attested synchronous path closes. Both require the prepared executor to return successfully. Pending signals keep the prepared completion open; missing reports close without proof; failed execution invalidates proof. `completionScopedSearchJoined` is excluded by `validPreparedCompletionProof` and cannot be reported as a final proof.
- The existing run-commit quiescence and owner-publication guards remain in place. The core tests exercise trusted prepared Grep/Find through the terminal run path and require matching receipts, quiescent execution resolution, and successful terminal outcome. The patch does not convert result receipts into cleanup evidence.

## Tests reviewed and run

The new cases cover pure-Go fallback and managed-helper proof for both tools, pending helper/fallback signals, missing report, failed executor, invalid hybrid proof, late report, and duplicate/mixed attachment rejection. The escaped-writer tests keep the escaped process observable until assertions finish and verify that it does not yield positive proof or fallback output.

I independently ran both focused commands successfully:

```text
rtk proxy go test ./internal/tools ./internal/core -run 'TestPreparedCompletionScopedSearchRequiresOneConcreteAttestation|TestPreparedScopedSearchReports(SynchronousFallback|ObservedHelper)Completion|TestPreparedScopedGrepForcedPipeCloseDoesNotAttestInlineFallback|TestRunCommitConsumerResolvesScopedSearchAfterCompletionProof' -count=1
ok internal/tools
ok internal/core

rtk proxy go test -race ./internal/tools ./internal/core -run 'TestPreparedCompletionScopedSearchRequiresOneConcreteAttestation|TestPreparedScopedSearchReports(SynchronousFallback|ObservedHelper)Completion|TestPreparedScopedGrepForcedPipeCloseDoesNotAttestInlineFallback|TestRunCommitConsumerResolvesScopedSearchAfterCompletionProof' -count=1
ok internal/tools
ok internal/core
```

The source owner separately reported full `internal/tools` and `internal/core` package checks plus its target race check passing. Those wider checks were not repeated by this reviewer.

## Coverage note and remaining acceptance

The prepared escaped-writer negative is explicit for Grep. Find has a direct forced-pipe-close test that verifies its reporter emits no positive proof while the escaped writer remains alive, plus prepared helper/fallback positives; there is no equivalent prepared-scoped Find escaped-writer test in this diff. Both tools share the same completion-reporting shape, so this is a coverage asymmetry rather than a demonstrated defect.

The Go proof patch is acceptable for commit. A fresh race-instrumented live PTY run must still demonstrate the model-requested tool call through receipt, matching completion proof, quiescent resolution, and successful terminal run before the prior live failure is considered fixed. Presentation, denial, and natural-compaction acceptance also remain separate until their own real-run evidence exists.
