# R1 design review

Review date: 2026-09-25  
Review target: `docs/iterations/2026-09-25-runtime/r1-contract.md`  
Review type: independent design and lane-boundary review; no implementation acceptance.

## Decision

**Approved for implementation.** The contract is specific enough for the builders to proceed. This is a design decision only; code, red/green tests, race checks, and final integration still require review.

## Reviewed boundaries

- The owner retains the shared run claim until every worker and pending resource is quiescent and the canonical transcript is committed. Unknown action outcome and live resource state remain independent; timeout alone is never proof of termination.
- The transcript is a per-call typed result, with explicit input kind and authoritative tool receipt keyed by the original tool-call ID. Events remain a lossy projection. Lane 01 exclusively owns `types.go`; lane 02 consumes its seam; lane 03a owns lifecycle and worker tracking; lane 03b owns Bash containment and the pending-execution error seam.
- The output guard runs on the complete candidate after the full continuation chain. Each named response accumulator has a hard cap, continuation count and accumulator sites are fixed to bound the total by a constant factor, and the contract rejects an unbounded hidden copy or a per-copy reservation allocator. No text or tool call is exposed before the complete response passes; provider errors, incomplete protocol, continuation failures, and overflow fail closed.
- The owner closes/cancels the EventStream when its bounded deadline expires, waking a blocked `Send` while leaving the independent status handle readable. Stream closure does not prove workers stopped or release the claim.
- Harness run-span ownership transfers to the per-run owner across actual execution. The caller-visible span ends once with the caller's observed completion or `cleanup_pending`; continued cleanup is recorded separately.
- Bash cancellation requires supported process-group/job containment, child reap, pipe drain, and reader join within a bounded budget. Unsupported or unprovable containment remains pending/quarantined rather than being reported as quiescent.

The lane table assigns the conflicting core files to one owner at a time and orders the typed seam before its consumers. I found no remaining design blocker in the current contract.
