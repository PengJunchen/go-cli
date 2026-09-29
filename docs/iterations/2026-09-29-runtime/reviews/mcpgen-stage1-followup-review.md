# MCP atomic generation Stage 1 follow-up review

**Verdict: REJECT.** Reviewed exact commit `974eb6289077f3f355fcd62d1371dcb540b4ac87`, parent `a6aaa816c63068ec47286d229a29ed1a918027b9`. Frozen design SHA-256: `37fa91cb2f71b0e0280039d765e3e14080f5655decaab0cbd7daddf850da5015`.

## Findings

1. **[P1] The frozen definition still retains the caller's mutable executor receiver.** The new wrapper snapshots name, description, schema, and prompt guidelines, but `prepareMCPGenerationCandidate` stores `candidate.Definition.Execute` as its executor (`internal/tools/mcp_generation.go:285-306`). For a pointer-receiver tool this method value retains the original receiver; it does not copy that receiver's state. A temporary review test began a generation with `mcp__srv__original`, changed the caller-owned tool name to `mcp__srv__mutated` before `Publish`, loaded the published wrapper, and executed it using the frozen original call name. The wrapper still reported the original name and schema, but execution returned output containing the mutated name. The committed snapshot test checks only `Name` and `Parameters`, so it misses this execution mismatch. Capture execution from candidate-owned copied MCP tool data and the intended generation client, or change the candidate API so the immutable executor inputs are explicit.

## Rechecked fixes

- **Trusted reservations: pass.** Direct `DefaultToolRegistry` trusted registration now checks `mcpReservations` under the registry lock, and `DeferredToolRegistryAdapter` holds the DTR read lock while forwarding trusted registrations. The committed test checks both direct and adapter `RegisterTrustedControl` calls return `ErrMCPNameReserved` without advancing the fence.
- **Concrete-generation List barrier: pass.** `deferredListSnapshotBarrier` embeds `*DefaultToolRegistry`, so Go promotes its `mcpGenerationRegistry()` method and the test reaches the generation-aware `List` branch. The barrier proves the DTR read lock is held across the backing snapshot, blocks replacement during that snapshot, then checks complete old and new generations. This corrects the prior review's List-test finding.

## Stage boundary

Prepared-dispatch lease acquisition and CLI/client retirement remain outside this commit and are not credited here. The lease is stored, but it is not acquired or threaded through prepared dispatch; this follow-up makes no old-generation lease-safety claim.

## Verification

- `go test -race ./internal/tools ./internal/mcp ./internal/cli`: passed, 2,183 tests across three packages.
- `go test -race -count=5 ./internal/tools -run '^TestDeferredListTakesDTRSnapshotBeforeBackingSnapshot$'`: passed, five runs.
- `go vet ./internal/tools ./internal/mcp ./internal/cli`: passed.
- `golangci-lint run ./internal/tools ./internal/mcp ./internal/cli`: no issues found.
- A temporary review-only executor-receiver test failed as expected: expected original output, received the mutated value. No review tests were added to the commit.
