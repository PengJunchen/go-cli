# Product capability truth builder report

## Snapshot and scope

- Baseline: `090303945508076bd2f4193d01305135e2e4dd6a`.
- Source/test commits: `bb4dbf91da8c056918f133dddbed832ed8b02c6d` and `d8e764ca057528e2f43418f59dc62b726817d246`.
- Worktree: `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/repair-20260930-producttruth`, branch `codex/repair-20260930-producttruth`.
- Contract: `docs/iterations/2026-09-29-runtime/product-capability-design.md` and its review were treated as frozen inputs. No protected Makefile, CI, or rules files were changed.

The default interactive assembly no longer creates an LLM memory extractor or places stored memory records in the model prompt. `/memory` remains the user-managed list/add/delete/search/clear path. `SessionManagement.MemoryExtractor` remains an explicit library injection point; default CLI assembly leaves it nil. Memory store file-format consistency work from `bf11439` was not repeated.

The default model-facing registry no longer contains `enter_plan_mode` or `exit_plan_mode`. The `/plan enter|exit` slash command remains user-controlled. The real assembled `ApproveAuto` test makes the model emit both removed tool names, checks that they are absent from the provider schema and registry, observes registry-lookup errors before execution, and confirms the mode stays unchanged; the same assembly's user slash commands still enter and exit Plan Mode.

The existing `/help` handler now appends the frozen, human-written default capability boundaries. Its slash-command list remains registry-driven, `/tools` remains a view of the actual tool registry, and no second capability registry was introduced. The English and Chinese README tables now distinguish implemented components from the per-run model registry and describe the selected defaults and limits. The session wording describes recovery by exact physical session ID in a configured session directory and excludes fork, branch switching, snapshot, and migration. This slice documents that selected contract but does not implement or test the session-resume path owned by the durable CLI work.

## RED and GREEN evidence

Before the default memory wiring changed, the multiple-turn default REPL test observed two memory-extraction requests instead of zero; the earlier phase 39 REPL test observed an extraction request and persisted generated memory. The assembly also still exposed Plan Mode transition tools, and the `/help` capability section required by the frozen choices was absent. These were behavioral assertion failures against the previous implementation.

After the change, memory tests prove that ordinary turns neither issue extraction requests nor persist generated facts, that seeded records are not injected into model prompts, and that an explicitly injected library extractor still works. The process-level test builds and runs the real CLI binary: process A adds a memory and has a normal model turn, process B lists/searches/deletes after restart, and process C verifies the deletion after another restart.

## Validation

```text
rtk go test ./internal/cli -count=1
Go test: 741 passed in 1 packages

rtk go test -race ./internal/cli -count=1
Go test: 741 passed in 1 packages

rtk go test ./cmd/cli -run '^TestMemoryManualCRUDSurvivesRealCLIRestarts$' -count=1
Go test: 1 passed in 1 packages

rtk go test -race ./cmd/cli -run '^TestMemoryManualCRUDSurvivesRealCLIRestarts$' -count=1
Go test: 1 passed in 1 packages

rtk go test ./internal/cli -run '^TestAutoFullModelCannotChangePlanModeButUserSlashCan$' -count=1
Go test: 1 passed in 1 packages

rtk go test -race ./internal/cli -run '^TestAutoFullModelCannotChangePlanModeButUserSlashCan$' -count=1
Go test: 1 passed in 1 packages

rtk go vet ./internal/cli ./cmd/cli
# exit 0

rtk golangci-lint run ./internal/cli ./cmd/cli
golangci-lint: No issues found

rtk gofmt -l cmd/cli/memory_persistence_e2e_test.go internal/cli/assemble.go internal/cli/completer_esc_test.go internal/cli/interactive_memory_test.go internal/cli/phase39_e2e_test.go internal/cli/r1_assembly_stream_test.go internal/cli/r4_extra_tool_schema_assembly_test.go internal/cli/repl_session.go internal/cli/slash_handlers.go internal/cli/slash_memory.go internal/cli/slash_test.go
# no output

rtk git diff --check
# clean
```

## Limits

These are scoped CLI package, lint, vet, and local-process tests, not a full repository `make verify` or CI run. The binary journey used a local loopback fake OpenAI-compatible server; no live third-party provider was exercised. No interactive PTY journey or Linux/Windows runtime was tested. The session recovery statement is the selected product boundary, not verification of another builder's resume implementation. The static help also records other frozen boundaries (including hard cost budgets, usage uncertainty, serve profile, remote TUI, image reading, themes/diff, and MCP reload); their independent runtime paths are not validated by this source slice. In particular this work does not claim R3 usage/cost accounting completion or integrated R2 independent review. Final product-truth review remains against the parent's integrated R2 base.

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
