# Atomic MCP generation Stage‑1 builder report

## Baseline and scope

- Base: `a6aaa816c63068ec47286d229a29ed1a918027b9` (the isolated Stage‑1 commit under review).
- Worktree/branch: `/Users/pengjunchen/Documents/github/go-cli-repair-20260930-mcpgen` / `codex/repair-20260930-mcpgen-stage1`.
- Scope: fix the two reproduced Stage‑1 review findings and make the DTR list interleaving test exercise the concrete generation-provider branch.

## Changes

`BeginMCPGenerationUpdate` now snapshots each candidate’s name, description, schema, and prompt guidelines into a private registry-owned definition before retaining it. Deferred loading and materialization use this snapshot, so later caller changes cannot rename a published registration or replace its schema. The executor remains bound to the candidate’s captured `Execute` method.

Every registry-owned reservation now applies to trusted registration paths as well as ordinary eager/deferred registration. The shared registration boundary checks reservations under the DispatchFence and registry mutex; adapter-forwarded trusted registration also holds the DTR read lock through delegation, preserving DTR-before-fence-before-registry ordering.

`TestDeferredListTakesDTRSnapshotBeforeBackingSnapshot` now publishes and materializes a real MCP source generation, pauses the concrete generation-provider backing `List`, proves the DTR read lock is held at that point, and verifies the reader returns the complete old generation before the writer publishes the complete new generation. The generic backing-registry retry test remains separate.

## TDD and verification

- RED on the exact base: trusted-control registration through both `DefaultToolRegistry` and `DeferredToolRegistryAdapter` returned nil for an MCP-reserved name and advanced the registry fence; mutating a candidate name between `Begin` and `Publish` made loading the original name fail with a definition-name mismatch.
- GREEN: `rtk go test ./internal/tools -run 'Test(MCPGenerationRejectsTrustedRegistrationOverReservedName|MCPGenerationSnapshotsCandidateNameAndSchemaBeforePublish|DeferredListTakesDTRSnapshotBeforeBackingSnapshot)$' -count=1` passed.
- `rtk go test -race -count=1 ./internal/tools ./internal/mcp ./internal/cli`: passed, 2,183 tests across three packages.
- `rtk go test -race -count=5 ./internal/tools -run 'Test(MCPGeneration|DeferredList)'`: passed, 45 test runs.
- `rtk go vet ./internal/tools ./internal/mcp ./internal/cli`: passed.
- `rtk golangci-lint run ./internal/tools ./internal/mcp ./internal/cli`: passed, no issues.
- `rtk git diff --check`: passed.

This report covers the Stage‑1 registry and DTR behavior only. It does not claim PreparedDispatchLease integration, CLI reload/config integration, stdio generation support, or full R6 completion.
