# Tagged E2E fixture repair report

Date: 2026-09-30
Worktree: `go-cli-repair-20260930-tagged-e2e-fixtures`
Branch: `codex/fix-tagged-e2e-phase21-phase22-fixtures-20260930`
Base: `a475d0e6ddde8600f107e048b4b8f2173dc38d21`
Input failure log: `/tmp/go-cli-tagged-e2e-bda13f2.jsonl`

## Changes

The Phase 22 shared provider fixtures used `http://127.0.0.1:0`. The current endpoint resolver correctly rejects port zero; each assembly test stopped before exercising the component under test. Both `phase22ProdTestConfig` and `phase33TestConfig` now use the valid loopback-only endpoint `http://127.0.0.1:12345/v1`. These tests do not issue provider requests, so this changes fixture validity without adding a network dependency or weakening endpoint validation.

The Phase 21 `moduleRoot` helper derived a filesystem path from `runtime.Caller`. With Go's `-trimpath`, the reported filename becomes the module import path, so the test tried to open `github.com/pengjunchen/go-cli/internal/cli/interactive.go`. The helper now walks upward from the test process working directory until it finds `go.mod`; the test still reads the repository's real `interactive.go` and checks that it does not contain `WithWidth(80)`.

## Verification

Before the changes, the Phase 22 Production tests reproduced the shared endpoint rejection. The Phase 21 test passed without `-trimpath` and failed with it, attempting to open the import path above. The input JSONL also shows the AllCapabilities tests failing at `phase33TestConfig` with the same port-zero error.

After the changes, this focused command passed all 18 selected tests, including Phase 21 under `-trimpath`, the Phase 22 Production tests except its tracing-enabled case, and AllCapabilities checks that had been blocked by the invalid endpoint:

```text
rtk proxy go test -json -count=1 -trimpath -tags=e2e ./tests/e2e_20260802 -run '^(TestET_Phase21_TUI_AC9_NoHardcodedWithWidth80|TestET_Phase22_Production_(CircuitBreaker|LoopDetector|IdempotentCache|AuditLog|Telemetry|ReminderManager|FailureSynthesizer|HookChain|TracingDisabled|CompactionMaxTokens|CompactionDefaultMaxTokens)|TestET_Phase22_AllCapabilities_(AC3_GitToolsFunctional|AC4_ProductionComponentsNotNil|AC5_MiddlewareComponentsNotNil|AC6_Phase20ComponentsNotNil|AC7_Phase19ComponentsNotNil|AC10_FullAgentTurnWithMockLLM))$'
```

A broader run of all `TestET_Phase22_.*` tests plus the Phase 21 path test returned 18 pass, 3 fail, and 2 skip. The remaining failures are separate from the endpoint/path repair:

- `TestET_Phase22_Production_TracingEnabled` and `TestET_Phase22_AllCapabilities_AC8_ConfigDrivenTracing` call standalone `AssembleAgent` and expect it to create an invocation-owned tracer. The assembly contract explicitly leaves tracing ownership to the CLI invocation; `internal/cli/assemble_tracing_test.go` verifies that standalone assembly has a nil tracer. The assertion was not weakened. A separate follow-up should exercise the real invocation tracing path and its output.
- `TestET_Phase22_AllCapabilities_AC1_ToolRegistryCompleteness` expects `plan_mode_enter` and `plan_mode_exit`, while the assembled registry contains `enter_plan_mode` and `exit_plan_mode`. This is an independent test/spec name mismatch.

The tagged E2E package and the full failure set were not claimed green by this scoped repair.
