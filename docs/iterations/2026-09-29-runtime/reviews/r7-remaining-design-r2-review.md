# R7 remaining design re-review

**Reviewed design:** [`r7-remaining-design.md`](../r7-remaining-design.md), SHA-256 `a1ded29094cb6de4fa64a724bca33ac4e294a27b9eee8adcf62d272ad816aa2b`.  
**Source reviewed:** current checkout `49d568a`; executable source matches the supplied `25b88ba` product baseline (the difference is documentation only).  
**Review type:** independent, source-grounded builder-handoff review. No product source was changed and no tests were run.

## Verdict

The revision closes the original “wait for R6” gap and is substantially builder-ready. Accept the bounded skill capture, argument delivery, schema, Hook DTO/action, XML, and most of the persistent-manifest requirements. Do not freeze the trust handoff yet: the public trust-status field can be mistaken for authority, and the design needs to state that the actual capability cannot be forged from a public snapshot/config value. Clarify the remaining inventory/dispatch seams below so builders do not weaken the R6 contract while wiring it.

## Blocking finding

### P0 — `ProjectSourceTrustStatus` must not authorize assembly

At lines 22 and 36, `ProjectSourceSnapshot` is a public capture value and `Config` is to carry a public `ProjectSourceTrustStatus` of `trusted` or a denial. Lines 42 and 76 then direct assembly consumers to use the snapshot/status as the authority. A caller can construct a `Config` with a trusted status and a public snapshot; the type design does not distinguish that value from the result of a successful persisted-store check. This would recreate a trust bypass even though ordinary CLI startup uses the manager.

The current implementation reinforces why this boundary needs to be explicit: `approval.TrustManager.IsTrusted` is path/expiry-only and `TrustProject` fingerprints only `.go-cli/mcp.json` ([`trust.go:22-32`](../../../../internal/approval/trust.go#L22), [`trust.go:55-99`](../../../../internal/approval/trust.go#L55)); `cmd/cli/main.go` currently supplies an in-memory manager to `LoadTrusted` ([`main.go:23-30`](../../../../cmd/cli/main.go#L23)); and `RunWithRegistry` currently receives its config only after the main entry point loads it ([`main.go:89-93`](../../../../cmd/cli/main.go#L89)). The new manifest check must therefore mint the authority object itself, not merely set a config flag.

**Required narrow API correction:**

```go
// Fields and constructor remain private to package approval.
type VerifiedProjectSources struct { /* frozen snapshot + verified-entry state */ }

type FrozenProjectSourceTrust interface {
    CheckProjectSourceSnapshot(context.Context, *ProjectSourceSnapshot) (*VerifiedProjectSources, error)
    TrustProjectSourceSnapshot(context.Context, *ProjectSourceSnapshot) error
}
```

`CheckProjectSourceSnapshot` must return a non-forgeable opaque capability only after it has matched the persisted manifest, canonical root, complete source/membership digest, and expiry in the existing store. Keep its fields and usable constructor private to `approval`; its zero value must be invalid and expose no snapshot. The public `NewProjectSourceSnapshot` remains a capture-input constructor only. `config.Config` should keep the verified capability in a private, non-serialized field and expose only an accessor returning the opaque approval type; `ProjectSourceTrustStatus` remains presentation/diagnostic data. Keep the unverified capture separately for the explicitly confirmed `trust grant` path, if needed, but never pass that capture to assembly. Assembly must require the verified capability and obtain its immutable snapshot through defensive-copy accessors. A manually constructed `Config{ProjectSourceTrustStatus: trusted}` or caller-built snapshot without the manager-minted capability must load no project config and publish no project MCP, hook, skill, command, or custom-tool source. Invalid or mismatched snapshots publish nothing. This uses the existing `DefaultTrustManager` and `FileTrustStore`; it adds no second persistent store, capability registry, or signed-manifest framework.

Add a real config/assembly test for that forged-config case, alongside the existing changed/revoked/legacy-store integration cases. In the normal path, the same manager verifies the one captured snapshot, config parses from its captured bytes, and assembly consumes the capability for that exact snapshot.

## Builder-ready clarifications

### P1 — Bind bytes for every present config candidate, not only the winner

The R6 contract summarized at line 7 requires presence and exact bytes for all six root config candidates. Line 30 says to capture all six and the acceptance at line 82 exercises edits to each, but the capture steps only explicitly say to parse the winning candidate from its bytes. State that each present candidate is opened/read once and its exact bytes enter the manifest, including lower-priority candidates hidden by a winner; only the highest-priority present candidate is parsed, using those already captured bytes. Otherwise an edit to a shadowed file could be omitted from the persisted identity despite the acceptance claim.

### P1 — Make “outside canonical root” a deny rule for project executable references

Line 31 allows a root-external reference “unless it has independent authority,” but this handoff defines no such authority. For this bounded R6/R7 implementation, state that project-referenced hooks, MCP, skills, commands, extensions, and other executable sources outside the canonical workspace root fail closed; only the separately specified canonical user-skill root may use the bounded user loader, and it never inherits project trust. Do not turn path permission or a runtime digest into the undefined exception.

### P1 — Name the existing registry construction seam for early help/version

The behavioral requirement at line 40 is right, but the builder still has to decide how to reach it. Today `cli.Run` creates its own registry ([`cli.go:77-82`](../../../../internal/cli/cli.go#L77)), while `RunWithRegistry` registers built-ins inside dispatch ([`cli.go:147-168`](../../../../internal/cli/cli.go#L147)) and `cmd/cli/main.go` loads config first. Its global `--version` path also constructs a version command directly and its `--help` path prints usage rather than resolving the registered command ([`cli.go:171-177`](../../../../internal/cli/cli.go#L171)). Assign `internal/cli/cli.go` to factor/reuse the existing default-command registration and global-flag parse so `cmd/cli/main.go` can create one existing `CommandRegistry` before config load, register trust commands with the one persistent manager, and classify only parser-recognized global `--help` / `--version`. For those invocations, dispatch the registered help/version command with the minimal config; otherwise load/validate config and pass the same registry into normal dispatch. This is a small seam in the existing registry/parser, not a second router or argv-string switch, and it must not initialize provider/session code. Preserve bad-config failure for every non-help/version invocation. The binary acceptance in line 82 should assert this path.

## Accepted portions

- The revision now requires the persistent canonical-root source manifest, exact source bytes and sorted directory membership, legacy path/MCP-only entries fail closed, one `FileTrustStore`-backed manager, TTY-only grant, and same-snapshot config/assembly flow (lines 20–42). This is the correct response to the current path-only store. Preserve those as prerequisites; do not revert to the earlier runtime-only/path-permission alternative.
- The loader now specifies `ReadDir(1)` depth-first enumeration, 4,096 entries, depth 128, 128 candidates, 1 MiB per file, 16 MiB per snapshot, 32 KiB cancellable reads, symlink/non-regular denial, and no partial publish (lines 34, 52, 75, 84). This closes the prior `WalkDir` wide-directory gap.
- `parameter_schema` has a strict single-JSON-object form and delegates its accepted keywords to the existing R4 validator before approval/dispatch. Legacy `parameters` retains its old value/default meaning. Valid arguments reach model-facing deterministic bounded `ToolResult.Output` as untrusted JSON; metadata is only a defensive copy and `Tools()` remains descriptive (lines 56–60, 86).
- Hook events use extension-owned typed snapshots and defensive cloning, preserve owner identity through staging, expose no raw errors/reasons/handles, and only support pass/block with observer-only after-run failures (lines 64–70, 85). Keep the external-package Block fixture test.
- XML attributes use structured encoding with a parse-after-render node assertion, as required (lines 60, 86). The skill plan adds no templating behavior or implicit executor permissions.

The names and ownership for all five implementation slices are sufficiently specific once the trust capability and small registry/capture clarifications above are folded into the prerequisite. This review does not claim any planned behavior test exists or passes.
