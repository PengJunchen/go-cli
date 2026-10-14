# Legacy compaction converter runtime metadata follow-up review

**Base:** `deefa15a3b8eed0d14f29f3d6b395aec27f1491a` (accepted durable core and CLI source).

**Source:** `780b11b6d2d6d6f2d0559f605cc0ebcc78c4d4d5`.

**Reviewed HEAD/report:** `2d4d3f12432e23d3c2050e5b1411202d61985b00`.

**Verdict: ACCEPT.** Both legacy CLI conversions now preserve the runtime metadata value. The follow-up adds no durable routing, sink call, or behavioral branch, so it does not create a route around the bound durable compaction flow.

`messagesToTurnItems` copies `AgentMessage.Runtime` into `TurnItem.Runtime` (`internal/cli/interactive.go:104-127`), and `turnItemsToMessages` copies it back (`:132-156`). The round-trip fixture now includes a `SourcePriorPublished` source, current-run transcript sources, and an `unknown` receipt represented in both canonical and runtime fields (`internal/cli/field_mapping_test.go:64-110`). Its full `AgentMessage` equality assertion verifies both source references and receipt outcome survive the two conversions. The pre-fix source at the stated base omits both assignments; the follow-up report records the focused test failing with zero-valued metadata before the change and passing after it.

The source diff is limited to those two field assignments and the round-trip fixture. Zero-value metadata remains zero, preserving existing non-durable behavior. The durable prompt and REPL continue to hydrate from the exact sink returned by `bindRun` before executing with its bound context (`internal/cli/prompt.go:250-265`, `internal/cli/repl_session.go:892-914`). Bound Agents still skip the legacy hook, direct `Agent.Compact` still returns `ErrDurableCompactionUnsupported`, and durable slash dispatch still rejects `/compact` before handler execution. No durable source branch or projection path changed in this follow-up.

## Validation

- RED evidence: the follow-up report records `go test ./internal/cli -run '^TestTurnItemsToMessages_RoundTrip$' -count=1` failing before the source change. The pre-fix converter at the stated base confirms both `Runtime` assignments were absent.
- GREEN passed: `rtk go test ./internal/cli -run '^TestTurnItemsToMessages_RoundTrip$' -count=1` (1 test).
- Passed race checks: `rtk go test -race ./internal/cli ./internal/compaction -count=1 -timeout=3m` (916 tests across 2 packages).
- Passed repository compile-only: `rtk go test ./... -run '^$' -count=0` (exit 0; no tests selected).
- Passed repository vet: `rtk go vet ./...`.
- Passed repository lint: `rtk golangci-lint run ./...` (no issues).
- Passed `rtk git diff --check deefa15..HEAD`.

This review covers only runtime metadata preservation in the legacy converters. It does not close the configured summary-model, attempt-accounting, or UI integration work, and no full repository test or end-to-end suite was run.
