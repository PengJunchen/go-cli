# go-cli

A Go AI Agent CLI built around **Harness → Agent → AgentLoop**, with model and tool integration, persistent sessions, context management, and terminal interfaces. The standalone executable needs no separately installed VM; model endpoints, MCP servers, and extension workers have their own requirements.

The project and its public extension API are experimental.

[中文文档](README.zh-CN.md) · [Runtime guide](docs/runtime-guide.md) · [Extension worker API](pkg/extensionapi/v1/README.md)

## About This Project

The architecture has evolved through three stages:

1. **Core CLI:** Harness, Agent, and AgentLoop connect models, tools, sessions, and context compaction through explicit interfaces.
2. **Reliable execution:** approval and project trust, durable sessions, cancellation and resource ownership, tracing, and the TUI make execution inspectable and controllable.
3. **Dynamic, lightweight extensions:** progressive Skills, lazy MCP discovery, refresh at idle boundaries, and a public JSONL worker API work across full, headless, and `no_plugin` builds. The separate `edge` profile applies capability limits and lazy loading.

[go-agent-core-v1](https://github.com/PengJunchen/go-agent-core) is the related interface-driven Agent core SDK. This repository is the complete CLI framework; its public worker API serves subprocess extensions rather than general Agent-core embedding.

Author: [pjcmice.com](https://www.pjcmice.com)

## Quick start

Requires Go 1.24+; the exact minimum version is declared in [go.mod](go.mod).

```sh
go mod download
make build
./bin/go-cli --help
```

Configure a model endpoint before sending a prompt. For an OpenAI-compatible endpoint:

```sh
export GO_CLI_PROVIDER_NAME=eino
export GO_CLI_PROVIDER_BASE_URL=https://api.openai.com/v1
export GO_CLI_PROVIDER_API_KEY="your-api-key"
export GO_CLI_MODEL_NAME=your-model-id

./bin/go-cli prompt "Explain the structure of this repository"
./bin/go-cli interactive
```

The local `EinoProvider` uses the standard-library `net/http` client for OpenAI-compatible requests; it does not import the Eino framework.

The CLI loads user/global configuration and environment settings as its base. It merges captured project configuration only after the project source manifest passes trust verification; without authorization, it continues with the user/global configuration alone.

A user configuration at `~/.go-cli/config.yaml` can enable persistent sessions:

```yaml
provider:
  name: eino
  base_url: https://api.openai.com/v1
model:
  name: your-model-id
session:
  store_path: .go-cli/sessions
tracing:
  enabled: true
  exporter: jsonl
```

Keep API keys in environment variables or configured credential storage.

## Build variants

`headless` and `no_plugin` are independent build tags:

| Build | Go tags | Terminal UI | Native Go `.so` loader |
|---|---|---|---|
| Full | none | Rich TUI | Available |
| Full without native plugins | `no_plugin` | Rich TUI | Disabled |
| Headless | `headless` | Line-oriented | Available |
| Headless without native plugins | `headless,no_plugin` | Line-oriented | Disabled |

```sh
go build -o ./bin/go-cli ./cmd/cli
go build -tags no_plugin -o ./bin/go-cli-no-plugin ./cmd/cli
go build -tags headless -o ./bin/go-cli-headless ./cmd/cli
go build -tags headless,no_plugin -o ./bin/go-cli-headless-no-plugin ./cmd/cli
```

All variants share the Agent core, Skills, MCP integration, and JSONL workers. Headless retains multi-turn input, final responses, SIGINT cancellation, and synchronous text approval. Fullscreen rendering, themes, modal approval, and in-flight keyboard steering require the rich UI.

### Edge profile

The runtime profile is configured separately from build tags and works with all four builds:

```yaml
runtime:
  profile: edge
  # Optional positive overrides:
  # max_skills: 128
  # max_mcp_servers: 2
  # max_prompt_bytes: 131072
```

`edge` enables dynamic capabilities, progressive Skills, and lazy MCP loading. Zero or omitted limits use defaults of **128 Skills, 2 MCP servers, and 128 KiB** of capability prompt/schema budget. It changes loading and resource limits, not which packages are linked into the binary.

Verified project Skill sources still undergo bounded content capture for trust checks. Metadata-first discovery does not imply zero Skill body reads. See the [runtime guide](docs/runtime-guide.md#build-tags-and-runtime-profile-are-separate-axes).

## Common commands

```sh
./bin/go-cli --help
./bin/go-cli --version
./bin/go-cli doctor
./bin/go-cli models
./bin/go-cli prompt --output json "Describe this project"
./bin/go-cli interactive
./bin/go-cli interactive --resume-session <physical-session-id>
```

In an interactive session:

| Command | Purpose |
|---|---|
| `/help` | Show commands and current capability boundaries |
| `/tools` | List tools registered for the current session |
| `/reload` | Refresh supported Skill, prompt, and hook plans at idle |
| `/mcp list` | Inspect configured MCP servers |
| `/mcp load <server>` | Activate a server in lazy mode |
| `/mcp reload <server>` | Replace a server catalog at idle in lazy mode |
| `/session tree`, `/session pending` | Inspect the durable session and unresolved actions |
| `/plan enter`, `/plan exit` | Change Plan Mode explicitly |

Durable session storage requires `session.store_path`; startup resume uses an exact physical session ID. Durable fork, clone, branch switching, snapshot, and schema migration are unsupported. `/memory` manages user-entered records; stored memories are not automatically extracted or injected into prompts. Goal, task, and todo state lasts only for the current process.

## Architecture and extension boundaries

```text
CLI: configuration → tracing → command dispatch
                       ↓
                Harness → Agent → AgentLoop
                           ↓
                   TurnRunner lifecycle
                       ↓
          Models · Tools · Sessions · Compaction
                       ↓
          Trust · Approval · Tracing · Extensions
```

- **Harness** exposes asynchronous runs through an event stream; **Agent** owns conversation state; **AgentLoop** performs think → act → observe. **TurnRunner** manages cancellation, steering, and follow-up input.
- **Models and tools** use injectable interfaces. Provider selection follows Extension → Config → Builtin priority. Available tools depend on the current session registry and configuration.
- **Sessions and context** provide JSONL persistence, context rebuilding, and Micro → Summary → Truncating compaction. The CLI exposes a narrower durable workflow than the internal session tree API.
- **Execution controls** include approval, project trust, retries, circuit breaking, auditing, output guards, and tracing across command, model, and tool calls.

Supported dynamic changes are prepared and validated before publication at an idle boundary. A failed candidate retains the last valid generation. Provider/session setup, native code, runtime profile, loading mode, active MCP limits, and certain trust/source changes require restart.

MCP integration is **tools-only**, with Stdio, SSE, and StreamableHTTP adapters. Discovery and `tools/list` can expose remote schemas, but the durable CLI currently rejects remote `tools/call` before dispatch. A connected server or visible schema does not establish remote execution support.

The experimental [JSONL extension worker API](pkg/extensionapi/v1/README.md) supports tools, hooks, commands, prompt contributions, and complete-response providers. Provider streaming emits one final response rather than token deltas. Workers share the user's OS permissions; subprocess isolation is not an OS sandbox. Native `.so` extensions run in process and cannot be unloaded.

See the [runtime guide](docs/runtime-guide.md#know-which-extension-path-you-are-using) for trust, refresh, and protocol boundaries.

## Development

```sh
make test           # Automated tests with race detection
make test-e2e       # Tests carrying the e2e build tag
make test-no-plugin # Native-loader stub coverage in extension/CLI packages
make test-cov       # Coverage report
make scan           # AST rules
make lint           # golangci-lint
make fmt            # gofmt and goimports
make check          # Formatting, vet, lint, build, tests
make verify         # check plus AST, log, and goroutine checks
```

Production code lives in `cmd/`, `internal/`, and `pkg/`. Automated integration tests and their required fixtures live in `tests/`; build and verification tasks are defined in [Makefile](Makefile). The [documentation index](docs/README.md) links current product and API guides.

## Design references

These projects informed selected design choices and are not required runtime dependencies:

| Project | Design input |
|---|---|
| [Pi Coding Agent](https://github.com/earendil-works/pi) | Shared Agent/session core and progressive capability exposure |
| [OpenCode v2](https://github.com/anomalyco/opencode) | Explicit service ownership, lifecycle scopes, and reload failure boundaries |
| [DeepSeek Harness / Cordis](https://github.com/deepseek-ai/deepseek-harness) | Harness composition, dependency ownership, and cleanup |
| [Magpie](https://github.com/yetone/magpie) | Plugin ownership and separation of hooks from subprocess extensions |
| [HashiCorp go-plugin](https://github.com/hashicorp/go-plugin) | Versioned subprocess contracts, handshakes, and process ownership |
| [CloudWeGo Eino](https://github.com/cloudwego/eino), [Google ADK-Go](https://github.com/google/adk-go) | Agent and model composition inspiration |

## License

MIT.
