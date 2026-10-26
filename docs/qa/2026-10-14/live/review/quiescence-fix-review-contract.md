# Independent review contract: local search-tool quiescence proof

This is a read-only reviewer checklist for the Go fix assigned to the source owner. The live smoke showed a real grep tool result while the durable run remained unresolved. The evidence does not justify weakening the core quiescence or owner-publication checks.

## Required positive cases

| Case | Required evidence |
| --- | --- |
| Managed local helper | Execute the prepared `GrepTool` path that actually starts the managed helper. Only attest `CompletionOwnedProcessGroupJoined` after the child group, output readers/writers, and lifecycle are joined. The completion reporter must receive the proof and a done signal. |
| Synchronous no-process path | Exercise the explicit pure-Go/fallback search path with no child process. It may attest a distinct synchronous/no-process proof only after the concrete search returns. The proof must identify this execution mode; it must not be inferred from a generic tool return. |
| End-to-end prepared dispatch | Through the trusted registry, execute each supported path and verify the prepared invocation receives the matching proof. The successful tool result must still pass the existing run-commit and owner-join path. |

## Required negative cases

| Case | Required invariant |
| --- | --- |
| Unattested wrapper or replacement tool | A wrapper returning a normal result without the concrete trusted proof remains cleanup-pending/unknown. Do not infer quiescence from `Execute` returning. |
| Missing, mismatched, or late proof | Prepared dispatch must reject an unrelated proof kind, a proof from another action, or a reporter that never completes. |
| Helper cancellation / join failure | A started helper whose process group or pipe workers are not joined must not produce the owned-process proof; core retains unknown outcome and the owner must not publish success. |
| Existing core safety guards | `ErrRunCommitQuiescenceUnproven`, `CleanupPending`, unknown durable run outcome, and owner publication failure remain observable when proof is absent. The fix must not convert a successful receipt alone into proof. |

## Review steps when the patch lands

1. Inspect the proof enum, registry wiring, concrete Grep/Find paths, and prepared reporter without editing source.
2. Read the added red/green tests and verify both positive modes and the negative fail-closed cases above.
3. Run only the focused source-owner tests first, then the required repository checks if the owner requests them; record exact command results.
4. Rerun the actual race-instrumented PTY smoke. The `grep` call must correlate provider ID → approval/dispatch → result receipt → matching completion proof → quiescent execution-resolution checkpoint → successful terminal run.
5. Keep any later test failure or missing proof as a failure/inconclusive result; do not rescue it by editing session history or asserting cleanup manually.

The live smoke records a successful grep result receipt and a `tool_outcome=success`, but also `resource_state=cleanup_pending` and `run_outcome=unknown`. Those facts must remain separate throughout the fix and the reviewer report.
