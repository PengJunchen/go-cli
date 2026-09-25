# R4 scoped workspace-read design review

Reviewed fixed design: `d7768d790cc631d1b34459cab83bfb95988f3a88` on `codex/runtime-r4-scoped-read-design`. This is a source-grounded design review only; it is not implementation acceptance or the R4 release gate.

## Verdict

Needs one targeted revision before this slice is ready for implementation. The design's scope minting, path-source, Plan Mode, stale-permit, and helper boundaries otherwise match the current source and the previously accepted R4 contracts.

## Finding

**[P1] Preserve the existing Read/Ls provenance and inline completion proof when no workspace scope is minted.** The handoff's decision table says an empty or invalid whitelist makes all four readers ordinary `tool_registry` registrations with unknown effects and zero scope. That is correct for `grep` and `find`, but it changes `read` and `ls` behavior: current `registerDefaultTool` always routes `*ReadTool` and `*LSTool` through the private inline-reader registrar (`internal/tools/builtins.go:205-210`). `DefaultToolRegistry.registerTrustedInlineLocalReader` verifies those exact concrete types and registers them as `RegistrationSourceBuiltin`, with unknown effects, zero scope, and `CompletionInlinePipelineJoined` (`internal/tools/trusted_registry.go:270-283`). The deferred and middleware adapters forward that existing path (`internal/tools/deferred.go:294-300`, `internal/tools/middleware_registry.go:135-141`). Prepared dispatch captures this proof and starts the inline completion signal from it (`internal/tools/prepared_registry.go:79-83`, `internal/tools/prepared.go:745-753`).

The new design should keep effect/scope authority independent from that already-reviewed completion fact:

| Reader state | Registration and effect | Scope | Completion proof |
|---|---|---|---|
| `read` / `ls`, valid non-empty whitelist | Built-in; exactly `workspace_read` | Non-zero fingerprint of the captured roots | Preserve `inline_pipeline_joined` |
| `read` / `ls`, empty or invalid whitelist | Built-in; unknown effect | Zero | Preserve `inline_pipeline_joined` |
| `grep` / `find`, valid non-empty whitelist | Built-in; exactly `workspace_read` | Non-zero fingerprint of the captured roots | None minted by this registration; retain only the separately reported helper/process proof |
| `grep` / `find`, empty or invalid whitelist | Ordinary unknown registration | Zero | None minted by this registration |
| Any unreviewed or same-name replacement definition | Ordinary unknown registration | Zero | No proof retained from the replaced definition |

For a registry that lacks the new scoped-read capability, the fallback must likewise leave `read`/`ls` eligible for the existing inline-reader registration when that capability is available, while granting no workspace-read effect or scope. Otherwise use ordinary `Register`. This preserves existing unscoped read behavior while Plan Mode still rejects it: `preparedPlanCallAllowed` requires the single trusted read effect, built-in source, matching name, and non-zero scope (`internal/approval/prepared_policy.go:311-323`).

## Source checks

- The proposed capability is implementable without caller-minted authority. Its method is package-private, and `DefaultToolRegistry` can repeat exact `*ReadTool`, `*LSTool`, `*GrepTool`, and `*FindTool` checks before atomically publishing source, effect, scope, and proof. A normal registration or same-name replacement goes through `registerDefinition`, which assigns ordinary provenance and unknown effects; the registry fence advances on replacement. The middleware and deferred wrappers are the actual private forwarding seams.
- The root source is the executor's captured `PathWhitelist`, not a parallel config copy. `PathWhitelist` stores private `paths` and `invalid` fields; `NewPathWhitelist` cleans, absolutizes, and resolves symlinks before storing roots (`internal/tools/bash_sandbox.go:87-113`). `RegisterDefaults` applies the configured list to all four reader constructors, including `ReadTool` (`internal/tools/builtins.go:94-107, 150-156`), and each executor validates requested paths against that same whitelist (`internal/tools/read.go:228-250`, `internal/tools/write.go:107-132`, plus the `ls`/`grep`/`find` call sites). The code and handoff correctly describe this as an application-level check with a symlink check/use race, not an OS sandbox. `--no-sandbox` changes Bash construction; the reader whitelist is wired independently in `assembleTools` (`internal/cli/assemble.go:832-920`).
- Sorting and deduplicating a copied root slice for identity does not change whitelist execution semantics. Use a versioned domain prefix and length-frame the exact root bytes; avoid lossy Unicode normalization or conversion because the captured Go strings are the values the filesystem checks use. The scope remains an opaque SHA-256 identity fact, not an authorization object.
- The Plan Mode and default classifier conditions are appropriately narrow when the private registrar is the only source of `EffectWorkspaceRead`. The effect is not derived from the tool name, caller metadata, or a public scope argument. Ordinary same-name tools remain unknown and a changed registration/scope changes identity and the prepared approval key. Plan's separate effect check is therefore still the final gate for unscoped `read`/`ls`; preserving their completion proof does not authorize them.
- Git status/diff/blame, LSP, Bash, mutations, remote tools, MCP, extensions, and SubAgent execution remain outside this effect. Grep/Find hardening is explicitly tied to the reviewed `rg --no-config -e ... -- path` and `fd -- pattern` invocation shape. The handoff does not claim that the workspace-read effect itself proves execution completion: only Read/Ls keep their existing inline proof; Grep/Find must rely on their independently joined helper lifecycle.

The acceptance text should update item 2 and item 5 to assert this distinction: unscoped Read/Ls have unknown effects and zero scope but retain built-in source and inline proof; unscoped Grep/Find are ordinary unknown registrations; the scoped effect never manufactures completion proof. The broader assembled Plan side-effect matrix and affected-package tests remain implementation gates and are not approved by this design review.
