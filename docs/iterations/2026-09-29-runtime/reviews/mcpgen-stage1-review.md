# MCP atomic generation Stage 1 review

**Verdict: Request changes.** Reviewed exact commit `a6aaa816c63068ec47286d229a29ed1a918027b9`, parent `69d24d89433eaf3ef0d11d3a61dbc33a61e7a25a`. The frozen design at `docs/iterations/2026-09-29-runtime/mcp-generation-design.md` has SHA-256 `37fa91cb2f71b0e0280039d765e3e14080f5655decaab0cbd7daddf850da5015`.

## Findings

1. **[P1] Candidate definitions are not frozen or copied.** `prepareMCPGenerationCandidate` freezes schema and metadata, but retains `candidate.Definition` directly (`internal/tools/mcp_generation.go:215-260`). `applyDTRState` closes the deferred loader over that same interface (`:329-356`). A review-only test began a generation with a tool named `mcp__srv__original`, changed the caller-owned tool name to `mcp__srv__mutated` before `Publish`, then observed `Get("mcp__srv__original")` fail because the published loader returned the mutated definition. Mutation of parameters or execution behavior can likewise make the registered definition diverge from the frozen schema/identity. Copy the returned MCP tool and its mutable schema into candidate-owned immutable data before deriving metadata or storing the loader.

2. **[P1] Trusted registration paths can shadow a reserved MCP name.** The new guard covers `DefaultToolRegistry.Register`, adapter `Register`, and deferred registration, but exported `RegisterTrustedControl` and `RegisterTrustedNetworkRead` go through `registerDefinitionWithScopeAndCompletion` without checking `mcpReservations` (`internal/tools/trusted_registry.go:224-231,430-490`). The adapter forwards those methods without holding/checking DTR reservations (`internal/tools/deferred.go:316-334`). A review-only test reserved and published `mcp__srv__reserved`, then called `base.RegisterTrustedControl` with the same name; registration returned nil, although the generation contract allows only its transaction or materializer to replace a reservation. Enforce reservation ownership under the backing registry lock on every registration route, including trusted routes, while preserving the generation transaction/materializer as the authorized bypass.

3. **[P2] The committed List barrier test does not exercise the generation-aware lock branch.** The concrete path in `DeferredToolRegistryAdapter.List` correctly takes DTR `RLock` before `DefaultToolRegistry.List` (`internal/tools/deferred.go:405-420`). However, `TestDeferredListTakesDTRSnapshotBeforeBackingSnapshot` wraps the registry in `deferredListSnapshotBarrier`, which does not implement the private `mcpGenerationRegistryProvider`; `List` therefore tests the generic revision-retry path instead of that concrete lock-order branch (`internal/tools/deferred_test.go:511-581`). A temporary same-package barrier wrapper that preserved the provider confirmed the concrete branch holds DTR `RLock` during the backing snapshot. Update the committed barrier to preserve that provider so the acceptance test protects the intended path.

## Stage boundary

Publication/disable are source-scoped and use one checked Registry fence update; rejected publication leaves the fence unchanged, and materialization failure leaves the deferred loader retryable without caching a definition. The focused tests cover these behaviors and source-only disable preserves a foreign registration.

The lease interface and lease pointer are stored in `mcpMaterializedOwner`, but this commit has no `Acquire` call and does not carry leases through `registeredToolSnapshot` into prepared dispatch. This review gives no credit for keeping old clients alive through committed calls or for retirement/join behavior; those remain required before the design’s old-generation lease guarantee can be claimed.

## Verification

- `go test -race ./internal/tools ./internal/mcp ./internal/cli` on the exact commit: passed, 2,181 tests across three packages.
- `go vet ./internal/tools ./internal/mcp ./internal/cli` on the exact commit: passed.
- Temporary review-only tests reproduced findings 1 and 2 (both expected assertions failed); a temporary provider-aware List barrier passed. No review tests were added to the commit.
