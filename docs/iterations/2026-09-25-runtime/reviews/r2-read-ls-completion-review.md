# R2 Read/Ls completion proof review

Reviewed source commit `05f20710a13fe6473778f1407e516de9cc97d452` in a detached worktree based on `5d6fcdd6e9ee9345180cd07fa089f6ff3fc010f6`. The reviewed change is limited to first-party Read/Ls registration provenance and prepared completion state in `internal/tools`. This review does not assess the `runOwner` consumer, session persistence, or full R2/R4 closure.

## Finding

**The bounded registration/state slice is accepted.** `registerDefaultTool` only tries the private `trustedInlineLocalReaderRegistrar` for concrete `*ReadTool` and `*LSTool` definitions (`internal/tools/builtins.go:205-220`). The default registry uses an exact non-nil type switch before minting `RegistrationSourceBuiltin` and `CompletionInlinePipelineJoined`; the effect set remains empty, which resolves to `EffectUnknown` (`internal/tools/trusted_registry.go:254-268`). The private registrar is forwarded by the known middleware/deferred adapters. An ordinary same-name registration replaces the stored definition and registration metadata instead of inheriting the old completion proof (`internal/tools/trusted_registry.go:308-369`). An unrecognized registrar path falls back to ordinary registration and mints no proof.

The implementation matches the intended narrow evidence boundary. `ReadTool.Execute` resolves and checks the path, then performs directory listing or file reading inline; opened files are closed before return, and special files are rejected (`internal/tools/read.go:92-207`). `LSTool.Execute` walks, sorts, and formats entries inline (`internal/tools/ls.go:83-217`). Neither implementation starts a goroutine or external process. The proof means that this exact local executor returned; it does not grant an effect or establish authorization. The maintained tests assert `EffectUnknown` and exercise success and error returns for both tools (`internal/tools/prepared_completion_test.go:66-109`).

The tests also cover proof lifetime at the prepared registry boundary: an explicitly attested blocking wrapper keeps `Done` open until it returns; an unattested wrapper suppresses the proof; and an ordinary replacement does not inherit proof (`internal/tools/prepared_completion_test.go:111-205`). These are appropriate boundaries for this registration/state slice. They do not test the production CLI wrapper chain.

## Default CLI integration remains open

The default assembly installs three execution wrappers but leaves `SynchronousExecutionWrappers` at its zero value (`internal/cli/assemble.go:1244-1257`). The prepared registry clears completion proof when wrappers are present without this attestation (`internal/tools/prepared_registry.go:50-53`). Consequently, default CLI Read/Ls calls still have no positive completion signal through this assembly, and no test in this commit proves an assembled invocation reaches a completion consumer. The new capability must not be described as closing default R2/R4 behavior.

Source review found no asynchronous queue in the fixed assembly implementations:

- `PromptInjectionToolWrapper` calls the next stage, then runs `PromptInjectionGuard.Check` synchronously over the result and returns (`internal/production/prompt_injection_guard.go:155-179`, `:96-113`).
- Assembly constructs `newProductionToolWrapper` with `cache == nil`, the concrete default telemetry instance, and either no audit logger or the concrete default audit logger (`internal/cli/assemble.go:1221-1257`). The wrapper calls `next`, then calls `recordAudit` and `recordToolTelemetry` before returning (`internal/cli/assemble.go:2491-2535`, `:2561-2615`). `DefaultAuditLog.Log` serializes and appends under its mutex before returning (`internal/production/audit.go:118-166`); `DefaultTelemetry.Record` updates its in-memory map under a mutex and returns (`internal/production/telemetry.go:91-139`).
- `ResultMaskingWrapper` masks the returned string in memory and returns synchronously (`internal/tools/result_masking.go:119-132`).

The current comment says execution wrappers “add no side effects” (`internal/tools/trusted_registry.go:103-117`). Root clarified that for this attestation, this means no new tool-visible file/process/network execution effects; the fixed assembly's synchronous, configured audit log and in-memory telemetry are allowed. This exception is limited to the three audited default components above, with cache absent and audit/telemetry held to their concrete default implementations. It does not authorize arbitrary wrappers with synchronous side effects. A bounded follow-up should encode this narrow contract in the comment and attest only this exact hard-coded assembly chain, then add assembly-level Read/Ls success/error tests through the actual completion consumer. Keep unknown/custom stages unattested by default. This decision does not mean the default CLI is already integrated: until that follow-up lands, default CLI Read/Ls calls still lack positive completion proof.

## Validation

- Behavioral RED was checked in the review worktree by temporarily suppressing the completion proof in the private reader registrar. The exact targeted tests compiled and failed assertions for the missing inline proof / closed completion signal. The source was restored afterward; this was not a compile-failure proxy.
- Reviewer reran the focused race target successfully:

  ```text
  rtk env GOMAXPROCS=2 NO_COLOR=1 TERM=dumb go test -race -p 2 -count=1 -run '^(TestPreparedCompletionRegisteredReadAndLSProvideInlineProof|TestPreparedCompletionRegisteredReadAndLSWaitForAttestedWrappers|TestPreparedCompletionRegisteredReadAndLSSuppressUnattestedWrapperProof|TestPreparedCompletionOrdinaryReadAndLSReplacementHaveNoProof|TestRegisterDefaultsMintsOnlyReviewedWriteEffects)$' ./internal/tools
  ok github.com/pengjunchen/go-cli/internal/tools 1.083s
  ```

- The implementer reported a full `./internal/tools` race pass for this fixed commit; the reviewer did not rerun that broader suite because the targeted race and source checks passed.
- The detached review worktree remained clean at the reviewed commit. No source files were modified and this report is intentionally unstaged.
