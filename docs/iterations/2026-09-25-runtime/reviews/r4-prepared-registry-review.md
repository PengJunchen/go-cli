# R4 prepared registry and proxy review

## Final fixed-snapshot follow-up: `9671f277e03c042b89b425f64453a94e3ef4f40a`

**Accepted for the prepared registry/proxy/queue slice.** This commit closes the two schema findings reproduced on `5002bc0`: schema keyword/type applicability and numeric comparison are now validated before policy admission.

- Common schema checks run before the object early return, and still run recursively for object properties and array items. The allowlisted-key scan remains at the start of every recursive node, so an unsupported key nested below multiple array items fails closed.
- Bounds and values use bounded exact rationals rather than `float64`. The parser limits numeric text to 256 bytes and exponents to ±1024, validates JSON number syntax, and rejects NaN/Infinity and out-of-budget exponents. Integer checks use exact integrality and bound comparisons preserve adjacent values above 2^53.
- The earlier object-schema examples now return `ErrPreparedSchemaInvalid` before the policy callback. The 2^53+1 argument is rejected against a 2^53 maximum, while the inclusive boundary is accepted.

Independent verification on this fixed snapshot:

- `rtk go test -race ./internal/tools -run '^(TestPreparedSchemaRejectsMalformedShapesBeforePolicy|TestPreparedSchemaEnforcesIntegerEnumNestedArraysAndNumericBounds|TestPreparedSchemaRejectsObjectMisappliedKeywordsBeforePolicy|TestPreparedSchemaNumericBoundsDoNotRoundLargeJSONIntegers|TestPreparedGitBlameRejectsFractionalLineNumbersBeforePolicy|TestPreparedRegistryAcceptsRegisteredDefaultToolSchemas|TestReviewSchemaRationalBoundedExactInputs|TestReviewZeroNumericBoundAcceptsBuiltinFloatZero|TestReviewNestedArrayRejectsMalformedAndUnknownChildSchema|TestReviewBuiltinIntegerAcceptsFloatZero)$' -count=1` passed 20 tests. The four `TestReview...` cases were temporary adversarial probes and are not part of the product tree.
- The probes covered malformed constraints nested through multiple array items, an unknown nested keyword, `json.Number` values on either side of 2^53, exact zero bounds, the existing `git_blame` integer schema with `float64(0)`, non-finite Go floats, and exponent/text limits. The builder's full `internal/tools` race suite was not repeated.

This accepts only the reviewed prepared registry/proxy/queue slice. It does not certify the assembled R4 integration or its remaining gates.

## Intermediate fixed-snapshot review: `5002bc0cea12f225548b9a160e0ff3b8a553a924`

Reviewed this fixed commit in a detached worktree. At this snapshot, the four findings from the initial `4f1d0ea` review were closed, but the following two schema issues remained and were fixed by `9671f27` below.

The four earlier findings are closed as follows:

- Proxies pin the registration identity and generation and return captured name, description, schema, and capabilities. `Execute` and streaming dispatch reject stale pins. `TopTools` results are reacquired through the protected registry, so callers receive protected child proxies rather than provider-owned raw definitions.
- `MiddlewareToolRegistry.Get`/`List` fail closed unless the dispatch fence, policy, and stages are configured. Protected execution has no raw fallback. Deferred placeholders remain display-only and their `Execute` returns an error.
- Ordinary and streaming prepared executor chains have an innermost atomic one-shot guard; a second `next` returns `ErrPreparedInvocationConsumed` without calling the definition again.
- The mutation queue runs the captured prepared invocation. Cancellation or registration change before the final fence commit prevents execution and leaves `DispatchCommitted` false; the invocation is consumed. After the private fence marks dispatch committed, an already-started side effect cannot be rolled back.

### Remaining blocker: malformed object-schema constraints bypass validation

`validatePreparedSchemaNode` returns from the object branch at `internal/tools/prepared_registry.go:222-247` before the common checks at lines 258-284. These schemas currently pass validation, although `items` is only supported for arrays and numeric bounds are only supported for numeric types (and must be finite numbers):

```go
map[string]any{"type": "object", "properties": map[string]any{}, "items": "malformed"}
map[string]any{"type": "object", "properties": map[string]any{}, "minimum": 1}
map[string]any{"type": "object", "properties": map[string]any{}, "maximum": "malformed"}
```

Each must fail with `ErrPreparedSchemaInvalid` before policy approval. The review-only `TestReviewObjectSchemaRejectsInapplicableConstraints` reproduced all three being accepted.

### Remaining blocker: large integer bounds lose precision

`schemaNumber` converts `json.Number` and integer values to `float64` (`internal/tools/prepared_registry.go:441-477`). This makes a value above an integer maximum compare equal after rounding. For example, the following must reject the argument before policy/execution, but the fixed snapshot accepts it:

```go
schema := map[string]any{
    "type": "object",
    "properties": map[string]any{
        "n": map[string]any{"type": "integer", "maximum": json.Number("9007199254740992")},
    },
}
args := map[string]any{"n": json.Number("9007199254740993")}
```

The review-only `TestReviewNumericBoundsDoNotRoundLargeJSONIntegers` reproduced this. Preserve exact integer/decimal comparisons (or fail closed when exact comparison is unavailable); do not let precision loss turn a violated bound into an accepted call.

### Verification and scope

- An independent focused race run passed: 30 selected prepared-schema, proxy, capability, streaming, mutation-queue, and registry tests. The builder's full `internal/tools` race run was not repeated.
- A review-only probe confirmed all 24 registered built-in schemas (the 7 base tools plus 17 Git tools) are accepted, and `additionalProperties: false` rejects an unknown argument.
- The three object-schema cases and numeric-bound case above were separate negative review probes and failed against `5002bc0`; they pass as rejection/boundary checks on `9671f27`. These temporary tests are not part of the product tree.
- This review covers the prepared registry/proxy/queue slice only; it does not certify the full R4 integration or its remaining gates.

Reviewed the fixed product snapshot `4f1d0ea513c35998589c4bfd88975dcf35418bad` in a detached worktree. This is a review of the Prepare/proxy seam only; the mutation queue and complete R4 wiring are not present in this slice.

## Initial snapshot decision (`4f1d0ea`)

**Not accepted yet.** The snapshot has four reachable fail-open paths that need fixes before the prepared registry can serve as the protected execution boundary.

1. **Schema validation accepts malformed or unsupported schemas and arguments.** `validateCapturedSchema` treats every non-object schema as valid (`internal/tools/prepared_registry.go:116-129`). The underlying `validateBasicSchema` also returns success if `properties` is absent or malformed, skips malformed `required` values and unknown properties, and checks `integer` exactly like `number` (`internal/tools/middleware.go:248-309`). This is reachable with registered definitions: for example, `GitBlameTool` declares `start_line` and `end_line` as integers and `additionalProperties: false` (`internal/tools/git_blame.go:38-55`), but `start_line: 1.5` passes and is truncated by the executor (`git_blame.go:75-82`); extra fields also pass. Other current schemas use `enum` and nested properties. The contract does not require a full JSON Schema implementation, but unsupported/malformed schema constraints and arguments must fail closed before policy approval. Define and validate a supported subset, reject unsupported schema shapes/keywords, and cover required/type/integer/unknown-field cases with assertions that policy and executor were not called.

2. **A proxy does not pin the registration snapshot it exposed.** `newProtectedToolDefinition` captures `Parameters` independently from the registry snapshot, while `Name` and `Description` delegate to the live definition (`internal/tools/prepared_proxy.go:15-32,96-97`). On `Execute`, the proxy only checks the current live name and then calls `PrepareToolCall` by name (`prepared_proxy.go:99-109`); `PrepareToolCall` resolves the current registration again (`prepared_registry.go:23`). A caller can obtain proxy A, replace the same-name registration with B, then execute A and silently run B. A mutable definition can likewise expose metadata/schema that differs from the schema used to authorize. Capture definition identity, schema, description, executor and generations as one registration snapshot when creating the proxy; keep all exposed accessors on that snapshot and reject a stale proxy or dispatch using its pinned snapshot, never silently re-resolve by name. Apply the same rule to protected `TopTools` results.

3. **An unconfigured MiddlewareToolRegistry proxy falls back to raw execution.** `preparedExecutionState` considers the pipeline enabled only when either policy or stages are configured (`internal/tools/prepared_proxy.go:268-282`), while `Execute` falls through to `d.fallback.Execute` when neither is configured (`prepared_proxy.go:99-116`). `TestMiddlewareToolRegistry_PassthroughNoWrappers` currently requires that behavior (`internal/tools/middleware_registry_test.go:111-130`). This bypasses the policy and prepared stages through an API that returns protected proxies. As directed by the R4 boundary, Middleware `Get`/`List`/`Search` must return an unavailable error on execution unless both policy and stages are configured; update the passthrough test to assert fail-closed behavior or configure the complete pipeline. The raw `DefaultToolRegistry` API may retain its existing semantics.

4. **Execution wrappers can invoke one prepared executor more than once.** `preparedExecutor` passes the real executor as `next` without a consumption guard (`internal/tools/prepared_registry.go:134-149`); the streaming variant does the same (`152` onward). A wrapper calling `next` twice invokes the underlying definition twice under one PreparedInvocation, despite its single-use dispatch contract. Add a small one-shot guard at the innermost `next` for ordinary and streaming calls: the first call may reach the definition; later calls return an explicit already-consumed error without another invocation. This is a contract guard, not a sandbox for wrappers. Document `SetPreparedToolStages` and `ExecutionWrappers` as trusted, audited assembly: wrappers must not add independent side effects, asynchronously escape execution, or mutate the prepared call.

## Accepted boundaries and evidence (initial `4f1d0ea` snapshot)

- Ordinary `Register` remains `EffectUnknown`; trusted workspace-write registration is limited to concrete `WriteTool`/`EditFileTool`, and trusted network-read registration is limited to concrete `WebSearchTool`/`WebFetchTool` (`internal/tools/trusted_registry.go:181-186` and corresponding registration paths). This narrow registration boundary is appropriately conservative.
- Get/List proxies preserve the supported `Parameterized`, prompt-guideline, streaming, and TopTools capability combinations. Configured streaming dispatch uses the prepared invocation path. At this original snapshot, TopTools results were rewrapped but still needed pinned registration snapshots (closed at `5002bc0`).
- `Write`/`Edit` returned `ErrPreparedMutationQueueUnavailable` before policy or execution at this original snapshot (`prepared_registry.go:36-38`). The mutation queue path was added after `4f1d0ea` and reviewed in the `5002bc0` follow-up above; R4 integration gates remain separate.
- The 24-case selected race run passed on this snapshot:
  `rtk go test -race ./internal/tools -run '^(TestPrepareToolCall.*|TestPrepared.*|TestProtected.*|TestMiddlewareToolRegistry.*|TestRegisterDefaultsMintsOnlyReviewedWriteEffects|TestDefaultToolRegistry.*)$' -count=1`.
  Separate review-only adversarial cases reproduced the failures above: malformed schema and fractional integer were accepted, an old same-name proxy executed its replacement, and a wrapper's second `next` call reached the executor again. Those temporary cases were removed after review.
