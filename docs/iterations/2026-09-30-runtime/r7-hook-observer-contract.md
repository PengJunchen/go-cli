# R7 Hook observer implementation contract

Status: designer proposal for independent review; not implementation or phase acceptance. Source baseline: `bb2fc1f6b4a3ae828aec29f2af5e0a938a2517b7`. This narrows the accepted builder design in [R7 remaining design](../2026-09-29-runtime/r7-remaining-design.md#L87) to the concrete Hook bridge gap recorded in the [R7 acceptance audit](r7-acceptance-audit.md). It does not change the protected task ledger or product source.

## Goal and non-goals

An extension Hook must receive a stable owner identity and an isolated, extension-owned snapshot of run data. Its event data must not expose mutable `core` objects, runtime handles, raw Go errors, or an unbounded extension-provided failure reason. The existing supported path is local Go plugins through `DefaultPluginLoader`; HTTP descriptors are unavailable because their `rpcExtension` has no remote call transport. This contract adds no remote protocol.

The Hook is installed executable code and is an explicit recipient of the fields in its Hook DTO. Submission text, history text, and the final assistant message remain present where the accepted snapshot specifies them. Do not describe this extension observer boundary as trace redaction: configured extensions can inspect these fields. The privacy guarantee here is that extensions receive only documented copied values, not live runtime objects or unbounded raw error diagnostics.

## Decisions

### Registration ownership

The bridge derives `HookEvent.Source` from the name of the successfully initialized extension that registered the Hook. The Hook cannot provide or override this owner. Capture the validated owner name once for that initialization and preserve it with the Hook through the existing private staging registry, atomic commit, registry snapshot, PluginManager view, and Assembly wiring.

Add an extension-owned value `HookRegistration{Owner string, Hook Hook}` and an ordered `HookRegistrations` accessor. Keep `ExtensionRegistry.RegisterHook(ctx, hook)` unchanged: the coordinator or an owner-bound staging adapter supplies the owner internally. Preserve the current staged-init rules: failed/canceled init publishes no registrations, conflict cleanup remains joined, and shutdown still joins the same lifecycle owner. `PluginManager.Hooks()` may remain as a compatibility projection, but production Assembly must consume `HookRegistrations()` so owner identity is not lost.

`HookEvent.Source` is the exact registration owner; `HookEvent.Name` remains `agent.before_run` or `agent.after_run`. A Hook's `Name()` is diagnostic identity only and never an owner source. Duplicate Hook-name behavior and registration order remain unchanged.

### Event DTOs and isolation

Keep the outer `HookEvent` fields for compatibility, but set `Data` to one of two exported extension-owned concrete value types, selected by event name:

- `HookBeforeRunSnapshot{Submission HookSubmissionSnapshot}`
- `HookAfterRunSnapshot{Submission HookSubmissionSnapshot, Run HookRunSnapshot, Error HookErrorSnapshot}`

The concrete field contract is:

```go
type HookSubmissionSnapshot struct {
    Type HookSubmissionType // defined by extension as a string type
    Content string
    Metadata map[string]any // validated JSON-compatible values only
    Messages []HookMessageSnapshot
}
type HookMessageSnapshot struct {
    Role, Content string
    ContentBlocks []HookContentBlockSnapshot
    Usage *HookUsageSnapshot
    ToolCalls []HookToolCallSnapshot
    ToolCallID, ToolName, InputKind, ReceiptOutcome string
}
type HookContentBlockSnapshot struct {
    Type, Text string
    ImageURL *HookImageURLSnapshot
}
type HookImageURLSnapshot struct { URL, Detail string }
type HookUsageSnapshot struct { InputTokens, OutputTokens, TotalTokens int }
type HookToolCallSnapshot struct { ID, Name string; Args any }
type HookBeforeRunSnapshot struct { Submission HookSubmissionSnapshot }
type HookAfterRunSnapshot struct {
    Submission HookSubmissionSnapshot
    Run HookRunSnapshot
    Error HookErrorSnapshot
}
type HookRunSnapshot struct {
    Message string
    Success bool
    RunID string
    RunIDAvailable bool
    Outcome HookRunOutcome // defined by extension as a string type
    OutcomeAvailable bool
    ExecutionState HookRunExecutionState // defined by extension as a string type
    ExecutionStateAvailable bool
}
type HookErrorSnapshot struct { Present bool; Class HookErrorClass }
```

The extension package also defines distinct string types `HookSubmissionType`, `HookRunOutcome`, `HookRunExecutionState`, and `HookErrorClass`; do not alias core enum types. The event-specific `Data` is exactly `HookBeforeRunSnapshot` or `HookAfterRunSnapshot`. Do not alias `llm` structs or use `core` types in any of these declarations. Each message snapshot covers the fields in the accepted design: role/content/content blocks/usage/tool calls/tool-call ID/tool name/input kind/receipt outcome. Runtime-only fields such as `AgentMessage.Runtime`, `Result.StatusHandle`, durable/session managers, `core.Submission`, `core.Result`, and raw `error` values never appear in a DTO. Unsupported dynamic payloads produce `ErrHookPayloadUnsupported` without exposing their `String`, `Error`, type-specific formatting, value, or key text.

All DTO leaf/container types are declared by `extension`; do not alias `llm` structs. At minimum define `HookContentBlockSnapshot{Type, Text, ImageURL *HookImageURLSnapshot}`, `HookImageURLSnapshot{URL, Detail}`, `HookUsageSnapshot{InputTokens, OutputTokens, TotalTokens}`, and `HookToolCallSnapshot{ID, Name, Args any}`. `HookMessageSnapshot` uses strings for role/IDs/name/input-kind/receipt-outcome, extension-owned block and tool-call slices, and an optional extension-owned usage value. `HookToolCallSnapshot.Args` and submission metadata may use `any` only after recursive conversion to JSON-compatible nil/bool/string/finite-number/`map[string]any`/`[]any` values. Recursively accept only nil, booleans, strings, finite numeric primitives, string-keyed maps, and slices/arrays; normalize containers to fresh `map[string]any` and `[]any`. Reject cycles, pointers/handles, errors, non-string map keys, structs, NaN/Inf, and all unsupported dynamic types before invoking the observer. Image URL fields are copied as DTO scalars, not retained as `llm` pointers. Preserve nil versus empty slices/maps where the accepted public shape requires it; a copied map or slice must not share storage with the core value or another observer.

Bound each `Handle` event DTO as a whole with one budget shared by metadata, tool-call args, message/history content, content blocks, image URLs, all other DTO strings, and map keys: nesting depth at most 64; at most 65,536 visited values/container entries; at most 33,554,432 aggregate bytes across strings and keys. Count each scalar/container toward the value limit, each string key and DTO text toward the byte limit, and root DTO fields toward depth/node accounting. Return fixed-message `ErrHookPayloadLimitExceeded` (mapped to the same `unsupported_payload` class) when any limit is exceeded; do not truncate or invoke the affected observer. Exact-limit values are accepted and one-over fixtures are rejected. Validate and make the canonical snapshot once, then validate each per-observer defensive copy against the same maxima; reset the counters for each observer event so registration order does not change acceptance.

`HookRunSnapshot` contains only the accepted scalar outcome fields: message, success, run ID, outcome, and execution state. Core's current `hookLoop` and `canonicalHookLoop` synthesize `Result` with only `Message` and `Success`; do not infer final outcome or execution state from those fields or from `runErr`. Populate `RunID` from `core.RunIDFromContext(ctx)` only when non-empty. Set each `*Available` flag exactly when its scalar is present; unavailable strings are empty. Copy `Outcome` or `ExecutionState` only when the `Result` field itself is non-empty, and document those values as unavailable through the current wrappers. This keeps the accepted fields without fabricating values or requiring a core interface change. Do not copy `RunStatusHandle`, callbacks, resource owners, or durable publication handles. Message text is intentionally available under the explicit extension observer contract above.

`HookErrorSnapshot` is value-only: `Present` plus a stable class from an explicit allowlist. Classify known sentinels with `errors.Is` and known typed error families with `errors.As`; all other non-nil errors map to `unknown`. Never call or copy `err.Error()`. Never expose the error chain, wrapped cause, provider body, filesystem path, URL, or request payload.

Freeze the initial class allowlist in code and tests as `canceled`, `deadline`, `hook_blocked`, `unsupported_action`, `unsupported_payload`, and `unknown`; `Present=false` has an empty class. Any later class addition requires review because observers can branch on it and it becomes part of the public DTO contract. This list is classification only; it never carries an error message.

DTO maps, slices, content blocks, tool calls, and args are defensive deep copies. Build a canonical copied value first, then give every Hook its own recursive copy; a Hook may mutate its copy, but that mutation cannot affect the runtime, later Hooks, or another event. Copy/validation failure occurs before that Hook is invoked. For BeforeRun, snapshot failure is fail-closed and prevents the run. For AfterRun, it is an observer error; the chain continues to later observers and the run result/error is preserved.

### Supported actions and failure semantics

Export `HookActionBlock` alongside the existing `HookActionPass`; these are the only supported actions. Retain the `HookResult` shape for source compatibility, but this bridge never applies `Replacement`.

- `Pass` with no replacement continues.
- `Block` with no replacement stops a BeforeRun chain using `ErrHookBlocked`.
- A Block during AfterRun is an observer failure only. Core invokes the remaining AfterRun observers and logs the first observer error; an already completed `core.Result` and the underlying run error are not rewritten or rolled back.
- Any unknown action, `terminate`, or `replace`, and either supported action paired with non-nil `Replacement`, returns `ErrUnsupportedHookAction`. BeforeRun fails closed; AfterRun reports an observer failure as above.
- `HookResult.Reason` is extension-controlled diagnostic text. The bridge neither returns it in an error nor logs it. `Error()` strings for bridge failures are fixed, short messages backed by `errors.Is` sentinels. Owner, Hook name, event phase, and stable failure class may be fields of a typed error for local inspection, but are not interpolated with arbitrary reason, prompt, error, or payload text.

This matches existing `core.HookChain` semantics: BeforeRun errors halt; AfterRun calls all hooks and returns the first observer error, while the core run loop logs that observer error without replacing the run result/error (`internal/core/hook.go:58-109,238-254`).

## Minimum implementation surface

| File | Required change |
| --- | --- |
| `internal/extension/types.go` | Export supported `HookActionBlock`, DTO value types, and stable `ErrHookPayloadUnsupported`, `ErrHookPayloadLimitExceeded`, `ErrUnsupportedHookAction`, and `ErrHookBlocked` sentinels. Keep `HookEvent` and `HookResult` outer compatibility. |
| `internal/extension/registry.go` | Store ordered owner-bearing Hook registrations. Bind owner only in manager-created staging; preserve it through the existing atomic commit. Keep the public `RegisterHook` signature. |
| `internal/extension/manager.go`, `plugin_manager.go` | Use the manager's captured extension name as staging owner; expose ordered `HookRegistrations()` from the committed manager view. Keep the old Hooks accessor only as a projection if compatibility requires it. |
| `internal/cli/extension_bridge.go` | Accept a registration, build copied DTOs, classify errors without calling `Error()`, enforce the Pass/Block table, and return typed bounded bridge errors. |
| `internal/cli/assemble.go` | Wire registrations, rather than ownerless Hooks, into the existing HookChain. No core Hook interface change is needed. |
| `internal/extension/*_test.go`, `internal/cli/extension_bridge_test.go` | Add staging/owner, clone/failure/action unit tests and assembled-path tests below. |
| `internal/extension/plugin.go`, `plugin_stub.go`, `plugin_manager.go` | Reject HTTP(S) in the active loader, keep `ErrRemoteExtensionUnsupported` visible with `no_plugin`, preserve the stub's explicitly different disabled-loader error, and redact remote path logging/wrapping. Rewrite old HTTP-loader assertions rather than allowing an unavailable descriptor test contract to remain in the suite. |
| `tests/phase48_security_e2e_test.go`, plugin loader tests | Update integration claims about the removed HTTP transport; keep URL security tests on the shared HTTP client/tools boundary where that behavior still exists. |

Keep this slice out of `core`: the CLI bridge is the adapter that already depends on both `core` and `extension`, while `extension` must remain independent of `core`.

## TDD fixtures and acceptance tests

Write the tests before the bridge and storage changes. Unit tests must assert behavior through the exposed value graph, not merely compare names or test helper internals.

1. `TestHookRegistrationOwnerSurvivesStageCommit`: two extension instances register Hooks; successful registrations retain their own exact owner and deterministic order. A failed/canceled init and a conflicting stage publish no Hook or owner. The Hook cannot spoof `Source`.
2. `TestHookSnapshotDeepCopyIsolatedPerObserver`: use metadata, history, content blocks/image URL, usage, and nested tool-call args; mutate the first observer's values and verify the original submission and second observer remain unchanged.
3. `TestHookSnapshotRejectsUnsupportedPayload`: use a cycle, non-JSON type, pointer/handle, and an `error` nested in metadata. Assert `errors.Is(err, ErrHookPayloadUnsupported)`, that the rejecting Hook was not called, and that formatted errors/logs contain none of the input canaries.
4. `TestHookSnapshotEnforcesPayloadLimits`: assert exact-limit depth/node/aggregate string-byte payloads pass, and each one-over case returns `ErrHookPayloadLimitExceeded`, invokes no observer, and reveals none of its unique string canaries.
5. `TestHookErrorSnapshotOmitsRawError`: pass an error chain containing a unique provider-secret canary; the Hook sees only `Present` and a stable class. Assert neither returned observer error nor captured log includes the canary.
6. `TestHookActionPublicAndBounded`: compile an external-package fixture that constructs `extension.HookActionBlock`; assert BeforeRun blocks via `errors.Is(ErrHookBlocked)`, Block's unique Reason does not appear in error/log output, unknown/terminate/replace and non-nil Replacement fail via `ErrUnsupportedHookAction`, and AfterRun observer failure does not change the original result/error or skip following observers.

The full configured journey uses the supported built-in Go shared-object loader, not a fake `PluginLoader`:


7. Add a test-only plugin under `internal/extension/testdata/r7hookplugin` with the supported `New() ([]extension.Extension, error)` factory. Its extension registers a named Hook during Init; the Hook writes a bounded test report to a per-test temporary file and intentionally mutates its received DTO; Shutdown writes a final marker. Put the assembly integration test in `internal/cli` so plugin-shared dependencies such as `extension` are not coverage-instrumented as the plugin is built. Build the fixture in the module with the same Go toolchain, environment, build tags, and flags as the test binary; specifically compile it with `-race` when the test binary uses `-race`, and pass the same non-default build tags/GOFLAGS to both commands. Configure its `.so` path through real `Config.Extensions.PluginPaths` and set `Config.Extensions.Enabled=true`, assemble the normal CLI runtime, execute one deterministic run, and close the returned Assembly. Assert the order `Load → Init/register → before_run → after_run → Shutdown`, the expected extension-derived `Source`, no mutation of the model's/original submission state, no raw error/reason canary in the HookErrorSnapshot or logs, and exactly one Shutdown. Use a deterministic mock model/provider to avoid network access.

Run this integration fixture on a CI host where the Go `plugin` package and `-buildmode=plugin` are supported, with cgo enabled. Provide an explicit supported Linux CI gate that runs `go test -race -count=1 -run '^TestConfiguredGoPluginLifecycle$' ./internal/cli` and fails if this test is skipped; the required gate can set `GOCLI_REQUIRE_NATIVE_PLUGIN_TEST=1`, which converts unsupported-platform/build-mode skip conditions to failures. For developer runs on unsupported OS/architecture, `no_plugin`, or `CGO_ENABLED=0`, report skip/unavailable rather than success. Keep ordinary DTO and registry tests runnable on those platforms. Ensure the `no_plugin` build retains the exported `ErrRemoteExtensionUnsupported` symbol for API parity; the stub loader may continue to return `ErrUnsupportedRPC` for every path because the entire native/remote plugin feature is disabled in that build.

## Adjacent remote capability truth

The HTTP JSON descriptor path returns `rpcExtension` values whose Init/Shutdown methods only log success; it has no remote Hook/tool call transport (`internal/extension/plugin.go:139-151,193-195`). Do not add a transport or treat a descriptor as executable. In the same bounded implementation, make `DefaultPluginLoader.Load` reject `http://` and `https://` extension paths before issuing a request with a stable `ErrRemoteExtensionUnsupported`. Preserve the existing `grpc://` `ErrUnsupportedRPC` behavior. This is rejection of an unavailable capability, not a new wire protocol.

The unavailable error must not echo the URL: it can contain credentials or a secret query. Sanitize the existing `extension.plugin.load` slog `path` field, PluginManager's `extension.plugin_manager.load_failed` slog `path` field, and PluginManager's `plugin %q` error wrapping for HTTP(S) paths so this rejection path does not print the endpoint. Use a fixed redacted label for remote paths; do not include URL host, userinfo, path, or query. Retain local `.so` diagnostics and their existing `errors.Is` chain behavior.

Tests:

- Replace contradictory HTTP-loader success/response tests (`TestPluginLoaderHTTP`, `TestPluginLoaderHTTPNoExtensions`, `TestPluginLoaderHTTPMissingKey`, `TestPluginLoaderHTTPEmptyBody`, `TestPluginLoaderHTTPConcurrent`, `TestPluginLoaderHTTPS`, `TestPluginLoaderingHTTPBadPayload`, `TestPluginLoaderingHTTPNonOK`, and `TestPluginLoadHTTP_LocalhostAllowed`) with assertions for immediate `ErrRemoteExtensionUnsupported` and zero requests. Rework `TestPluginLoaderCanceledContext` and `TestPluginLoadHTTP_SSRSafeClient` to verify that HTTP(S) rejection happens before cancellation handling or dialing. Rework the plugin-loader-specific `TestPluginLoaderSSRFBlocksPrivateIPs`, `TestPluginLoaderSSRFResponseSizeLimit`, and `TestPluginLoaderSSRFRejectsNonHTTPS` so they no longer claim to verify an HTTP transport that is unavailable; retain the generic SSRF protection tests in `internal/tools`.
- Under `-tags e2e`, rewrite `TestET_Phase48_SSRF_BlocksPrivateIPs` and `TestET_Phase48_SSRF_RequiresHTTPS` to assert immediate `ErrRemoteExtensionUnsupported`, and retain transport-level SSRF tests for the shared HTTP client where applicable.
- `TestPluginLoaderRejectsHTTPDescriptorAsUnavailable`: use an `httptest.Server` request counter and a URL containing a unique query canary; assert the request count remains zero, `errors.Is` matches the new sentinel, and error/captured logs omit the canary.
- `TestAssemblyRejectsHTTPPluginPathAsUnavailable`: configure the HTTP path through the normal plugin configuration; assert assembly fails before publishing any extension or returning a ready agent. With the active loader build, expect `ErrRemoteExtensionUnsupported`; with `-tags no_plugin`, explicitly expect the stub's `ErrUnsupportedRPC` because the entire loader is disabled before scheme dispatch.
- Keep `TestPluginLoaderGRPCUnsupported` proving the existing gRPC sentinel.
- Define `ErrRemoteExtensionUnsupported` in a build-tag-independent source (or identically in both build-tag variants) so the `no_plugin` API compiles; its all-path disabled-loader behavior remains `ErrUnsupportedRPC`.
- The native-plugin journey above is the positive counterpart; it proves the supported built-in loader still works end to end.

## Reviewer gate

Before builder handoff, an independent reviewer should verify that this contract is consistent with the accepted R7 design and does not break the public extension registration signatures or HookChain semantics. The reviewer should pay particular attention to owner identity through staged commit, content disclosure being intentional for configured extensions, bounded error strings, `AfterRun` non-rollback behavior, and the native plugin fixture's supported-platform gate. No production change is authorized by this designer document; builder work begins only after review approval and R6 prerequisite confirmation recorded by the parent task owner.
