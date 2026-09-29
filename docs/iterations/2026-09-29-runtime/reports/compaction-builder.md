# Durable compaction builder report

## Source and scope

- Implementation base: `258c8f6de9d7e03360aa84536acdaa0c1684ca9e` (the accepted stream-chain integration root).
- Worktree/branch: `../go-cli-runtime-worktrees/repair-20260929-compaction`, `codex/repair-20260929-compaction`.
- This checkpoint implements runtime source metadata, captured PublishedView source snapshots, typed per-call compaction results, request preflight and source/suffix validation, synced compaction projections, summary wrapping/rehydration, and durable Agent hook/manual-compaction fences.
- No CLI adapter, TUI/event occurrence stamping, attempt-accounting authority, native/provider codec, or MCP stdio/tools preparation changes are included. This is not a claim that R3 or CLI integration is complete.

## Behavior and evidence

- Runtime-only source references and receipt outcomes survive core/compaction transformations while remaining absent from JSON serialization. Hydration assigns each history row the exact captured source entry ID and preserves receipt outcomes.
- Every durable request validates source continuity before generation. Over-budget history validates conservative compaction eligibility before invoking a strategy; a complete historical `unknown` receipt can pass unchanged below budget but fails over budget before the compactor, summarizer, projection commit, or provider call.
- Durable compaction accepts only a typed summary result with one exact summary/cursor and an unchanged source-backed suffix. Unsupported source-backed prefixes, same-run synthetic suffix summaries, malformed/open groups, and stale/missing source mappings fail closed before projection commit and generation.
- Compaction is append-and-synced through the run sink before the provider request. A real `SessionRunCommitSink`/JSONL test uses duplicate source text with distinct IDs, asserts the marker's exact old assistant boundary and raw summary before provider entry, then verifies live/reopened PublishedView and source-ID parity. The wrapper appears exactly once. A subsequent reopened run sees the summary as `SourcePriorPublished`, tied to the actual compaction marker ID rather than the prior run cursor.
- Bound Agents bypass the legacy post-terminal hook; direct bound `Agent.Compact` returns the typed unsupported error before mutation or summarizer invocation.
- TDD included a pre-implementation compile-red for the new runtime metadata contract (missing `SourceRef`/typed API), followed by focused conversion/source/projection tests. The real-sink projection test and conservative-unknown gate tests pass in the green run.

## Validation

Passed:

- `go test ./internal/core ./internal/session ./internal/compaction ./internal/llm -count=1`
- `go test -race ./internal/core ./internal/session ./internal/compaction ./internal/llm -count=1`
- `go vet ./internal/core ./internal/session ./internal/compaction ./internal/llm`
- `golangci-lint run ./internal/core ./internal/session ./internal/compaction ./internal/llm`
- `git diff --check`

Known compile gap deferred to the separately owned CLI adapter stage:

- `go test ./... -run '^$' -count=0` currently reaches the two expected stale result conversions: `internal/cli/compaction_adapter.go:41` and `internal/cli/registry_adapters.go:130` pass `CompactionResult` where `[]TurnItem` is expected. This stage deliberately leaves those CLI files untouched; the CLI adapter stage must update them and rerun repository-wide compilation before integration.
