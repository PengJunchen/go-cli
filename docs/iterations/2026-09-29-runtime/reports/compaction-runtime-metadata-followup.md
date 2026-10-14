# Legacy compaction converter runtime metadata follow-up

## Source and scope

- Base: `deefa15a3b8eed0d14f29f3d6b395aec27f1491a` (accepted durable core and CLI source).
- Source: `780b11b6d2d6d6f2d0559f605cc0ebcc78c4d4d5`.
- Change: the legacy CLI `AgentMessage` ↔ `TurnItem` converters now copy `Runtime` in both directions. The round-trip fixture covers a `SourcePriorPublished` reference, current-run transcript coordinates, and an `unknown` receipt outcome in both canonical and runtime metadata.
- No durable path, compaction strategy, or non-durable behavior changed. Messages with zero-value `Runtime` continue to convert as before.

## TDD and validation

- RED: `go test ./internal/cli -run '^TestTurnItemsToMessages_RoundTrip$' -count=1` failed before the converter fix; the source refs and runtime receipt outcome returned zero-valued.
- GREEN: the same focused test passed after copying `Runtime` in both converters.
- Passed: `go test -race ./internal/cli ./internal/compaction -count=1 -timeout=3m`.
- Passed: `go test ./... -run '^$' -count=0` (repository-wide compile-only).
- Passed: `go vet ./...`.
- Passed: `golangci-lint run ./...` (0 issues).
- Passed: `git diff --check`.

The repository-wide compile check selected no tests; a full repository test run was not performed.
