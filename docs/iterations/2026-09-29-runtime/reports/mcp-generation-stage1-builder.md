# Atomic MCP generation Stage‑1 builder report

## Baseline and scope

- Reviewed baseline: `974eb6289077f3f355fcd62d1371dcb540b4ac87`, descended from the Stage‑1 source `a6aaa816c63068ec47286d229a29ed1a918027b9`.
- Worktree/branch: `/Users/pengjunchen/Documents/github/go-cli-repair-20260930-mcpgen` / `codex/repair-20260930-mcpgen-stage1`.
- Scope: address the follow-up review finding that a frozen MCP definition retained the candidate `Execute` method value and therefore the caller-owned adapter receiver.
- Review state: the prior `974eb62` review rejected that method-value alias. This patch is prepared for a fresh independent review; it does not claim acceptance.

## Review history

The first Stage‑1 review found that trusted registration paths could bypass MCP name reservations and that the DTR list barrier test did not reach the concrete generation-provider branch. The `974eb62` follow-up added reservation checks for direct and adapter-forwarded trusted registration, plus a deterministic test that proves the DTR read lock covers the backing snapshot while a complete generation replacement waits.

The next review reproduced a separate P1: `candidate.Definition.Execute` kept a method value bound to the caller's mutable `MCPToolAdapter`. Mutating its remote tool name after Begin changed the tool called by the published definition. This patch removes `ToolDefinition` and arbitrary execute callbacks from `MCPToolCandidate`.

## Changes

`MCPToolCandidate` is now an opaque value containing private copied registry and remote names, description, frozen schema, a narrow `MCPGenerationClientHandle`, and its optional dispatch lease. Its constructor snapshots and validates schema data. A zero-value candidate, missing handle, or legacy adapter without a client-generation owner fails closed before the DTR lock is taken.

`MCPClientGeneration` is the MCP-owned implementation of the narrow call interface. A source owner creates one generation handle for its client instance, and adapters created from that handle produce candidates through `MCPToolAdapter.MCPGenerationCandidate`. The candidate definition executes using the captured remote tool name through that generation object; it does not retain the original `MCPToolAdapter` or its `Execute` method value. The API does not claim to deep-copy arbitrary user closures or make reconfiguring a client in place safe. Source lifecycle ownership and lease-backed retirement remain separate work.

The trusted-registration reservation checks and the DTR/backing-registry snapshot barrier from `974eb62` remain covered by their Stage‑1 regression tests.

## TDD and verification

- RED against `974eb62`: `rtk go test ./internal/mcp -run '^TestMCPGenerationCandidateDoesNotRetainMutableAdapterReceiver$' -count=1` failed because the remote call used `mutated` instead of `original` after the adapter was changed between Begin and Publish.
- GREEN: the same probe now constructs through `MCPClientGeneration` and `MCPToolAdapter.MCPGenerationCandidate`, mutates the adapter's tool name, description, schema, and client field, then verifies the published definition keeps the original metadata and calls the original remote tool.
- Zero-value and legacy-adapter tests verify candidates without the generation capability fail closed.
- `rtk go test ./internal/tools ./internal/mcp -run 'MCPGeneration|DeferredListTakesDTRSnapshot|MCPToolAdapter' -count=1`: passed, 18 tests.
- `rtk go test -race -count=1 ./internal/tools ./internal/mcp ./internal/cli`: passed, 2,185 tests across three packages.
- `rtk go test -race -count=5 ./internal/tools -run '^TestDeferredListTakesDTRSnapshotBeforeBackingSnapshot$'`: passed, five runs.
- `rtk go vet ./internal/tools ./internal/mcp ./internal/cli`: passed.
- `rtk golangci-lint run ./internal/tools ./internal/mcp ./internal/cli`: passed with no issues.
- `rtk git diff --check`: passed.

This report covers Stage‑1 registry generation publication and its opaque execution binding only. It does not claim PreparedDispatchLease acquisition, CLI source reload integration, client retirement/join, stdio lifecycle integration, or full R6 completion.
