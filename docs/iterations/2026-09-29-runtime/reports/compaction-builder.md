# Durable compaction builder report

## Source and scope

- Implementation base: `25b88ba890f59be488e2ea5defbf09186baf873d` (accepted durable CLI/R2 integration root).
- Core source commit: `f3cac72f5b7771b76d5a718da22ea512b7fb0a12` (durable source projections and core compaction contract).
- CLI/API source commit: `a7adc76187d0ae6bacd123511b29ae1501b8b3b8` (typed API call-site migration and same-sink history hydration).
- Worktree/branch: `../go-cli-runtime-worktrees/repair-20260929-compaction`, `codex/repair-20260929-compaction`.
- These source commits implement runtime source metadata, captured PublishedView source snapshots, typed per-call compaction results, request preflight and source/suffix validation, synced compaction projections, summary wrapping/rehydration, durable Agent hook/manual-compaction fences, typed API call-site migration, and CLI prompt/REPL hydration from the same bound run sink before execution.
- No TUI/event occurrence stamping, attempt-accounting authority, small-model assembly, native/provider codec, or MCP stdio/tools preparation changes are included. Prompt/REPL source hydration and typed API call-site migration are included; this is not a claim that R3 is fully closed.

## Behavior and evidence

- Runtime-only source references and receipt outcomes survive core/compaction transformations while remaining absent from JSON serialization. Hydration assigns each history row the exact captured source entry ID and preserves receipt outcomes.
- Every durable request validates source continuity before generation. Over-budget history validates conservative compaction eligibility before invoking a strategy; a complete historical `unknown` receipt can pass unchanged below budget but fails over budget before the compactor, summarizer, projection commit, or provider call.
- Durable compaction accepts only a typed summary result with one exact summary/cursor and an unchanged source-backed suffix. Unsupported source-backed prefixes, same-run synthetic suffix summaries, malformed/open groups, and stale/missing source mappings fail closed before projection commit and generation.
- Compaction is append-and-synced through the run sink before the provider request. A real `SessionRunCommitSink`/JSONL test uses duplicate source text with distinct IDs, asserts the marker's exact old assistant boundary and raw summary before provider entry, then verifies live/reopened PublishedView and source-ID parity. The wrapper appears exactly once. A subsequent reopened run sees the summary as `SourcePriorPublished`, tied to the actual compaction marker ID rather than the prior run cursor.
- Bound Agents bypass the legacy post-terminal hook; direct bound `Agent.Compact` returns the typed unsupported error before mutation or summarizer invocation.
- Prompt and REPL run paths hydrate durable history from the exact sink bound to that run, preserving source references before `Harness.Submit` or `TurnRunner.RunTurn`. The existing e2e typed-result fakes and assertions were migrated along with the API.
- TDD included a pre-implementation compile-red for the new runtime metadata contract (missing `SourceRef`/typed API), followed by focused conversion/source/projection tests. The real-sink projection test and conservative-unknown gate tests pass in the green run.

## Validation

Passed:

- `go test ./internal/core ./internal/session ./internal/compaction ./internal/llm -count=1`
- `go test -race ./internal/core ./internal/session ./internal/compaction ./internal/llm -count=1`
- `go test ./... -run '^$' -count=0`
- `go test -race ./internal/cli -count=1 -timeout=2m`
- `go test ./tests/e2e_20260802 -count=1`
- `go test ./tests -count=1`
- `go vet ./internal/core ./internal/session ./internal/compaction ./internal/llm ./internal/cli ./cmd/cli ./tests ./tests/e2e_20260802`
- `golangci-lint run ./internal/core ./internal/session ./internal/compaction ./internal/llm ./internal/cli ./cmd/cli ./tests ./tests/e2e_20260802`
- `git diff --check`

All listed commands passed after the CLI/API migration. Full non-race `go test ./...` was not run. UI event stamping, small-model assembly, attempt accounting, native/provider codecs, and MCP stdio/tools preparation remain outside this checkpoint; this is not a claim that R3 is fully closed.
