# go-cli

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

> **Experimental Software** — This project is under active, experimental development. Due to limited time and budget, it has not been thoroughly tested. Use at your own risk. Contributions and issue reports are welcome.

A pure-Go AI Agent CLI framework. Its standalone Go executable needs no separately installed VM; configured model endpoints, MCP servers, and extension workers remain external dependencies. Dependency-inversion driven, full-chain tracing.

Inspired by and building upon [CloudWeGo Eino](https://github.com/cloudwego/eino/tree/main) and [Google ADK-Go](https://github.com/google/adk-go). Developed with [GLM 5.1](https://bigmodel.cn) and [DeepSeek-v4-flash](https://deepseek.com) (pre-20260731 release), using [Trae](https://www.trae.ai) and [Trae Work](https://www.trae.ai) — the late-June updates to Trae significantly improved stability and continuity, making sustained development sessions actually viable.

> **Related project**: [go-agent-core-v1](https://github.com/PengJunchen/go-agent-core) is the interface-driven Agent core SDK (layered L0–L4, depends on Eino for LLM adaptation). This repo (**go-cli**) is the full CLI framework with Harness→Agent→AgentLoop architecture and TUI, rather than a general importable Agent-core SDK. It does provide a separate public [extension worker API](pkg/extensionapi/v1/README.md); the project and API remain experimental.

**[中文文档](README.zh-CN.md)**

**[Iteration reports and architecture diagrams](docs/iterations/README.md)**

**[Runtime build, profile, and extension guide](docs/runtime-guide.md)**

---

## About This Project

This project has a timeline that reads like a part-time developer's diary:

- **Jan 2026** — An idea took shape. The corporate air was... unconvincing. So it waited.
- **Mar 2026** — Bought a CodePlan subscription and started hacking on weekends only. Relearned Go from scratch (promptly forgot most of it). Got a ReAct agent + Eino framework prototype working — SSE streaming, tool integration, the whole deal. All committed between midnight and Monday morning.
- **Apr 2026** — Picked up multi-agent orchestration and software engineering principles. Ran several fast iterative experiments — memory systems, context management, MCP integration, hook systems. Discovered the hard way that cramming six systems into one agent context window is a recipe for chaos. Learned that "not fully verified" is not a deployment strategy.
- **May 2026** — Weekends hijacked by work. Project flatlined.
- **Jun–Jul 2026** — Existed in a fog of existential doubt: is reinventing the wheel meaningful? Meanwhile, explored the boundaries of Loop Engineering and Graph Engineering while the industry raced ahead with SubAgent isolation, Git Worktree-based parallelism, and dynamic workflows.
- **Aug 1, 2026** — Inspired by the DeepSeek Harness evaluation, decided to stop overthinking and start building. Synthesized four months of lessons into one project: **go-cli**. The core insight crystallized into a formula: **Agent = Model + Loop + Harness**. The rest is architecture.

**Disclaimer**: I'll keep iterating, time permitting. Which is to say — no promises on cadence, but the intent is real.

**About me**: [pjcmice.com](https://www.pjcmice.com)

> Agent engineering is entering an era of close collaboration with models — the loop is no longer just infrastructure, it's the co-pilot's nervous system.

---

## Architecture Overview

```
┌──────────────────────────────────────────────────────┐
│  CLI (cmd/cli)                                       │
│  signal → config → tracing → command dispatch        │
└──────────────┬───────────────────────────────────────┘
               │
┌──────────────▼───────────────────────────────────────┐
│  Harness ── Agent ── AgentLoop (ReAct)               │
│  Submit→EventStream   Run→Result   think→act→observe │
│              │                                       │
│         TurnRunner (cancel/steer/follow-up)           │
└──────┬───────┬──────────┬──────────┬─────────────────┘
       │       │          │          │
  ┌────▼──┐ ┌──▼───┐ ┌───▼────┐ ┌──▼──────┐
  │  LLM  │ │Tools │ │Session │ │Compaction│
  │Compose│ │Reg.  │ │Tree    │ │Unified   │
  └───────┘ └──────┘ └────────┘ └──────────┘
       │
  Extension > Config > Builtin
```

### Core Layers

| Layer | Responsibility | Key Interfaces |
|---|---|---|
| **core** | Interface definitions, Agent/Harness/Loop | `Agent`, `Harness`, `AgentLoop`, `TurnRunner` |
| **llm** | LLM communication & Provider composition | `BaseChatModel`, `ModelProvider`, `ProviderComposer` |
| **tools** | Tool registration & execution | `ToolDefinition`, `ToolRegistry` |
| **session** | Session tree & persistence | `SessionStore`, `SessionTree`, `ContextManager` |
| **compaction** | Context compression | `Compactor`, `TokenEstimator` |
| **extension** | Extension lifecycle | `Extension`, `Hook`, `Middleware` |
| **production** | Production resilience | `CircuitBreaker`, `RetryPolicy`, `AuditLog`, `OutputGuard` |
| **approval** | Approval & trust | `ApprovalClassifier`, `TrustManager` |
| **config** | Config loading | `Loader`, `Config` |
| **tracing** | Full-chain tracing | `Tracer`, `TraceExporter` |
| **mcp** | MCP protocol integration | `MCPClient`, `MCPToolAdapter` |
| **acp** | Agent Communication Protocol | `ACPClient`, `StdioAdapter`, `gRPCAdapter` |
| **skill** | Skill loading & matching | `SkillDefinition`, `SkillLoader`, `SkillRegistry` |
| **tui** | Terminal UI | `App`, `Renderer`, `RendererRegistry` |
| **verify** | Verification framework | `Scanner`, `LogCapturer`, `GoLeakChecker` |

## Quick Start

### Prerequisites

- Go 1.24+

### Build & Run

```bash
go mod download
make build           # Build to bin/go-cli
./bin/go-cli         # Run
```

The same CLI entry point supports four build variants:

| Build | Go tags | UI | Native Go `.so` loader |
|---|---|---|---|
| Default | none | Full TUI | Available |
| No native plugin | `no_plugin` | Full TUI | Disabled |
| Headless | `headless` | Line-oriented | Available |
| Headless, no native plugin | `headless,no_plugin` | Line-oriented | Disabled |

`runtime.profile=edge` is a separate runtime setting, not a build tag: it enables progressive Skill and lazy MCP loading without removing packages from the binary. Selected design references include [Pi Coding Agent, OpenCode v2, DeepSeek Harness/Cordis, Magpie, and HashiCorp go-plugin](docs/runtime-guide.md#what-informed-the-design). They are research inputs, not required dependencies or evidence that every upstream feature is implemented here.

### Development Commands

```bash
make test            # Unit tests (with race detector)
make test-cov        # Coverage report
make test-log        # Log capture verification
make test-leak       # Goroutine leak detection
make scan            # AST scan (mock/hardcoded bypass)
make lint            # golangci-lint
make fmt             # gofmt + goimports
make verify          # Full verification (fmt+vet+lint+build+test+scan+log+leak)
make check           # Pre-commit check (fmt+vet+lint+build+test)
```

## Architecture Details

### Agent Execution Loop

Three-layer architecture: **Harness → Agent → AgentLoop**

- **HarnessImpl**: Async facade, `Submit()` returns `EventStream` non-blockingly, background goroutine executes agent
- **AgentImpl**: Stateful wrapper, maintains message history, thread-safe (`sync.Mutex`)
- **LoopAgent**: Pure ReAct loop (think → act → observe), injected with `BaseChatModel` + `ToolRegistry`
- **TurnRunner**: Single-turn lifecycle, supports `Cancel`/`Steer`/`FollowUp`

### LLM Provider Composition

Three-tier priority: **Extension > Config > Builtin**

| Tier | Source | Example |
|---|---|---|
| Builtin | Pre-registered | eino, openai, claude, gemini |
| Config | Config file | User-defined Providers |
| Extension | Extension registration | Plugin-injected Providers |

Name conflicts: higher-priority tier wins; within the same tier, higher `Priority` wins.

The local `EinoProvider` name identifies an OpenAI-compatible adapter implemented with the standard-library `net/http`; it does not import the CloudWeGo Eino module.

### Built-in Tools

This table describes implemented tool components; it is not the per-run model tool registry. Check `/tools` for tools registered in the current interactive session and `/help` for default capability limits.

| Category | Tool | Description |
|---|---|---|
| File/Shell | `bash` | Execute shell commands (streaming output, sandbox & resource limits) |
| File/Shell | `read` | Read text files |
| File/Shell | `write` | Write files |
| File/Shell | `edit` | Diff edit (old/new replacement) |
| File/Shell | `grep` | Regex search (pure Go mode supported) |
| File/Shell | `find`/`ls` | File search & listing |
| Git | `git_diff`/`git_status`/`git_log`/`git_blame` | Git inspection |
| Git | `git_commit`/`git_branch`/`git_create_branch`/`git_checkout` | Git branching & commits |
| Git | `git_push`/`git_pull`/`git_fetch`/`git_remote` | Git remote operations |
| Git | `git_merge`/`git_stash`/`git_stash_pop`/`git_reset`/`git_revert` | Git workflow |
| Git | `git_pr_create` | Pull request creation |
| Web | `web_fetch` | Fetch URL content |
| Web | `web_search` | Requires a configured search provider; unavailable by default |
| Web | `tool_search` | Semantic tool search |
| Productivity | `todo_write` | In-process todo list; not retained across restarts |
| Productivity | `task_create`/`task_get`/`task_list`/`task_update` | In-process task store; not retained across restarts |
| Productivity | `goal_create`/`goal_update`/`goal_list`/`goal_get` | In-process goal store; not retained across restarts |
| Code | `lsp_query` | Language Server Protocol queries |
| Multimodal | `image_read` | Not registered in the default CLI |
| Interaction | `ask_user` | Human-in-the-loop question |
| Interaction | `/plan enter`/`/plan exit` | User-controlled Plan Mode; not model-call tools |
| Agent | `dispatch_subagent` | SubAgent dispatch |
| Agent | `remote_bash` | Remote shell via SSH |

### Compaction Strategies

Auto-routing by ascending cost: **Micro → Summary → Truncating**

| Strategy | Mechanism | LLM Cost |
|---|---|---|
| Micro | Replace old tool results with short placeholders | None |
| Summary | LLM-driven semantic chunking + summarization | Yes |
| Truncating | Drop entries starting from oldest | None |

**UnifiedCompactor**: Routing layer, escalates to next strategy on failure.

**MidTurnCompact**: Overflow auto-compaction guard, triggers when token estimate exceeds threshold ratio.

### Session Management

- **SessionTree**: Core API supports branching (`Branch`), moving (`MoveTo`), and context rebuilding (`BuildContext`). The default CLI resumes only by exact physical session ID within a configured session directory; fork, branch switching, snapshot, and migration are unsupported.
- **JSONLSessionStore**: File persistence, one JSON record per line, append-only writes
- **BranchSummary**: Auto-generate summary when leaving a branch
- **Memory**: `/memory` manages user-entered records; the default CLI does not extract memories automatically or inject stored memories into model prompts.

### Extension System

Extension lifecycle: `Init` → running → `Shutdown`

Registration building blocks:
- **Hook**: Event hooks, supports `pass`/`block`/`terminate`/`replace` actions
- **Middleware**: Request/response interception, can wrap Agent and Model calls
- **Tool / Command / Provider**: Extension registration

External workers use the experimental public `pkg/extensionapi/v1` JSONL contract. It is an extension API, not a general Agent SDK or an OS sandbox; native `.so` extensions remain trusted in-process code. See the [runtime guide](docs/runtime-guide.md#know-which-extension-path-you-are-using).

### Production Resilience

| Component | Description |
|---|---|
| **CircuitBreaker** | Three-state machine (Closed → Open → HalfOpen), supports fallback |
| **RetryPolicy** | Jittered exponential backoff, error classification (Transient/RateLimit/Timeout/Fatal) |
| **IdempotentCache** | FIFO idempotent deduplication cache |
| **AuditLog** | JSONL audit log, supports time range/operation name/tool name filtering |
| **OutputGuard** | Output guard chain: regex block → PII detection → code injection detection → length truncation |

### Approval & Trust

- **ApprovalClassifier**: Tool call classification (Allow / Deny / RequireApproval)
- **ApprovalMiddleware**: Deny-first gating, denied calls never reach the executor
- **TrustManager**: Project-level trust gating, SHA-256 fingerprint + expiration

### ACP (Agent Communication Protocol)

Inter-process Agent communication protocol, two adapters:

| Adapter | Transport |
|---|---|
| **StdioAdapter** | Newline-delimited JSON (io.Pipe) |
| **gRPCAdapter** | JSON-over-HTTP |

Message types: `connect` / `disconnect` / `message` / `response` / `ack` / `error`

The `grpc` transport uses the private JSON-over-HTTP profile. Set
`acp.bearer_token_env` to the name of a dedicated environment variable to send
a bearer token to that configured HTTP endpoint. The value is read at startup,
is never stored in config, and a missing or malformed explicit reference fails
assembly before any ACP request. When the field is unset, only a credential
scoped to provider `acp`, protocol `go-cli-acp-json/1`, and the endpoint origin
is eligible. This setting does not apply to `stdio`.

```yaml
acp:
  transport: grpc
  endpoints:
    - http://127.0.0.1:8081/acp
  bearer_token_env: GO_CLI_ACP_BEARER_TOKEN
```

### MCP Tool Integration

The MCP adapters and catalog can expose remote tool schemas to the model. The durable CLI currently rejects remote `tools/call` before dispatch; a connection or `tools/list` schema is not a successful remote execution. See the [runtime guide](docs/runtime-guide.md#know-which-extension-path-you-are-using) and [MCP contract review](docs/iterations/2026-10-25-sol-runtime/mcp-contract-review.md).

Tool name normalization: `mcp__{server}__{tool}`

Supported transports: Stdio (subprocess), SSE, StreamableHTTP

### Skill System

YAML frontmatter format loading:

```yaml
---
name: my-skill
description: skill description
version: 1.0.0
category: coding
prompt: |
  You are a coding assistant.
tools:
  - bash
  - read
trigger_hint: "fix bug"
parameters:
  max_attempts: 3
---
optional body markdown
```

- **YAMLSkillLoader**: Pure stdlib parsing, supports `.md`/`.yaml`/`.yml`
- **SkillAdapter**: Maps SkillDefinition to ToolDefinition
- **Progressive disclosure matching**: Name → trigger hint → category, layer by layer

The `edge` runtime profile enables progressive Skill loading, while verified project sources are still captured for trust checks. See the [runtime guide](docs/runtime-guide.md#build-tags-and-runtime-profile-are-separate-axes).

### TUI

Bubbletea-driven render loop, dispatches by ContentType to Renderer. Supports streaming renderers (replace last frame) and non-streaming renderers (append). 26 built-in renderers.

### Full-chain Tracing

Span hierarchy throughout the execution chain:

```
cli.invocation
  └─ command.dispatch
       └─ prompt.run
            └─ harness.start
                 └─ agent.run
                      └─ loop.run
                           ├─ llm.request
                           └─ tool.call (tool_name=edit|grep|...)
```

Exporters: `stdout` / `jsonl` (file) / `otlp` (HTTP) / `kafka`

### Verification Framework

| Tool | Description |
|---|---|
| **Scanner** | AST scanning, 13 rules detecting mock imports, hardcoded credentials, test URLs, etc. |
| **LogCapturer** | Intercepts slog global handler, asserts log entries/sequences |
| **GoLeakChecker** | Goroutine leak detection, 2-second polling timeout |
| **VerifyRunner** | 36 verification rules (VQ/VT/VS/VC/VH/VP/VG/VE) |

## Configuration

### YAML Format

```yaml
provider:
  name: eino
  base_url: http://localhost:9999
  temperature: 0.7
  max_tokens: 4096
model:
  name: gpt-4
  max_tokens: 2048
tools:
  builtin:
    - bash
    - read
    - edit
    - grep
tracing:
  enabled: true
  exporter: jsonl
  level: info
compaction:
  strategy: micro_first
  max_tokens: 1000
approval:
  mode: deny_first
session:
  store: jsonl
  path: ~/.go-cli/sessions
```

### Priority

Ascending override: `Default` → `File` → `Env` → `Flag` → `Override`

## End-to-End Tests

| Phase | Verification Scope |
|---|---|
| Phase 2 | 100+ turn long conversation stability, auto-compaction, session recovery, trace chain integrity, quality metrics |
| Phase 3 | Approval gating, MCP calls, loop detection, circuit breaker degradation, retry, idempotent dedup, audit records |
| Phase 4 | Skill loading, extension registration, SubAgent generation, TUI rendering, YAML config, Provider composition, ACP communication, OTLP export |
| Phase 20+ | REPL, git tools, streaming, token estimation, interrupt/steer, system prompt, cross-scenario |
| Phase 21+ | SubAgent, TUI, production hardening, all-capabilities integration |
| Phase 22–26 | Reviewer-driven E2E, production, LSP, full wiring |
| Phase 39 | Interactive REPL sessions |
| Phase 46–49 | Security hardening & boundary edge cases |

Run:

```bash
go test -race -count=1 -v -run "TestPhase2|TestPhase3|TestPhase4" ./tests/...
```

Full E2E suite (organized under `tests/e2e_20260802/`):

```bash
go test -race -count=1 ./tests/...
```

## Project Structure

```
go-cli/
├── cmd/cli/                # CLI entry point
├── internal/
│   ├── acp/                # Agent Communication Protocol
│   ├── approval/           # Approval gating & trust management
│   ├── cli/                # CLI command parsing
│   ├── compaction/         # Context compression strategies
│   ├── config/             # Config loading (YAML/JSON)
│   ├── core/               # Core interfaces & Agent/Harness/Loop
│   ├── extension/          # Extension lifecycle
│   ├── llm/                # LLM communication & Provider composition
│   ├── mcp/                # MCP protocol integration
│   ├── mock/               # Test mocks (test-only)
│   ├── production/         # Production resilience (circuit breaker/retry/audit/output guard)
│   ├── session/            # Session tree & persistence
│   ├── skill/              # Skill loading & matching
│   ├── tools/              # Tool registration & built-in tools
│   ├── tracing/            # Full-chain tracing
│   ├── tui/                # Terminal UI
│   └── verify/             # Verification framework (Scanner/Leak/Log)
├── tests/                  # End-to-end integration tests
│   ├── e2e_20260802/       # Full E2E suite (36 files, phases 1–47)
│   ├── phase2_e2e_test.go  # Long conversation + compaction + session
│   ├── phase3_e2e_test.go  # Resilience + approval + MCP
│   ├── phase4_e2e_test.go  # Skill + extension + SubAgent + TUI + ACP
│   ├── e2e_phase20_steer_test.go ... e2e_phase49_test.go  # Phase-specific E2E
│   └── smoke_test.go       # Smoke tests
├── .github/workflows/      # CI/CD
├── Makefile                # Build/test/verify
└── go.mod                  # Go 1.24+, test dependencies only
```

## Design Principles

- **Dependency Inversion**: `core` package has zero downward dependencies, defines interfaces only; implementations are injected by the service layer
- **Zero External Runtime Dependencies**: LLM communication via stdlib `net/http`, JSON parsing via stdlib
- **Full-chain Tracing**: Every operation emits a span, trace_id spans the entire execution chain
- **Production Ready**: Circuit breaker, retry, idempotency, audit, output guard — out of the box
- **Extension First**: Hook/Middleware/Tool/Command/Provider can all be injected via extensions

## License

[MIT](LICENSE)
