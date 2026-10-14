# R7 remaining design review

**Reviewed design:** [`r7-remaining-design.md`](../r7-remaining-design.md), working-tree version (untracked at review time).  
**Source checkout:** `25577ec5b3876439216ae2423014d8719efed1db`.  
**Review type:** source-grounded design review. No product source was changed and no tests were run.

## Verdict

Accept the narrow Hook action/snapshot direction, reuse of the R4 schema gate, and XML attribute encoding requirement. The design is not ready as a builder handoff until it adds the bounded directory walker, a concrete R7-owned project-source freeze path, canonical source authority rules for explicit `Skill.Dir`, and model-facing delivery of skill arguments. The R6 API is absent, but that should be a bounded integration task rather than a reason to defer project skills indefinitely.

## Findings

### P1 — Candidate limits do not bound directory enumeration

`LoadDir` uses `filepath.WalkDir` and applies no per-entry or depth barrier ([`skill_loader.go:141-180`](../../../../internal/skill/skill_loader.go#L141)). `WalkDir` reads a directory's entries before invoking the callback, so a root with a very large flat directory can consume unbounded work and memory before the 128-candidate limit is noticed. Candidate count also says nothing about ignored files or nested directory depth.

Keep the proposed 128-candidate, 1 MiB-per-file, and 16 MiB-total-read limits, and add a 4,096-total-entry and 128-depth ceiling. Enumerate depth-first with `os.File.ReadDir(1)`, count every entry (including ignored names), and fail the whole load as soon as a limit is crossed. Gather only the bounded candidate paths, sort them by canonical-root-relative path, reject candidate symlinks and non-regular files, then read each file in fixed-size chunks with context checks. Do not use `WalkDir` or `os.ReadDir` for this bounded path. This keeps ordering deterministic while bounding enumeration, candidate memory, and reads.

### P1 — The frozen project-source handoff is missing, and current trust is path-only

There is no R6 frozen-source runtime API in this checkout, as the design says at lines 15 and 55. The existing `DefaultTrustManager.IsTrusted` looks up the exact supplied path and expiry ([`trust.go:52-83`](../../../../internal/approval/trust.go#L52)); it does not canonicalize the path or compare a current fingerprint. `TrustProject` fingerprints only `.go-cli/mcp.json` ([`trust.go:85-99`](../../../../internal/approval/trust.go#L85)). That is not a persisted skill-content or directory-membership digest. Also, `cmd/cli/main.go` creates a local in-memory manager for `LoadTrusted`, while `registerSkillTools` asks the separate process-wide manager returned by `approval.GetTrustManager()` ([`main.go:23-30`](../../../../cmd/cli/main.go#L23), [`registry.go:18-31`](../../../../internal/approval/registry.go#L18), [`assemble.go:2420-2431`](../../../../internal/cli/assemble.go#L2420)). The production CLI therefore does not currently share one trust authority across config and skill loading.

Replace the “wait for R6 owner/API” step with this small R7 handoff, owned by `internal/skill/skill_loader.go` and `internal/cli/assemble.go`:

1. Add loader entry points `CaptureDir(ctx, canonicalRoot) (*SourceSnapshot, error)` and `LoadSnapshot(ctx, snapshot)`. Keep `SourceSnapshot` opaque (private fields; accessors return copies); it contains the canonical root, sorted candidate membership, exact bytes, and a digest computed with length-prefixed relative paths and bytes during the bounded capture. The parser, deferred closures, and `SkillInfo` prompt input must consume only this snapshot; none may reopen a path.
2. Replace the current `IsTrusted`-then-`LoadDir` branch with one `loadTrustedProjectSkills` handoff in `internal/cli/assemble.go`: resolve canonical roots, consult the same `approval.TrustManager` instance used by `config.LoadTrusted`, capture the root once, and consume that exact snapshot for parse/register/prompt. Thread/register that manager once in `cmd/cli/main.go` so config and skills do not consult different instances. The snapshot digest binds the capture and parse to one byte set; it is runtime-only, with no persisted R7 manifest or registry.
3. Treat the digest as identity for the exact bytes consumed in this run. Do not claim it is a previously approved skill digest: the current `TrustEntry.Fingerprint` only covers `mcp.json`. If historical skill-byte approval is required, the present store has no evidence for it and that requires a separate trust-model decision.

If the trust check itself needs a named seam, put one method beside the existing manager in `internal/approval/trust.go` (no new registry or persistence):

```go
type ProjectSkillSourceTrust interface {
    CaptureTrustedProjectSkills(ctx context.Context, projectRoot, skillRoot string) (*skill.SourceSnapshot, error)
}
```

`DefaultTrustManager` implements it by canonicalizing and containing both roots, checking the existing project-path authority, and delegating exactly once to `skill.CaptureDir`; the returned opaque snapshot is what `LoadSnapshot` consumes. `registerSkillTools` requires this interface for project sources and fails closed when the configured manager cannot provide it. This seam connects the existing authority decision to the exact runtime bytes without adding a manifest. The current `Fingerprint` field still cannot validate those bytes, as noted above.

For `Skill.Dir`, classify by resolved canonical location, not by “auto-discovered” or `filepath.IsAbs`: a directory under the canonical project root uses the project trust/snapshot path; a directory under the canonical user skill root (`$HOME/.config/go-cli/skills`) uses the bounded user loader; all other paths fail closed. This closes the current explicit-path bypass and prevents a global config path from granting project trust ([`assemble.go:2406-2431`](../../../../internal/cli/assemble.go#L2406)). `config.LoadTrusted` merges global and project config into one `Config`, and `SkillConfig` retains only `Dir`, so the final value has no source provenance ([`config.go:128-193`](../../../../internal/config/config.go#L128), [`types.go:284-288`](../../../../internal/config/types.go#L284)). Canonical-root classification avoids depending on provenance that is not present and rejects paths outside the two allowed roots.

### P1 — Validated arguments are currently invisible to the model

The draft puts accepted arguments in `ToolResult.Metadata` only (line 61). The core loop sends `ToolResult.Output` as the tool receipt content and ignores metadata ([`loop.go:783-784`](../../../../internal/core/loop.go#L783)). As written, a valid skill call reaches `SkillAdapter.Execute`, but the model receives only the skill prompt; it never receives the arguments it supplied ([`skill_adapter.go:57-97`](../../../../internal/skill/skill_adapter.go#L57)).

Add a bounded canonical JSON rendering of the frozen `call.Args` to `ToolResult.Output`, under an explicit instruction that these arguments are untrusted data. Use deterministic `encoding/json` output and a fixed byte ceiling (the existing LLM tool-payload ceiling is 400,000 bytes); return an execution error if the rendering exceeds it. Keep a defensive copy in metadata for callers that use it, but do not rely on metadata for model context. The arguments must not grant any tool capability; `Tools()` remains descriptive text.

### P2 — `parameter_schema` needs a strict frontmatter representation

The design names a new `parameter_schema` key but does not define its serialized syntax. The current parser only collects a one-level `parameters:` map and silently ignores other unknown keys ([`skill_loader.go:227-333`](../../../../internal/skill/skill_loader.go#L227)). A nested JSON Schema supplied as ordinary YAML would therefore be dropped rather than reaching R4.

Specify one strict representation, such as a `parameter_schema: |` block containing exactly one JSON object. Parse the block as JSON and reject malformed/trailing content; pass the resulting value through the existing R4 supported-keyword/schema validation when the deferred tool is materialized. Do not reinterpret legacy `parameters:` values. The rest of this design is aligned with the existing `tools.Parameterized` optional interface and R4 validation-before-policy order ([`tools.go:61-73`](../../../../internal/tools/tools.go#L61), [`prepared_registry.go:111-145`](../../../../internal/tools/prepared_registry.go#L111)).

### P2 — Hook DTO and identity details are not fully typed or available today

The event DTOs are a good boundary, and current `core.AgentMessage` fields can be copied into a stable extension-owned message DTO. However, `History []any` leaves the most complex payload field open-ended (design lines 23-45). Define a `HookMessageSnapshot` with explicit message fields, including typed content blocks/usage/tool calls and recursively cloned JSON arguments; reject unsupported values before calling a hook. Map errors to a stable class without calling `Error()` or exposing error objects. For a blocked before-run event, keep the typed error text to extension and hook identity only; an arbitrary `HookResult.Reason` can itself repeat submitted content.

The adapter currently receives only an `extension.Hook`, sets `Source` to the constant `extension-bridge`, and formats errors with only `Hook.Name()` ([`extension_bridge.go:31-73`](../../../../internal/cli/extension_bridge.go#L31)). The extension registry also discards which `Extension` registered each hook when it publishes staged registrations ([`manager.go:153-170`](../../../../internal/extension/manager.go#L153), [`registry.go:130-148`](../../../../internal/extension/registry.go#L130)). Add the owner name to the existing staged hook registration (or pass `{extensionName, hook}` from the manager) so `ErrHookBlocked` can identify both without carrying submission data.

The Pass/Block-only contract otherwise fits current runtime behavior: `HookChain.Before` stops at the first error; both hook middleware paths log and ignore after-run observer errors, returning the completed run result unchanged ([`hook.go:69-82`](../../../../internal/core/hook.go#L69), [`hook.go:246-251`](../../../../internal/core/hook.go#L246), [`hook.go:289-294`](../../../../internal/core/hook.go#L289)). Exporting `HookActionBlock` and rejecting replacement, termination, and unknown actions is the right narrow support boundary.

### Accepted XML requirement

The skill attributes are currently interpolated into XML with `%s` and `%q`, which do not provide XML attribute encoding ([`system_prompt.go:268-281`](../../../../internal/core/system_prompt.go#L268)). The specified structured `encoding/xml` attribute encoder is appropriate. Keep the parse-after-render assertion: adversarial name/category/description values containing quotes, `&`, `<`, or a closing-tag sequence must parse as exactly one `skill` element with unchanged attribute text and no injected node.

## Review boundary

This report reviews the current checkout and the untracked design text only. It does not claim any planned test exists or passes, and it does not accept R7 implementation or phase completion.
