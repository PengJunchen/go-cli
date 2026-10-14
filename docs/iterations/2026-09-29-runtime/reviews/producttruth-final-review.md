# Product capability truth final review

**Verdict: ACCEPT for the accepted-root integration.** Reviewed product commits: `bb4dbf91da8c056918f133dddbed832ed8b02c6d`, `d8e764ca057528e2f43418f59dc62b726817d246`, and `cf850f03edc705ea13099bb2d3a68d4596b6c7fe`. The product branch is based on `090303945508076bd2f4193d01305135e2e4dd6a`; the evaluated integration base was current root `dbc0cfdf171698477e45ab050e062cf73e9015d2`, containing accepted source commit `25b88ba890f59be488e2ea5defbf09186baf873d`. The disposable integration overlay ended at `3660d1700057612c5a5b065f09fe14207933fcaa`.

There are no P0/P1 product findings against that integrated result. The frozen design review remains the authority for the manual-only Memory and user-controlled Plan Mode choices.

## Findings

- **Memory is manual-only in the default CLI.** The integrated `AssembleAgent` result keeps the `MemoryStore` for `/memory`, keeps the accepted `PhysicalSessionID`, and leaves `MemoryExtractor` unset (`internal/cli/assemble.go`, `SessionManagement` assembly). Default loop setup no longer reads the full memory store into `SystemPromptOptions`, and `REPLSession.extractMemory` returns immediately when the extractor is nil (`internal/cli/repl_session.go`). The explicit library injection seam remains available to callers that deliberately construct an `AgentAssembly`; `TestExplicitLibraryMemoryExtractorCanBeInjected` covers that opt-in case. Default-path tests verify zero extraction requests, no persisted extracted facts, and no stored-memory canary in model requests. Manual add/list/search/delete and restart persistence remain covered.

- **Plan Mode transitions are user-controlled.** The default trusted-control list no longer registers `enter_plan_mode` or `exit_plan_mode` (`internal/cli/assemble.go`). The `/plan enter|exit` handler still invokes the same `PlanCtrl`. `TestAutoFullModelCannotChangePlanModeButUserSlashCan` checks that an `auto_full` model request omits both tools, guessed calls fail at registry lookup without changing state, `/tools` omits them, and user slash commands still enter and exit Plan Mode.

- **Discovery and documentation match the integrated runtime.** `/help` has a static, manually maintained capability-boundary section while retaining the dynamic slash-command list (`internal/cli/slash_handlers.go`). `/tools` still calls `ToolRegistry().List(ctx)` and prints those actual definitions. The English and Chinese READMEs now label the table as implemented components rather than the per-run model registry, describe default image and web-search limits, identify in-process task stores, and state the default Memory and Plan Mode boundaries.

  The declarations that depended on newer accepted source work are accurate in the integrated overlay: interactive resume accepts an exact physical ID and rejects ambiguous boolean `--resume`; `serve` rejects `ask` before token or listener work; remote TUI is rejected before session setup; and only unified diff style is accepted. The relevant paths are `internal/cli/repl_session.go`, `internal/cli/serve_cmd.go`, `internal/cli/interactive.go`, and `internal/config/validator.go` on the accepted root. Their focused tests passed with the product commits applied.

## Integration note

Applying `bb4dbf9` directly onto the accepted root produces content conflicts in `internal/cli/assemble.go` and `internal/cli/interactive_memory_test.go`. The temporary overlay resolved them by preserving the root's `PhysicalSessionID` field and directory-mode session setup, keeping the `/memory` store and cleanup wait group, and removing only the default `MemoryExtractor` assignment. The test helper retained the root's directory-based session path. Apply the same semantic resolution when integrating; the accepted-root index was not changed, and the temporary overlay was removed after validation.

The product branch at `0903039` predates the accepted entrypoint and durable-session fixes, so its standalone `/help` and README assertions should not be treated as a releasable tree. This is baseline ordering, not a product regression in the reviewed slice. Acceptance here is for the product commits rebased onto the accepted root. I did not run a separate RED suite against `0903039`; the pre-change `assemble.go` source in that baseline contains the full `MemoryStore.List` prompt injection, default `NewLLMMemoryExtractor`, and model-facing Plan Mode tools that the added regression tests cover.

## Validation

- On the isolated product worktree: focused `go test -race ./internal/cli` passed 7 tests; `go test -race ./cmd/cli -run '^TestMemoryManualCRUDSurvivesRealCLIRestarts$'` passed. The latter builds and launches the real CLI in three separate processes.
- On the accepted-root integration overlay: focused `go test -race ./internal/cli` passed 16 tests covering Memory, Plan Mode, `/help`, `/tools`, exact resume, remote-TUI rejection, and Serve approval handling. The real-binary Memory restart test passed again.
- No full `make verify`, full package suite, PTY, or production validation was performed for this review.
