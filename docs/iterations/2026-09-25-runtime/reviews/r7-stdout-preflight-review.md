# R7 Stdout Tracing Preflight Review

Review date: 2026-09-25  
Review type: independent fixed-commit implementation review

## Reviewed snapshot

| Item | Snapshot |
|---|---|
| Commit | `e709cbb7ffad9b5813ddf138615e40d1da846c85` (`fix(cli): reject stdout tracing with structured prompt output`) |
| Base | `7ed603e0525a8942c4982b748e6c849034287d6e` |
| Worktree | `/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r7-stdout-preflight-review` |
| Worktree state | clean at review time |

## Decision

**Approved for this stdout-tracing preflight slice.** It rejects only enabled stdout tracing paired with a valid prompt JSON/stream mode and a message, before tracing runtime creation. This review is not approval of the full R7 lifecycle or other deferred R7 gates.

## Findings

No blocking findings.

- `run` invokes `stdoutTracingOutputConflict` after configuration loading but before `newTracing` (`cmd/cli/main.go:71-76`). The helper requires `Exporter == "stdout"`, treats nil `Enabled` as the existing enabled-by-default behavior, and returns no conflict for disabled tracing, `none`, or a nil config pointer (`cmd/cli/main.go:123-147`).
- Global parsing is shared: both `PromptArgsFromCLI` and `RunWithRegistry` use `newGlobalFlagSet` for the same `-version`/`-help` definitions. The helper observes global help/version precedence and the standard first-positional stop rule (`internal/cli/cli.go:90-117,128-140,171-189`).
- Prompt parsing is shared: `ParsePromptOutputMode` and the real prompt command both call `parsePromptArgs`, so the recognized flags, joined/separate values, error handling, and positional stopping behavior use one parser (`internal/cli/prompt.go:64-100`). The preflight ignores parse errors and missing-message cases; the actual command retains the existing `UsageError` path and error text.
- Only `OutputJSON` and `OutputStream` conflict, and only when at least one message argument remains. Text output, disabled/none tracing, a non-prompt command, and flags after the first positional message do not trigger the new rejection.
- For rejected cases, real CLI entrypoint tests assert exit code 2, empty stdout, zero HTTP requests, and no trace directory/file. The preserved-mode tests exercise an HTTP provider and confirm dispatch still occurs. Usage-error tests retain invalid-mode, prompt-help, and missing-message messages; global help/version still take precedence.

The helper's nil-config guard prevents a nil dereference in preflight. `run` still relies on its existing contract that a successful loader returns a non-nil `*config.Config` when it later calls `newTracing`; that assumption predates this change and the production `config.LoadTrusted` path returns a config or an error.

## Validation

The changed and adjacent targets were enumerated before running this focused race command; both packages passed:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 -run '^(TestRunRejectsStdoutTracingForStructuredPromptOutputBeforeRuntime|TestStdoutTracingOutputConflictIsTypedAndHonorsDefaultEnabled|TestRunStdoutTracingStructuredOutputPreflightPreservesOtherModes|TestRunStdoutTracingKeepsPromptUsageAndGlobalHelpBehavior|TestParsePromptOutputModeUsesPromptFlagSemantics|TestPromptArgsFromCLIUsesGlobalDispatchSemantics|TestRun_VersionFlag|TestRun_HelpFlag|TestRun_FlagParseError|TestPromptCmd_RunUnknownFlag|TestPromptCmd_RunMissingMessage|TestRunSuccessHelp|TestRunSuccessVersion)$' ./cmd/cli ./internal/cli
```

The stdout/structured-output incompatibility is now rejected before tracer construction; no claim is made here about unrelated stdout/log combinations or completion of broader R7 work.
