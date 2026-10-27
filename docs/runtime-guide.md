# Runtime builds, profiles, and extension boundaries

This guide describes the runtime at the [PR #5 merge](https://github.com/PengJunchen/go-cli/pull/5), commit [`365f306`](https://github.com/PengJunchen/go-cli/commit/365f306bd26199b6f6276ca90b04ae04754e6a92). It separates build-time UI/plugin choices from runtime capability loading, then explains which extension paths overlap and where they differ.

The CLI is a standalone Go executable and needs no separately installed VM to run. A configured model endpoint, MCP server, or extension worker is still an external service or program with its own requirements. The project and its public extension API remain experimental; the `v1` worker protocol name is not a general stability promise.

## Choose a build

`headless` and `no_plugin` are independent Go build tags. They select implementations at build time; neither changes the runtime profile.

| Build | Tags | Terminal renderer | Native Go `.so` loader | Shared core, Skills, MCP, JSONL worker |
|---|---|---|---|---|
| Full | none | Rich TUI | Available | Yes |
| Full without native plugins | `no_plugin` | Rich TUI | Stubbed out | Yes |
| Headless | `headless` | Line-oriented event consumer | Available | Yes |
| Headless without native plugins | `headless,no_plugin` | Line-oriented event consumer | Stubbed out | Yes |

Build any of the four from the same CLI entry point:

```sh
go build -o ./bin/go-cli ./cmd/cli
go build -tags no_plugin -o ./bin/go-cli-no-plugin ./cmd/cli
go build -tags headless -o ./bin/go-cli-headless ./cmd/cli
go build -tags headless,no_plugin -o ./bin/go-cli-headless-no-plugin ./cmd/cli
```

For a portable Linux ARM64 artifact, cross-compile the same entry point:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
  -trimpath -tags headless,no_plugin \
  -o /tmp/go-cli-linux-arm64 ./cmd/cli
```

Cross-compilation confirms that the target artifact builds. It does not verify execution on an ARM64 board or its memory, latency, or device behavior. The [10/22 record](research/iteration-2026-10-22/results.md) includes host measurements and cross-build evidence; its measured process/task comparison is described below, not as an edge-device benchmark.

### What headless retains

Headless keeps the shared REPL and Agent assembly, row-oriented multi-turn input, final response, SIGINT cancellation, and synchronous text approval. It removes the rich renderer experience: fullscreen layout, themes, modal approval UI, and in-flight keyboard steering are unavailable. Headless is not a second reduced Agent implementation.

### What `no_plugin` means

`no_plugin` replaces the native Go shared-library loader with stubs. It does not remove MCP support or the public JSONL subprocess worker. The Makefile's [`test-no-plugin` target](../Makefile#L77) exercises only the `internal/extension/...` and `internal/cli/...` packages that provide those stubs; it is not a whole-repository test of the combined `headless,no_plugin` build.

## Build tags and runtime profile are separate axes

`runtime.profile` is configuration, not a build tag. `default` and `edge` can be paired with any build in the table above. The edge profile enables the dynamic capability path, progressive Skill loading, and lazy MCP loading; it does not remove linked packages or create a smaller binary. If unset, its current defaults are 128 Skills, 2 MCP servers, and a 128 KiB capability prompt/schema budget. Positive explicit limits override these defaults; zero or unset values use the profile defaults.

```yaml
runtime:
  profile: edge
  # Optional overrides: max_skills, max_mcp_servers, max_prompt_bytes
```

The implementation computes these effective settings in [`effectiveRuntimeConfig`](../internal/cli/dynamic_capabilities.go#L609). A project Skill source is still verified and captured with bounded contents for the trust check; metadata-first discovery does not mean startup reads zero Skill body bytes. Full user Skill instructions are materialized on demand.

| Combination | Meaning |
|---|---|
| Full + `default` | Rich TUI and the regular runtime loading mode |
| Full + `edge` | Rich TUI with progressive Skills and lazy MCP loading |
| Headless + `default` | Line-oriented UI with the regular runtime loading mode |
| Headless + `edge` | Line-oriented UI with progressive Skills and lazy MCP loading |

The same pairings work with `no_plugin`. Pick `headless` for a smaller terminal surface and `no_plugin` when native shared-library loading is not wanted; choose `edge` separately when runtime capability discovery and resource limits are wanted.

## Know which extension path you are using

These paths can contribute overlapping model-visible tools or prompt behavior, but they have different lifecycle, trust, and protocol boundaries.

| Path | What it contributes | What overlaps | Boundary |
|---|---|---|---|
| File/config Skills, hooks, and prompt contributions | Skill metadata and instructions, configured prompt material, and declared hook behavior | Skills can inform prompts; hooks can also be supplied by an extension worker or native extension | Project sources are verified and bounded-captured; progressive exposure/materialization does not skip that trust capture. Changes to cold runtime settings require restart. |
| MCP | Remote tool catalog and protocol calls; lazy mode can leave unselected servers disconnected | Its tool schemas can appear alongside local and extension tools in a model request | `mcp_list` is local metadata. Discovery/handshake and `tools/list` can expose a schema, but durable remote `tools/call` is currently rejected by the CLI completion preflight. A connection or schema is not proof of a successful remote call. See the [MCP contract review](iterations/2026-10-25-sol-runtime/mcp-contract-review.md). |
| Public `pkg/extensionapi/v1` worker | Tools, existing hook events, commands, prompt contributions, and complete-response providers over newline-delimited JSON | It can contribute tools, hooks, and prompt material much like an in-process extension | This is a subprocess API, not a general Agent-core SDK. Provider streaming returns one final response, not token deltas; RPC Skill DTO negotiation is unsupported. The worker shares the user's OS permissions and is not an OS sandbox. See the [worker contract](../pkg/extensionapi/v1/README.md). |
| Native Go `.so` extension | In-process typed Go registration | It may also add tools, hooks, commands, or providers | It runs with host privileges, is available only where Go's plugin mechanism is supported, and cannot be unloaded. Disabling it changes no MCP or worker semantics. |

The public worker package is standard-library-only, but its API and the overall project are still experimental. Hash pinning checks artifact bytes; it does not authenticate a publisher. Process isolation alone does not impose filesystem, network, CPU, or RSS limits.

### Reload and authorization

For supported dynamic capabilities, the CLI prepares and validates a candidate at an idle boundary, then publishes it as a unit. A failed candidate keeps the last valid generation. Each run uses its admitted capability view; authorization is checked against current trust/policy, so an older view does not preserve a revoked grant. A callback that has already caused an external side effect cannot always be rolled back; cancellation or a late result can therefore remain `unknown`.

Changes to provider/session setup, native code, runtime profile, loading mode, and active MCP limits are not general hot-reload operations. They require a restart. The [10/25 runtime verification](iterations/2026-10-25-sol-runtime/verification.md) records the source freeze and Go gates; the [real Sol report](iterations/2026-10-25-sol-live-e2e/report.md) records the separate UI scenarios and their unsupported/negative outcomes.

## What informed the design

The comparisons below refer to the frozen upstream source versions in the linked research, not to later upstream releases. They describe selected design inputs, not performance rankings or wholesale code reuse.

| Project and frozen source | What was studied | Applied here | Not adopted or not established |
|---|---|---|---|
| [Pi Coding Agent, `bd3c9b8`](https://github.com/earendil-works/pi/tree/bd3c9b823762b7383954aa49e13ee350471015fe) | Shared agent/session across CLI, print, RPC, and SDK; Skills and tool exposure; evaluation protocol | Separate Skill discovery from body materialization and MCP connection loading from schema exposure; use fixed evidence and honest missing-case accounting | No Pi API or runtime was copied. Pi's deferred tool exposure does not prove lazy server connections; the same API/features or performance are not claimed. |
| [OpenCode v2, `4617210`](https://github.com/anomalyco/opencode/tree/4617210822bdbb31e5749751afbfb4584ccedac8) | Typed dependency graph, Scope ownership, per-location lifecycle, reload failure behavior | Lifecycle ownership and candidate-failure boundaries informed the Go generation work | No Effect runtime or TypeScript service graph was ported. OpenCode's generation integer is not represented as proof of this CLI's leases or atomicity. |
| [DeepSeek Harness/Cordis, `687f282`](https://github.com/deepseek-ai/deepseek-harness/tree/687f282e8625c6b8d11ff58211a90524be2e4317) | Service dependencies and Fiber/effect cleanup | Ownership and cleanup are design inputs | Cordis is not reused in this Go CLI. The official source reviewed does not establish a DeepSeek project named “Honeycomb.” |
| [Magpie, `d783df4`](https://github.com/yetone/magpie/tree/d783df46cbac9021e26a6bd71ca0b55544552eb2) | Plugin owners/disposers and separation of in-process hooks from subprocess plugins | Helped frame extension placement by workload and lifecycle | No Magpie runtime, JavaScript engine, or claimed microsecond performance was adopted. |
| [HashiCorp go-plugin, `fa60f8a`](https://github.com/hashicorp/go-plugin/tree/fa60f8a91608db94647a9dc739b3a33634d35163) | Versioned subprocess RPC, startup handshake, and process ownership | The need for an explicit worker protocol and lifecycle informed the public worker design | The CLI uses its own JSONL contract, not go-plugin's RPC implementation; neither protocol is itself an OS sandbox. |
| [wazero, `e234f6f`](https://github.com/tetratelabs/wazero/tree/e234f6fe6ecd4589dd0c643b641bd38f6d826ee5) and [Extism Go SDK, `de6f8ac`](https://github.com/extism/go-sdk/tree/de6f8ac95721dff6cf8f5fdca26887699a53b3e9) | Optional WASM runtime controls and a cross-language ABI candidate | Kept as future options for a concrete use case and adoption review | Neither is a default dependency or a current extension backend. |
| [CloudWeGo Eino](https://github.com/cloudwego/eino) and [Google ADK-Go](https://github.com/google/adk-go) | Project-level inspiration already named in the root README | The README retains this background attribution | They were not pinned as part of the 10/18 or 10/22 architecture/source comparison. The local `EinoProvider` is an OpenAI-compatible `net/http` adapter and does not import the Eino module. |

The detailed, commit-pinned evidence is in [reference projects](research/architecture-2026-10-18/reference-projects.md), [OpenCode v2](research/architecture-2026-10-18/opencode-v2.md), and [the 10/22 upstream mapping](research/iteration-2026-10-22/upstream.md). Those reports explicitly avoid deriving Rust parity from language reputation, atomic pointer costs, or unrelated framework benchmarks.

## Verification and evidence limits

- The 10/22 results include a limited five-arm process/task comparison of Go, official PI 1.3.0, and OpenCode 2.0.32 on one host with a fixed scripted provider and task. Tool sets/schema and OpenCode startup differ, so this does not establish overall product rankings, feature parity, or real-model quality parity. The Go arms use the default profile, not edge; Rust was not included in the comparison.
- The 10/25 `make verify` and E2E counts apply to their recorded Go source freeze. The real Sol report has its own final candidate, passed journeys, safety rejections, and unmet expectations; it does not turn a rejected MCP preflight into a remote tool success.
- The later `--help` observations in the verification record measure help startup only. They are not interactive, model, or edge-board latency.
- Linux/ARM64 cross-builds are not physical-device tests. The reviewed work did not establish full-load capability limits at 0/100/1000 Skills or MCP servers, Rust performance parity, or real-model quality parity with Pi.

For the PR #5 merge and its permanent CI records, see the [iteration index](iterations/README.md).
