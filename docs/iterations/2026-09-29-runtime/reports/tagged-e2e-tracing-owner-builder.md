# Tagged E2E tracing ownership follow-up

Date: 2026-09-30
Worktree: `go-cli-repair-20260930-tagged-e2e-fixtures`
Branch: `codex/fix-tagged-e2e-phase21-phase22-fixtures-20260930`
Base commit: `881a18cf80375dd3147f8438a0f01aceb43b18d3`

## Changes

The Phase 22 tracing tests incorrectly expected standalone `AssembleAgent` to create an invocation tracer. The assembly contract leaves tracer ownership to the CLI invocation, so the production assembly test now asserts that standalone assembly has no tracer. The config-driven tracing test now builds and runs the real `cmd/cli` binary with an isolated HOME, config, cache, temp directory, and working directory. Its user config enables the JSONL exporter; the test invokes `go-cli version`, reads the flushed JSONL file, validates each row as a `tracing.SpanData`, and asserts the invocation emits both `cli.invocation` and `command.dispatch` spans with one trace ID.

The real command does not contact a provider. Its valid loopback provider URL is only configuration input, while the version command exercises the normal user-facing CLI startup and tracing finalization path.

## TDD and verification

Before the follow-up, the broader Phase 21/22 selector reproduced two tracing failures: the standalone assembly test expected a non-nil tracer, and the AllCapabilities tracing test attempted to read a trace file from a standalone assembly that does not own one. After the changes, the focused Phase 21/22 selector passed 20 tests, including the real-binary tracing test. The broader selector passed 20, retained the independent Phase22 AC1 tool-name mismatch, and reported two existing skips. AC1 remains separate: its fixture expects `plan_mode_enter`/`plan_mode_exit`, while the current registry exposes `enter_plan_mode`/`exit_plan_mode`.

The main tracing assertion is an actual subprocess of the built CLI binary and actual exporter output, not an in-process `AssembleAgent` tracer assertion. Full tagged E2E suite status is not claimed by this scoped follow-up.
