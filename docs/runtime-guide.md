# Runtime builds, profiles, and extension boundaries

The CLI is a standalone Go executable and needs no separately installed VM. A configured model endpoint, MCP server, or extension worker is an external service or program with its own requirements. The project and public worker API are experimental; the `v1` protocol name is not a general stability promise.

## Choose a build

`headless` and `no_plugin` are independent Go build tags. Neither selects the runtime profile.

| Build | Tags | Terminal renderer | Native Go `.so` loader | Shared core, Skills, MCP, JSONL worker |
|---|---|---|---|---|
| Full | none | Rich TUI | Available | Yes |
| Full without native plugins | `no_plugin` | Rich TUI | Disabled | Yes |
| Headless | `headless` | Line-oriented event consumer | Available | Yes |
| Headless without native plugins | `headless,no_plugin` | Line-oriented event consumer | Disabled | Yes |

```sh
go build -o ./bin/go-cli ./cmd/cli
go build -tags no_plugin -o ./bin/go-cli-no-plugin ./cmd/cli
go build -tags headless -o ./bin/go-cli-headless ./cmd/cli
go build -tags headless,no_plugin -o ./bin/go-cli-headless-no-plugin ./cmd/cli
```

A Linux ARM64 cross-build can use the same entry point:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
  -trimpath -tags headless,no_plugin \
  -o ./bin/go-cli-linux-arm64 ./cmd/cli
```

A successful cross-build establishes compilation for that target, not execution or resource behavior on a physical device.

### Headless interaction

Headless keeps the shared REPL and Agent assembly, line-oriented multi-turn input, final responses, SIGINT cancellation, and synchronous text approval. Fullscreen layout, themes, modal approval UI, and in-flight keyboard steering are unavailable.

### Native plugins

`no_plugin` replaces the native Go shared-library loader with stubs. It leaves MCP and public JSONL subprocess workers available. Native Go plugins require platform support, run in process with host privileges, and cannot be unloaded.

The [Makefile](../Makefile) target `test-no-plugin` covers the extension and CLI packages that provide the stubs; it does not test every package or the combined headless build.

## Build tags and runtime profile are separate axes

`runtime.profile` is configuration. Both `default` and `edge` can be paired with any build above. The edge profile enables dynamic capabilities, progressive Skill loading, and lazy MCP loading. It changes loading and resource policy, not the packages linked into the binary.

```yaml
runtime:
  profile: edge
  # Optional positive overrides:
  # max_skills: 128
  # max_mcp_servers: 2
  # max_prompt_bytes: 131072
```

Zero or omitted values use edge defaults: **128 Skills, 2 MCP servers, and a 128 KiB capability prompt/schema budget**. Positive explicit limits override these defaults. The implementation applies them in [`effectiveRuntimeConfig`](../internal/cli/dynamic_capabilities.go).

| Combination | Behavior |
|---|---|
| Full + `default` | Rich TUI with regular runtime loading |
| Full + `edge` | Rich TUI with progressive Skills and lazy MCP loading |
| Headless + `default` | Line-oriented UI with regular runtime loading |
| Headless + `edge` | Line-oriented UI with progressive Skills and lazy MCP loading |

All four pairings also work with `no_plugin`.

### Progressive Skills and project trust

Skill discovery exposes metadata before full user Skill instructions are materialized on demand. Verified project sources still undergo bounded content capture for trust checks. Metadata-first discovery therefore does not mean zero Skill body reads at startup.

Project trust authorizes captured source content; it is not a blanket permission to execute every future version of files at that location. A changed source or trust grant can require reauthorization and restart.

## Know which extension path you are using

The extension paths can contribute overlapping tools or prompt behavior but have different lifecycle and trust boundaries.

| Path | Contributions | Boundary |
|---|---|---|
| File/config Skills, hooks, and prompts | Skill metadata/instructions, prompt material, and configured hooks | Project sources are verified and bounded-captured. Progressive materialization does not skip trust capture. |
| MCP | Remote tool catalogs and tool protocol adapters | Tools-only integration. Discovery and `tools/list` expose schemas; durable remote `tools/call` is currently rejected before dispatch. |
| Public `pkg/extensionapi/v1` worker | Tools, existing hook events, commands, prompt contributions, and complete-response providers over JSONL | Experimental subprocess extension API. Provider streaming returns a final response, not token deltas. RPC Skill DTO negotiation is unsupported. |
| Native Go `.so` extension | Typed, in-process Go registration | Trusted host-privilege code, platform-limited, with no unload guarantee. |

### MCP discovery and execution

MCP supports Stdio, SSE, and StreamableHTTP transports. Tool names are normalized as `mcp__{server}__{tool}`. The local `mcp_list` tool reports metadata; lazy loading can leave unselected servers disconnected.

In lazy mode, `/mcp load <server>` activates a configured server and `/mcp reload <server>` replaces its catalog at an idle boundary. `/reload` reindexes server descriptors. Successful handshake or `tools/list` is not proof of remote tool execution: the durable CLI completion preflight still rejects remote `tools/call`. This path does not provide the full MCP resources or prompts surface.

### Worker execution

The [public worker contract](../pkg/extensionapi/v1/README.md) describes initialization, frame limits, supported methods, artifact verification, deadlines, and teardown. Its public Go package imports only the standard library.

Workers share the user's OS permissions. Process isolation does not itself impose filesystem, network, CPU, or RSS limits. Hash pinning checks artifact bytes; it does not authenticate the publisher or pin separate interpreters and assets.

The host does not replay failed worker calls. A transport loss or deadline after dispatch can leave the external outcome unknown. Workers must honor deadlines and reserve stdout for protocol frames.

## Refresh and restart

For supported dynamic capabilities, the CLI prepares and validates a complete candidate at an idle boundary, then publishes it as a unit. Candidate failure retains the last valid generation. Each run uses its admitted capability view; authorization still checks current trust and policy, so an older view does not retain a revoked grant.

| Change | Apply through |
|---|---|
| Supported Skill, prompt, and hook changes | `/reload` at idle |
| Lazy MCP activation | `/mcp load <server>` |
| Lazy MCP catalog replacement | `/mcp reload <server>` at idle |
| Provider/session setup or native code | Restart |
| Runtime profile, loading mode, or active MCP limits | Restart |
| Dynamic mode with eager MCP sources | Restart |
| Active source identity or project trust-grant changes | Restart and any required trust review |

A callback that has already caused an external side effect cannot always be rolled back. Cancellation or a late result can leave its outcome `unknown`; inspect durable pending actions before continuing.

## Session and command boundaries

Persistent sessions require `session.store_path`. The default durable CLI resumes an exact physical session ID at startup with `--resume-session`; it does not expose durable fork, clone, branch switching, snapshots, or schema migration. `/session tree` and `/session pending` inspect current state; `/help` describes available reconciliation commands.

The CLI's `/memory` stores user-entered records without automatic extraction or prompt injection. Goal, task, and todo stores are process-local. Tool availability is determined by the current registry and configuration; `/tools` shows the registered set.

The `serve` command exposes the private `go-cli-acp-json/1` profile. Its `grpc` setting uses JSON-over-HTTP rather than the official ACP wire protocol. It requires an explicit `--approve auto` or `--approve deny`; remote ask approvals and remote TUI are unavailable.

## What informed the design

These references describe selected design inputs, not feature parity, performance rankings, or required dependencies.

| Project | Design input |
|---|---|
| [Pi Coding Agent](https://github.com/earendil-works/pi) | Shared Agent/session core and progressive capability exposure |
| [OpenCode v2](https://github.com/anomalyco/opencode) | Explicit dependency ownership, scoped lifecycle, and reload failure behavior |
| [DeepSeek Harness / Cordis](https://github.com/deepseek-ai/deepseek-harness) | Harness composition, service ownership, and cleanup |
| [Magpie](https://github.com/yetone/magpie) | Plugin owners/disposers and separation of in-process hooks from subprocess plugins |
| [HashiCorp go-plugin](https://github.com/hashicorp/go-plugin) | Versioned subprocess contracts, startup handshakes, and process ownership |
| [CloudWeGo Eino](https://github.com/cloudwego/eino), [Google ADK-Go](https://github.com/google/adk-go) | Agent and model composition inspiration |

The CLI uses its own JSONL worker contract. It does not import the reference projects' runtimes, and its local `EinoProvider` is a standard-library OpenAI-compatible adapter.
