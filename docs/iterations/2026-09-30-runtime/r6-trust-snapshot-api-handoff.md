# R6 executable-source trust snapshot API handoff

This note freezes the approval/config boundary implemented in the isolated R6 trust snapshot branch. The accepted design inputs were `r7-remaining-design.md` (SHA-256 `685b4eef2a30ebe1ac2e7a6579e39b393f2938ef5e824206bd970f6b5855a702`), `r6-entrypoint-design.md`, and the accepted final review.

## Authority contract

`approval.ProjectSourceSnapshot` is an immutable, non-authoritative capture. `approval.NewProjectSourceSnapshot(root, fileInputs, directoryInputs)` canonicalizes the root, copies all input bytes/members, sorts records, validates root-relative identities, and calculates a versioned length-prefixed SHA-256 manifest. Its accessors return defensive copies.

The runtime authority interface is:

```go
type FrozenProjectSourceTrust interface {
    CheckProjectSourceSnapshot(context.Context, *ProjectSourceSnapshot) (*VerifiedProjectSources, error)
    TrustProjectSourceSnapshot(context.Context, *ProjectSourceSnapshot) error
}
```

`approval.DefaultTrustManager` implements this optional interface on top of the existing `TrustStore`. `CheckProjectSourceSnapshot` mints `*approval.VerifiedProjectSources` only after comparing manifest version, canonical root, full source records, presence, exact-byte digests, directory memberships, and total digest, and only while the entry has not expired. `TrustProjectSourceSnapshot` persists the capture but does not mint authority; the next startup check is required. Existing path-only `TrustProject` records remain legacy and cannot authorize sources. Path-only `IsTrusted` is not a substitute for the frozen-source interface.

The verified capability has no exported constructor or state. Its read-only API is:

```go
Valid() bool
CanonicalRoot() string
ManifestDigest() string
ManifestVersion() int
SourceRecords() []approval.ProjectSourceRecord
DirectoryRecords() []approval.ProjectSourceDirectoryRecord
SourceBytes(id string) ([]byte, bool)
DirectoryMembers(id string) ([]string, bool)
```

Bytes and member lists are defensive copies. Directory member paths are sorted and workspace-root-relative. For a known but absent directory, `DirectoryMembers` returns `ok=true` with no members. A zero-value capability is invalid. The file trust store persists records/digests, never source contents.

## Config loading boundary

`config.CaptureProjectSources(ctx, canonicalRoot, globalConfig)` returns the raw capture for display/grant only. `config.LoadTrustedWithManager(ctx, approval.TrustManager)` loads user/global config, captures project sources, requires the frozen interface, checks the exact capture, and parses the selected project root config from capability bytes only after verification.

`Config` exposes the narrow handoff methods:

```go
VerifiedProjectSources() *approval.VerifiedProjectSources
ProjectOverlayForVerifiedSources() *Config
CapturedProjectSourcesForGrant() *approval.ProjectSourceSnapshot
UserGlobalConfig() *Config
MCPServersProvenance() config.MCPServersProvenance
```

`ProjectOverlayForVerifiedSources` returns a defensive project-only copy carrying the same private capability. `UserGlobalConfig` returns a defensive copy without project overlay or authority. `CapturedProjectSourcesForGrant` returns a raw snapshot only and must not reach assembly. `ProjectSourceTrustStatus` is presentation only. A manually built or JSON-decoded `Config`, even one with status "verified", has no private capability.

When project trust/capture is denied, `LoadTrustedWithManager` returns a usable global-only config and a diagnostic status; cancellation and deadline errors propagate. Status mapping is `untrusted`, `legacy`, `changed`, `expired`, `unsupported`, `capture_error`, or `store_error`. Typed approval denials are `ErrProjectSourceUntrusted`, `ErrProjectSourceLegacy`, `ErrProjectSourceManifestMismatch`, `ErrProjectSourceExpired`, `ErrProjectSourceSnapshotInvalid`, and `ErrProjectSourceManagerUnavailable`. Capture also exposes typed limit, symlink, outside-root, non-regular, and canonicalization errors. The old `LoadTrusted(pathCallback)` remains source-compatible but ignores the callback as authority and uses global-only behavior.

## Captured identities and limits

The six root config IDs (including absent and shadowed entries) are:

| Relative config candidate | Stable ID |
| --- | --- |
| `.go-cli.yaml` | `config/.go-cli.yaml` |
| `.go-cli.yml` | `config/.go-cli.yml` |
| `.go-cli.json` | `config/.go-cli.json` |
| `go-cli.yaml` | `config/go-cli.yaml` |
| `go-cli.yml` | `config/go-cli.yml` |
| `go-cli.json` | `config/go-cli.json` |

Fixed sources are `hooks/.go-cli/hooks.yaml` and `mcp/.go-cli/mcp.json`. Default directory IDs are `skills/.go-cli/skills` and `commands/.go-cli/commands`. Default skill candidates use `skill/<root-relative-path>`; default command candidates use `command/<root-relative-path>`; extension candidates use `extension/<root-relative-path>`. Explicit directory references use `<category>/<root-relative-path>` (`skills`, `commands`, or `extensions`); explicit file references use their source category (`extensions`, `custom-tools`, `mcp-command`, or `lsp-command`). A root directory reference uses `<category>/root`.

The captured project MCP file is available as `verified.SourceBytes(config.ProjectMCPSourceID)`. Its `servers` array and `mcpServers` map are decoded from those bytes during inventory discovery so local executable command files are also captured as `mcp-command/<root-relative-path>`. The project MCP file is not merged into `Config.MCP.Servers`; downstream MCP wiring must read it from the verified capability. `MCPServersProvenance` labels only the merged `Config.MCP.Servers` list: `user_global`, `project`, or `unknown`. Empty/manual/decoded sources are `unknown`; a non-empty merged list alone is not provenance.

Configured/global references into the workspace are included in the project manifest, while user/global values remain separately available through `UserGlobalConfig`. Project command path references must resolve inside the canonical workspace. Bare project executable names are resolved through `PATH`; unresolved or external names fail closed. A relative `PATH` result is rejected with Go's `exec.ErrDot` semantics instead of being reinterpreted against the project root. Relative slash-containing custom-tool commands resolve against `WorkingDir`, LSP commands against their `WorkspaceRoot`, and commands without a configured working directory against the process working directory. Those resolved files must still be inside the canonical workspace. PATH lookup is bounded to 256 entries and 64 KiB. External global command names stay user/global provenance unless PATH resolves them into the workspace. Capture uses `ReadDir(1)`, at most 4,096 entries per scanned root, DFS depth 128, 128 candidates per root, 1 MiB per file, 16 MiB total captured bytes, and 32 KiB read chunks with cancellation checks. Symlinks, out-of-root project paths, unreadable directories, non-regular candidates, malformed selected config, and limit violations deny the complete project capture.

Persisted file trust requires a current-user-owned regular file with exact mode `0600` in a current-user-owned parent with no group/other permission bits. Insecure, replaced, or malformed stores fail closed; reads never chmod or upgrade older records. Atomic writes use a private `0600` file and create missing parents with mode `0700`.

## R6 CLI/assembly follow-up

The CLI owner should create one `DefaultTrustManager(NewFileTrustStore(path))`, pass the same instance to `LoadTrustedWithManager`, and use its `FrozenProjectSourceTrust` methods for `trust grant`. Grant must persist `cfg.CapturedProjectSourcesForGrant()` only after confirmation; it must not mint or consume an assembly capability in that invocation. Assembly should consume `UserGlobalConfig()` for user/global static MCP servers and consume project config/source bytes only with `cfg.VerifiedProjectSources()` plus `cfg.ProjectOverlayForVerifiedSources()`. Project `.go-cli/mcp.json`, hooks, skills, commands, extension files, and custom tools must never be reopened by their original path after verification. Missing or ambiguous provenance/capability fails closed for project-sourced registration. The same source view must reach prompt/serve assembly, REPL command loading, and doctor checks; those paths must not call an independent path-based config or MCP loader.

`TrustProjectSourceSnapshot` currently writes a non-expiring entry. The checker honors a persisted RFC3339 `ExpiresAt` and returns `ErrProjectSourceExpired`; a CLI expiry option would need a deliberate API extension rather than a path-only trust grant. `RevokeTrust` canonicalizes existing roots and removes canonical and caller-path aliases so a snapshot grant can be revoked through the current root spelling.
