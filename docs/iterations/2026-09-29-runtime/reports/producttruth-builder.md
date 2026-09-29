# Product capability truth builder report

## Snapshot and scope

- Original builder baseline: `090303945508076bd2f4193d01305135e2e4dd6a`.
- Rebase base: accepted root `57ce1ebf9dfb065d722734aeacf2600b708cf74e`, which contains source base `584d57b491c2af5222c6d67c67918930b1256abb` and its documentation-only successors.
- Rebased commits: `3193e0f9f88883272cf18360cfc5639c3c767b92`, `3b146bcfa321cbb4a4842d875281b16f7c2695a4`, and `40ae97bc5cd5de28454406496da77136dddf1e4f`.
- Worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260930-producttruth`, branch `codex/repair-20260930-producttruth`.
- Accepted-root review: `docs/iterations/2026-09-29-runtime/reviews/producttruth-final-review.md` returned **ACCEPT** for the accepted-root integration.
- Contract: `docs/iterations/2026-09-29-runtime/product-capability-design.md` and its review were treated as frozen inputs. No protected Makefile, CI, or rules files were changed.

The original-to-rebased commit mapping is:

| Original commit | Rebased commit | Change |
|---|---|---|
| `bb4dbf91da8c056918f133dddbed832ed8b02c6d` | `3193e0f9f88883272cf18360cfc5639c3c767b92` | Product behavior, CLI tests, and real-binary test |
| `d8e764ca057528e2f43418f59dc62b726817d246` | `3b146bcfa321cbb4a4842d875281b16f7c2695a4` | Prove model Plan Mode calls stop at registry lookup |
| `cf850f03edc705ea13099bb2d3a68d4596b6c7fe` | `40ae97bc5cd5de28454406496da77136dddf1e4f` | Bilingual user documentation and this report |

The default interactive assembly no longer creates an LLM memory extractor or places stored memory records in the model prompt. `/memory` remains the user-managed list/add/delete/search/clear path. `SessionManagement.MemoryExtractor` remains an explicit library injection point; default CLI assembly leaves it nil. Memory store file-format consistency work from `bf11439` was not repeated.

The default model-facing registry no longer contains `enter_plan_mode` or `exit_plan_mode`. The `/plan enter|exit` slash command remains user-controlled. The real assembled `ApproveAuto` test makes the model emit both removed tool names, checks that they are absent from the provider schema and registry, observes registry-lookup errors before execution, and confirms the mode stays unchanged; the same assembly's user slash commands still enter and exit Plan Mode.

The existing `/help` handler now appends the frozen, human-written default capability boundaries. Its slash-command list remains registry-driven, `/tools` remains a view of the actual tool registry, and no second capability registry was introduced. The English and Chinese README tables now distinguish implemented components from the per-run model registry and describe the selected defaults and limits. The session wording describes recovery by exact physical session ID in a configured session directory and excludes fork, branch switching, snapshot, and migration. The accepted root already contains the durable resume implementation; this slice leaves that path intact.

During rebase, `assemble.go` retained the accepted root's `PhysicalSessionID`, directory-mode session setup, `MemoryStore`, and cleanup wait group. Resolution removed only the default extractor assignment and default extractor/prompt-injection construction. The memory test helper retained the accepted root's directory-based session path.

## RED and GREEN evidence

Before the default memory wiring changed, the multiple-turn default REPL test observed two memory-extraction requests instead of zero; the earlier phase 39 REPL test observed an extraction request and persisted generated memory. The assembly also still exposed Plan Mode transition tools, and the `/help` capability section required by the frozen choices was absent. These were behavioral assertion failures against the previous implementation.

After the change, memory tests prove that ordinary turns neither issue extraction requests nor persist generated facts, that seeded records are not injected into model prompts, and that an explicitly injected library extractor still works. The process-level test builds and runs the real CLI binary: process A adds a memory and has a normal model turn, process B lists/searches/deletes after restart, and process C verifies the deletion after another restart.

## Validation

```text
rtk go test -race ./internal/cli -count=1
Go test: 780 passed in 1 packages

rtk go test -race ./cmd/cli -run '^TestMemoryManualCRUDSurvivesRealCLIRestarts$' -count=1
Go test: 1 passed in 1 packages

rtk go test -run '^$' ./...
# exit 0; compile-only, no tests selected

rtk go vet ./...
# exit 0

rtk golangci-lint run ./...
golangci-lint: No issues found

rtk gofmt -l cmd/cli/memory_persistence_e2e_test.go internal/cli/assemble.go internal/cli/completer_esc_test.go internal/cli/interactive_memory_test.go internal/cli/phase39_e2e_test.go internal/cli/r1_assembly_stream_test.go internal/cli/r4_extra_tool_schema_assembly_test.go internal/cli/repl_session.go internal/cli/slash_handlers.go internal/cli/slash_memory.go internal/cli/slash_test.go
# no output

rtk git diff --check
# clean
```

## Limits

The rebased checks include the complete `internal/cli` race suite, real-binary restart journey, whole-repository package/test compilation, repository-wide vet, and repository-wide lint. This is not a full repository test run, `make verify`, or CI run. The binary journey used a local loopback fake OpenAI-compatible server; no live third-party provider was exercised. No interactive PTY journey or Linux/Windows runtime was tested. The static help also records other frozen boundaries (including hard cost budgets, usage uncertainty, serve profile, remote TUI, image reading, themes/diff, and MCP reload); their independent runtime paths are not validated by this source slice. This work does not claim R3 usage/cost accounting completion.

## Changed files

- `internal/cli/assemble.go`
- `internal/cli/repl_session.go`
- `internal/cli/slash_handlers.go`
- `internal/cli/slash_memory.go`
- `internal/cli/completer_esc_test.go`
- `internal/cli/interactive_memory_test.go`
- `internal/cli/phase39_e2e_test.go`
- `internal/cli/r1_assembly_stream_test.go`
- `internal/cli/r4_extra_tool_schema_assembly_test.go`
- `internal/cli/slash_test.go`
- `cmd/cli/memory_persistence_e2e_test.go`
- `README.md`
- `README.zh-CN.md`
- `docs/iterations/2026-09-29-runtime/reports/producttruth-builder.md`
