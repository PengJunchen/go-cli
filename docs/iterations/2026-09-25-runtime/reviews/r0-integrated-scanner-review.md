# R0 integrated scanner review

Reviewed the frozen two-commit chain ending at `65e232496c1dfa93121ebf850db09e8ed9d2327b` in `/Users/pengjunchen/.codex/worktrees/r0-integrated-scanner-fix`:

- `f45cbf64b4cdfffdaebddfcca1f377c7d16dc50e` replaces the five scanner-reported ACP HTTP credential/query canaries with runtime-generated secrets.
- `65e232496c1dfa93121ebf850db09e8ed9d2327b` narrowly exempts direct message literals passed to the standard library `errors.New` or `fmt.Errorf` from SCAN-009.

The target worktree was clean at the fixed hash. No product source was changed during review.

## Review result

Accepted. The canary change removes the five known SCAN-003 findings without deleting or weakening the surrounding auth, redirect, request-count, error-chain, or redaction assertions. The scanner change removes the single error-message false positive while preserving true positives for prompt literals outside recognized standard error-message positions.

The baseline report `docs/iterations/2026-09-25-runtime/evidence/integration-main-575fc5f-scan-report.json` identified exactly these five canary errors:

1. `internal/acp/grpc_adapter_http_contract_test.go:66` — authenticated loopback token.
2. `internal/acp/grpc_adapter_http_contract_test.go:110` — wrong bearer token.
3. `internal/acp/grpc_adapter_http_contract_test.go:198` — redirect bearer token.
4. `internal/acp/grpc_adapter_lifecycle_test.go:452` — private bearer value.
5. `internal/acp/grpc_adapter_lifecycle_test.go:453` — private endpoint query value.

The same baseline report had one unrelated SCAN-009 false positive at `internal/compaction/compactor.go:113`: `ErrMandatorySystemPromptOverBudget` is a human-readable error created with standard `errors.New`, not a prompt or default value. The new AST helper recognizes only a direct first string argument to standard-library `errors.New` or `fmt.Errorf`, using the resolved imported identifier to reject local shadowing. It does not suppress other string arguments or arbitrary packages that happen to use the same name.

The new tests cover standard and aliased imports; prompt constants; a prompt string passed as a non-format argument to `fmt.Errorf`; local variables shadowing both imported package names; and a non-standard package imported under the name `errors`. The shadow, same-name non-standard import, and non-format-argument cases remain positive findings. Thus the exemption does not create a real-prompt blind spot in the tested cases.

## Validation

The focused ACP tests passed under the race detector:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 -run '^(TestGRPCAdapterAuthenticatedHTTPProfileLoopback|TestGRPCAdapterRejectsWrongHTTPBearerTokenWithoutLeaking|TestGRPCAdapterHTTPBearerTokenIsNotForwardedAcrossRedirect|TestGRPCAdapterConnectErrorPreservesCauseWithoutLeakingEndpointOrToken)$' ./internal/acp
ok github.com/pengjunchen/go-cli/internal/acp 6.416s
```

The SCAN-009 exemption and positive-prompt regression matrix passed under race:

```text
rtk env GOMAXPROCS=2 go test -race -p 2 -count=1 -run '^TestScanHardcodedDefaults' ./internal/verify
ok github.com/pengjunchen/go-cli/internal/verify 1.028s
```

The integrated scanner gate completed successfully:

```text
rtk make scan
Files scanned: 366
Findings: 1510 (ERROR: 0, WARN: 1510, INFO: 0)
```

No Linux runtime claim is made; these commands ran on the available Darwin host.
