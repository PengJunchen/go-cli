# R0/R1 planned test-name mapping audit

Baseline: `5dcf538c5edbffd2304808fa3a6117e602a9088e`.

This audit covers the 15 R0/R1 planned test names that remained unmatched after `TestEventStreamDropVisibleToRunOwner` landed in the root branch. It updates a planned name only where an existing test proves the same acceptance behavior. Task status fields remain unchanged. Eleven planned names map to equivalent tests; three more now have exact acceptance tests. The remaining legacy SSE case is excluded by the current transport boundary below.

## Equivalent mappings

| Planned acceptance | Existing test name(s) | Evidence |
| --- | --- | --- |
| `TestApprovalAllowOnlyAuthorizesCurrentCall` | `TestMiddlewareCallbackAllowOnlyAppliesToCurrentCall` | `internal/approval/middleware_test.go:241` sends two identical calls, requires two callback/store lookups, and proves callback `Allow` is not cached. |
| `TestWriteMissingContentPreservesExistingFile` | `TestWriteMissingContentArg`; `TestMutationQueueRejectsMalformedPayloadWithoutChangingFile` | `internal/tools/write_test.go:197` requires an error and unchanged bytes; `internal/tools/mutation_middleware_test.go:179` repeats missing-content and wrong-type cases through the mutation queue and verifies the original file remains byte-for-byte intact. |
| `TestEditMissingNewStringPreservesMatchedText` | `TestEditMissingNewStringDoesNotChangeFile`; `TestMutationQueueRejectsMalformedPayloadWithoutChangingFile` | `internal/tools/edit_test.go:163` requires an error and unchanged bytes; the mutation-queue test also covers missing `new_string` through the queued path. |
| `TestExplicitEmptyContentClearsFile` | `TestWriteEmptyContent`; `TestMutationQueueAllowsExplicitEmptyStrings` | `internal/tools/write_test.go:85` proves an explicit empty string creates a zero-byte file; `internal/tools/mutation_middleware_test.go:243` preserves explicit empty write/edit values through the queue. |
| `TestServeNoAuthWildcardRejected` | `TestServeCmdNonLoopbackNoAuthRejected` | `internal/cli/serve_cmd_tls_test.go:22` runs the actual command with `--no-auth` against `:0`, IPv4 wildcard, and IPv6 wildcard addresses and requires rejection. |
| `TestMCPCallUnknownDoesNotReplay` | `TestMCPCallToolLostResponseIsUnknownAndNotRetried`; `TestMCPToolCallLostResponseIsUnknownAndNotRetried` | `internal/mcp/mcp_reconnect_test.go:220` covers stdio reconnect; `internal/mcp/http_adapter_test.go:682` covers HTTP POST. Both simulate remote execution followed by a lost response and verify an unknown outcome without replay. Both names are retained because the contract requires both transport paths. |
| `TestPlanModeBlocksUnknownEffectTool` | `TestPlanModeToolWrapper_FailsClosedForUnknownAndExternalTools` | `internal/core/plan_mode_middleware_test.go:61` exercises the R0 static name-based wrapper, denies unknown/external tools, and asserts zero executor calls. This is the R0 stopgap acceptance only. |
| `TestGateIgnoresExternalNoColorForANSIContractOnly` | `TestGateIgnoresExternalNoColorForANSIContractOnly` | `internal/tui/color_profile_test.go:140` starts a child test with external `NO_COLOR=1` and `TERM=dumb`; the child verifies TUI `TestMain` clears/normalizes them and ANSI rendering remains enabled. `TestNoColorSuppressesANSI` remains separate product-behavior coverage. |
| `TestCancelDoesNotMarkRunQuiescentBeforeToolExit` | `TestRunOwnerCancellationKeepsClaimUntilWorkerStops` | `internal/core/run_owner_test.go:405` registers a blocked worker, observes `cleanup_pending` after bounded cancellation, and proves the claim is retained until that worker exits. |
| `TestUnknownOutcomeRetainsMutationSlot` | `TestAssemblySharesRunSlotGuardAcrossHarnessAndTurnRunner` | `internal/cli/run_ownership_integration_test.go:252` uses actual `AssembleAgent` wiring in sequential and parallel cases; an unknown pending mutation blocks the opposite run entry point until worker release, then the claim becomes reusable. |
| `TestHarnessSpanEndsAfterRun` | `TestHarnessSpanEndsAfterRunAndIncludesOutcomeAttributes` | `internal/core/run_owner_test.go:664` keeps the span open while the run is active, ends it on observed completion, and checks outcome, execution state, and run ID attributes. |

## Later exact acceptance tests

| Planned acceptance | Current test and scope |
| --- | --- |
| `TestPlanModeBlocksKnownMutatingBuiltins` | `internal/core/plan_mode_r0_acceptance_test.go:25` runs registered mutation tools through the actual Plan Mode guard. This proves the R0 name-based stopgap, not the R4 effect policy. |
| `TestPlanModePreservesApprovedReadOnlyTools` | `internal/core/plan_mode_r0_acceptance_test.go:58` runs the approved registered read and control tools through that same guard. |
| `TestNonCooperativeWorkerQuarantinedAfterDeadline` | `internal/cli/run_ownership_integration_test.go:397` exercises a non-cooperative Assembly worker and its retained run claim after the cleanup deadline. |

`TestMCPRejectsCrossOriginEndpointWithCredentials` remains unregistered. The historical foreign absolute SSE endpoint flow is not implemented by the current Streamable HTTP adapter; [the reachability review](../evidence/r0-mcp-foreign-sse-endpoint-reachability.md) records the source proof and the applicable `TestHTTPAdapterRejectsCrossOriginRedirectBeforeCredentials` regression. Do not count an absent test name or an inapplicable flow as a passing test.

The R0 contract keeps its conservative name-based Plan Mode guard separate from the general effect/capability model assigned to R4. No R4 prepared-policy result is counted as R0 Plan Mode acceptance.

## Verification

The added ANSI-isolation test was verified with targeted `go test -list` and passed through `verify-test-gate` while the gate inherited `NO_COLOR=1 TERM=dumb`. The TUI package also passed `go test -race -count=1` and `go vet`. Later exact Plan Mode and worker tests were independently run under race before integration; their names were checked in the current source. Full final-branch verification is tracked separately.
