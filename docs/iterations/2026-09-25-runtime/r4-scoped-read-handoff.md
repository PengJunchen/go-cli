# R4 scoped workspace-read handoff

Status: design-only follow-up for R4-EFFECT-01. This proposal grants no effect by itself and does not close the R4 gate. It is based on the current source in `codex/runtime-r4-scoped-read-design` and the accepted boundaries in [R4 remaining source contract](r4-remaining-source-contract.md) and [trusted-read review](reviews/r4-trusted-read-review.md).

## Decision

Mint `workspace_read` only for the four concrete first-party file readers `read`, `ls`, `grep`, and `find`, and only when each registered instance already contains a valid, non-empty `PathWhitelist`. The trusted scope must be derived from that instance's canonical roots—the exact roots its executor uses—not by resolving `Sandbox.AllowedPaths` a second time. A zero-root/unrestricted reader and an invalid whitelist remain ordinary unknown registrations. The existing invalid-whitelist behavior rejects every path, so retaining it as unknown does not broaden access or introduce a new assembly startup failure.

Keep effect identity separate from completion proof. Read and Ls may retain their existing inline completion proof; Grep and Find acquire a workspace-read effect only through this proposal and do not acquire completion proof from it. Their completion continues to depend on the independently reviewed actual helper/process join path. Bash, Write/Edit, Git tools, LSP, remote tools, custom tools, MCP, extensions, and SubAgent dispatch do not use this registrar.

## Registration seam

Add an unexported capability in `internal/tools`:

```go
type trustedScopedWorkspaceReadRegistrar interface {
	registerTrustedScopedWorkspaceRead(context.Context, ToolDefinition) error
}
```

`registerDefaultTool` is the only default-tool dispatch point that should request this capability, and only for `*ReadTool`, `*LSTool`, `*GrepTool`, and `*FindTool`. Forward the unexported method through `DeferredToolRegistryAdapter` and `MiddlewareToolRegistry`, as their existing private trusted-registration forwarders do. `DefaultToolRegistry` must repeat the concrete pointer-type checks and reject nil pointers. Do not add a public effect/scope parameter or a generic “trusted registration” constructor: code outside package `tools`, ordinary `Register`, and dynamic registrars must be unable to mint this authority.

If a registry in the `RegisterDefaults` chain does not implement the private capability, retain the existing ordinary `Register` fallback. That registry gets no `workspace_read` metadata or scope; missing capability must never be treated as implicit trust.

For a supported reader, inspect its private `whitelist` value and branch as follows:

| Reader whitelist | Registration result |
|---|---|
| `invalid == false` and canonical `paths` is non-empty | `RegistrationSourceBuiltin`, exactly `EffectWorkspaceRead`, scope fingerprint from those roots |
| `invalid == false` and `paths` is empty | ordinary registration (`tool_registry`, `unknown`, zero scope) |
| `invalid == true` | ordinary registration (`tool_registry`, `unknown`, zero scope); preserve the executor's existing deny-all behavior |
| Any other definition, including same-name custom/MCP/extension tools | ordinary registration (`tool_registry`, `unknown`, zero scope) |

The capability must use the concrete instance's existing `whitelist.paths`. `NewPathWhitelist` has already cleaned, made roots absolute, and resolved symlinks when the reader was constructed. It stores a separate private slice; `ReadTool`, `LSTool`, `GrepTool`, and `FindTool` expose no setter for it. Copy that slice before canonical sorting/deduplication for identity. Do not re-resolve the original config strings, use `Workdir` as a substitute, or include a fingerprint cache that could hide later registration replacement. The executor keeps its original whitelist slice, so metadata describes the same configured roots without mutating execution behavior.

Encode the root set deterministically with a version tag and length-delimited UTF-8 path bytes (for example, a fixed domain prefix followed by each sorted, deduplicated root's byte length and bytes), then pass the resulting non-empty token to the existing private `newTrustedToolScope`. Sorting and deduplication are for the identity encoding only; they do not rewrite the executor's whitelist. Length delimiting avoids ambiguous concatenation. The scope fingerprint remains an opaque SHA-256 identity fact, not a path authorization object or a deserializable capability.

Use a narrow private registration helper or equivalent extension to `registerDefinitionWithCompletion` so source, effects, scope, and any already-reviewed completion proof are written together under the existing registry fence. Re-registering the same name must still increment the registration generation and replace all metadata atomically; an ordinary same-name replacement clears the trusted source/effect/scope. The existing inline-reader path currently grants Read/Ls completion proof but no effect; do not repurpose completion proof as authority. Prefer one combined private branch for the four readers so effect and completion are recorded from the same exact definition snapshot.

## Configuration and path boundary

The production path is `assembleState.assembleTools`: it creates `DefaultToolRegistry`, wraps it in `DeferredToolRegistryAdapter`, and supplies `WithRegisteredPathWhitelist(s.rc.Sandbox.AllowedPaths)` to `RegisterDefaults` only when the configured list is non-empty. `RegisterDefaults` constructs the four concrete reader definitions with `WithReadPathWhitelist`, `WithLSPathWhitelist`, `WithGrepPathWhitelist`, and `WithFindPathWhitelist`. Keep that existing wiring. The `--no-sandbox` branch changes Bash sandbox construction; it must not disable the separately configured reader whitelist.

The identity scope is captured from each constructed reader, not from a parallel copy of CLI config. `NewPathWhitelist` records a private canonical slice and a private invalid bit. With no roots it is deliberately unrestricted; with an invalid non-empty scope `IsAllowed` denies every target. Preserve both behaviors. Do not silently convert invalid roots to an unrestricted whitelist, and do not change the current config-validator/startup behavior as part of this slice.

At execution, the readers continue resolving and validating requested targets against the captured whitelist. This is an application-level path check, not an OS sandbox: a concurrent filesystem or symlink change can race validation and the later open/traversal. `AllowedPaths` also does not confine Bash. The effect/scope identity must not claim stronger isolation than these checks provide.

Grep and Find are eligible only with their reviewed bounded invocation paths intact: Grep uses `rg --no-config`, `-e <pattern>`, `-- <path>` and removes `RIPGREP_CONFIG_PATH`; Find puts `--` before the model pattern, and its fallback passes the resolved root before `-name`. The host-selected helper binaries are treated as trusted host executables under the existing review boundary. This does not promote Git status/diff/blame, LSP, or arbitrary helpers: Git clean/process filters remain executable, and LSP URI scope remains unverified. Those stay unknown.

## Approval and Plan behavior

`preparedPlanCallAllowed` currently accepts `EffectWorkspaceRead` only when the prepared identity source is `RegistrationSourceBuiltin`, the scope fingerprint is non-zero, and the identity tool name matches the call. The private registrar above is what restricts that otherwise generic policy condition to the four concrete readers. Active Plan Mode therefore allows these registered, scoped readers while their executor still enforces the same path set. Empty/unrestricted readers, invalid-root readers, same-name ordinary replacements, and all other unknown tools remain denied by the existing fail-closed policy.

The default/auto prepared classifier likewise trusts a workspace-read effect only with built-in provenance and non-zero scope. `AutoFull` remains explicit operator authorization and is not evidence that a registration is a trusted read. Approval cache identity already incorporates the registration identity, effect, scope fingerprint, schema/argument digests, and fence generations; changing roots or replacing a registration must not reuse an approval from the earlier identity. Do not change the reviewed network-read/control matrix, `exit_plan_mode`, or D-01 behavior in this slice.

## Bounded implementation and tests

Keep implementation ownership in `internal/tools/trusted_registry.go`, `builtins.go`, and the private forwarding points in `deferred.go` and `middleware_registry.go`; add focused tool tests there. The assembly behavior test belongs with CLI approval/Plan integration, coordinated with that owner. Do not change Loop, session, approval semantics, or filesystem enforcement in this handoff.

The minimum acceptance tests should prove:

1. `RegisterDefaults` with two non-empty valid roots gives exactly `read`, `ls`, `grep`, and `find` built-in provenance, one `workspace_read` effect, and a non-zero scope; equivalent root order/duplicates encode identically, while a different root set has a different scope.
2. Empty roots and invalid whitelists stay unknown with zero scope. A normal/custom registration with a reader's canonical name stays unknown. Replacing a scoped reader invalidates an old prepared invocation and does not retain old scope/effects.
3. The four executors still allow targets inside their configured roots and reject outside absolute paths and tested symlink escapes. Empty configuration preserves existing unrestricted direct-reader behavior while leaving the effect unknown. `--no-sandbox` does not remove configured reader path checks.
4. Through the real assembled registry and prepared approval path, active Plan permits in-scope Read/Ls/Grep/Find and existing reviewed network/control cases, while unknown/custom same-name tools, Bash, Write/Edit, Git, LSP, and other unreviewed registrations are stopped before their executor can produce a side effect. A scope-A decision cannot dispatch a scope-B replacement.
5. Effect metadata and completion proof are asserted independently: Read/Ls may keep their currently accepted inline proof; Grep/Find do not receive inline proof from `workspace_read` and only receive any proof established by their actual joined helper lifecycle.

This bounded slice does not satisfy R4-REV-01 by itself. The final phase gate still needs the generated real-Assembly registration/effect/schema/decision matrix, the complete Plan side-effect matrix, and the combined affected-package verification described in `r4-remaining-source-contract.md`.
