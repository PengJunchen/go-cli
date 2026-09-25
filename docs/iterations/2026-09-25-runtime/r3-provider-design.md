# R3 provider capability and request integrity design

状态：R3-DES-01 设计提案；resolver 全消费者、run-owned typed retry facts/统一 dispatch budget、legacy unscoped credential 规则已按 review blocker 收敛，等待 R3 独立设计复核后再决定 builder handoff。R1-REV-01 已完成；R2 设计在 `4f8d1a0d72eebc25fcf06c1fcb897d64bf73d41c` 获批，但 R2-BLD 实现与 R2-REV-01 restart-safety 验收仍是后续 R3-BLD 的门禁。本文不修改产品代码、session ledger 或任务台账，也不承诺增加 provider/protocol 支持。

设计依据：`docs/audits/2026-09-24/02-llm-production.md`、`docs/audits/2026-09-24/proposed-tasks.json` 中 R3-DES-01 与 R3-BLD-01..03、当前 `execution-tasks.json`、`r0-contract.md`、`lane-provider.md`、`r1-contract.md`、`r2-contract.md` 和当前源码。R1 source/review gate 已通过；R2 design 已批准，但 R2-BLD 实现和 R2-REV-01 restart-safety review 未完成。因此本文仍是 R3 proposal，不代表 R3 可实施或运行时正确。

## 决策摘要

1. **唯一解析面是 provider-scoped request resolver。** `ProviderSpec` 决定规范 provider identity、alias、wire protocol、endpoint/auth policy、adapter factory 和协议能力状态；`ModelSpec` 决定 `(ProviderID, ModelID)` 的模型级能力与带来源的上下文、输出和定价元数据。配置文件、环境变量、CLI 参数、small model、model cycling、模型切换和 subagent 都先解析成同一个 `ModelRequest`，再由同一 resolver 生成 `ResolvedModel`。调用者不能各自按字符串猜 endpoint、凭据或协议。
2. **R0 默认 fail-closed 保持不变。** 当前 CLI 可用路径是 Eino 的 OpenAI-compatible wire protocol；显式 `BaseURL` 的自定义 provider 名称继续表示 OpenAI-compatible endpoint。`claude`、`anthropic`、`gemini` 仍在 resolver 中明确禁用。只有逐协议的本地 HTTP wire fixture 全部通过、R1/R2 接口已审定且产品明确指定支持的 provider/model 版本后，才可通过独立 reviewed registration change 启用；测试不能在运行时自动打开协议。
3. **凭据绑定 provider、协议和 origin。** 主模型、small model、cycler candidate 与 subagent override 是独立的请求身份。R0 已建立的 small-model 继承规则继续有效：只有规范 provider identity、协议和有效 origin 都相同才可继承 primary key；跨 provider、跨协议或跨 origin 不继承。`CredentialRef` 和任何 merged secret 均绑定这三项，provider override 不能把 primary secret 重新解释为另一个 provider/protocol/origin 的凭据。Onboarding 的 provider 无关单 key 不能被解释成可供任意 provider 使用的凭据。
4. **成本按真实 provider attempt 记一次。** 一次逻辑 run 可以产生初次调用、重试、cycler/fallback 调用和 continuation；每个真正派发到 provider 的调用独立记录 provider、model、attempt ID、实际 usage 和可得成本。缺少 usage 或价格时显示未知/估算，不能伪报为零或实际计量。此统计不是资金账本，也不构成默认硬预算。
5. **重放受仍由 Loop owner 持有的 typed attempt facts 约束。** context cancel 不重试；已显示的部分内容不透明重放；工具已派发或 receipt outcome unknown 时不重放工具或整轮。只有 typed transient provider failure 且当前 Loop/Run owner 证明旧 stream 已 quiescent、本次 attempt 没有发布 candidate、没有工具派发、没有 partial-result durable acknowledgment 时，才允许在同一逻辑 operation 的共享 `MaxAttempts` 内重试。R2 已提交的本轮 user input 和 compaction context 仅可供这个仍存活的 run 复用；R2 crash recovery 永不自动重新派发旧 run 的 provider request。

## 当前切口与已知分歧

| 当前路径 | 当前行为与缺口 | 设计约束 |
|---|---|---|
| 名称解析 | `prompt.go:311-338` 中 provider/model flag 覆盖 config；model 依次取 `Provider.Model`、`Model.Name`、默认 `gpt-4o-mini`，provider 默认 `eino`。 | 保留既有优先级，但只由一个请求解析器规范化一次；不得在下游再次把 model 名当 provider 名。 |
| provider 构造 | `assemble.go:1480-1555` 的 `buildModel` 和 `resolveCLIProvider` 是 CLI 实际入口；`ProviderRegistry` (`internal/llm/registry.go:19-45`) 新建时只注册 Eino。 | CLI、model list、small model、cycler、selector 和 subagent override 使用同一个 resolver/catalog 实例或同一不可变 spec 集；不新造 Runtime。 |
| composer | `DefaultProviderComposer` (`internal/llm/composer.go:218-224`) 另外列出 Eino、OpenAI、Claude、Gemini，但 CLI 没有使用它；`selectWinningProviders` (`:299-331`) 从 map 生成 winner，存在同优先级排序不确定性。 | Composer 只能提供 descriptor/spec，不能自行扩大已启用能力；同名冲突按稳定规则解决，协议 identity 不能被 config/extension 偷换。 |
| model metadata | provider `Models()` 与可选 `ModelsDevRegistry` 并存；models.dev 支持 context、pricing、modality 等元数据 (`model_registry.go:15-29`, `modelsdev_client.go:112-129`)。它不是 adapter、协议或能力启用来源。 | 通过一个 `ModelCatalog.Resolve(providerID, modelID)` 返回规范 `ModelSpec`。外部 registry 只补充标注来源/时间的 metadata，不能启用 wire capability 或覆盖协议。未知模型 metadata 保持 unknown，不把任意模型名伪装成已认证能力。 |
| assembly entrypoints | `prompt.go:122-140`、`serve_cmd.go:164-193` 和 `repl_session.go:228-274` 都通过 `AssembleAgent`；但 cycler 在 `assemble.go:679-695` 创建只有 Eino 的新 registry，subagent 在 `:1190-1195` 也创建新 registry。 | 从共同 assembly 注入同一个 resolver。所有 entrypoint 测试要验证真实 HTTP endpoint/header/body，而非只验证 helper 输出。 |
| model cycling | `ModelEntry` 只含 Provider/Model/Weight/TaskType；`cycledModel.buildSelectedModel` 只传 model 名，build/error 会静默 fallback primary (`internal/llm/cycler.go:229-265`)。 | Candidate 必须是完整 provider-scoped request（引用 scoped credential，不复制明文 key）；selection/fallback policy 显式，实际被调用的模型/attempt 要可观察。 |
| subagent | 默认继承 parent model；override 在 `real_subagent_runner.go:79-89` 通过 `GetModel(ctx, r.modelName, ModelConfig{Model:r.modelName})` 解析，缺少显式 provider 与 origin scope；assembly 传入的 registry仅有 Eino (`assemble.go:1193-1195`)。 | 默认仍使用 parent resolved model；override 表示完整的 provider/model request，并由 assembly 授予精确的 credential scope；解析失败显式返回，不静默切换到另一个 provider。 |
| 凭据链/onboarding | config loader 为 Default < File < Env < Flag < Override (`config/loader.go:32-34,118-145`)；`ProviderConfig.APIKey` 是普通字段。`AuthChain` 有另一优先链但生产 CLI 无调用点；keychain 用全局 `go-cli/api-key`，`auth.json` 兼容单个无 provider 的 `{"api_key":...}` (`config/auth_chain.go:14-16,45-59`, `keychain.go:22-29,88-93`)。Onboarding 只收一个 key，保存在内存 config 并写到通用 keychain 或单 key auth.json (`cli/onboarding.go:157-199,234-261,359-485`)。 | 把凭据解析整合到 resolver 的一个显式 precedence；每次查找必须使用规范 ProviderID，禁止全局 key 被另一个 provider 名命中。Onboarding 要明确 provider identity 后存 scoped credential；既有无 scope secret 不得自动复制给多个 provider。 |
| sampling | `buildModel` 只传 `Model` (`assemble.go:1483-1498`)，丢失 `ProviderConfig`/`ModelConfig` 的 Temperature 与 MaxTokens；small model 会传自己的 sampling (`:1561-1569`)。 | 对所有调用统一 `per-call explicit option > model setting > provider setting > adapter default`。显式 per-call 零值有效；配置层也必须保留“未设置”和“显式 0”的区别，不能只看当前 `float64/int` 零值。 |

## Resolver delivery and failure contract

Each assembly owns one immutable provider/model spec snapshot and passes the same resolver authority to every actual model construction path. Keep the existing request boundary small; do not introduce a second Runtime or a parallel run controller:

```go
type ModelResolver interface {
    ResolveModel(context.Context, ModelRequest) (ResolvedModel, error)
}
```

Every consumer passes a complete `ModelRequest` (canonical provider identity, model identity, endpoint override/origin, scoped `CredentialRef`, sampling, and purpose), not a model-name string. `ResolvedModel` is immutable for that request and carries the validated protocol/origin/capability snapshot. The resolver checks the enabled-protocol gate before reading any credential source or constructing an adapter. Raw secret material never enters the request, spec snapshot, trace, or model-list data.

| Consumer | Request source and required behavior |
|---|---|
| Primary model | Resolve the configured provider/model request once; provider dispatch uses that resolved snapshot. |
| Small/summarizer model | Use the same resolver. Apply the existing R0 same-provider/protocol/origin inheritance rule only when the small-model request is otherwise unspecified; a cross-scope request must resolve its own scoped credential. |
| Model switch / selector | Carry and resolve a full provider-scoped request; a name-only switch cannot inherit the current credential or silently change protocol. |
| Cycler | Store full candidate requests and resolve each through the shared resolver; do not construct a fresh Eino-only registry or turn a candidate build failure into an implicit primary dispatch. |
| Subagent | Default to the parent's already-resolved model; an override carries a full request through the same resolver and can use only the matching scoped credential. |
| Model list / doctor / composer | Read the shared immutable catalog/spec snapshot. Composer or extensions may describe candidates but cannot enable a protocol or construct a different adapter. |

The shared authority is injected from assembly through every consumer that can actually construct or dispatch a model. Cycler and subagent code must not instantiate a private registry/resolver; model switch/select flows must resolve a full request before replacing the active model. Candidate failure has no implicit unsupported/unresolved-to-primary fallback: only a separately explicit, observable fallback policy may select another candidate, and it uses the same resolver and operation attempt budget. A fallback candidate that is disabled, malformed, unresolved, or lacks a matching scoped credential fails before secret lookup/provider dispatch for that candidate; it never becomes a reason to reinterpret credentials or hide an earlier unsupported-provider error.

Fallback behavior is explicit and observable. Unsupported/native-disabled identity, unresolved candidate, malformed request, or missing candidate-scoped credential returns a typed configuration/resolution error before provider dispatch; none may silently select the primary. Auth/protocol/schema errors are not candidate fallback signals. A transient dispatched failure may move to another explicitly listed candidate only when Loop-owned attempt facts prove retry is safe; it consumes the same operation budget and reports both the failed and selected provider/model. Returning to primary is subject to the same explicit policy and budget. If no candidate is explicitly authorized, return the original terminal error. Never hide an unsupported-provider error behind a successful primary response.

Executable acceptance must drive the actual assembly paths through local HTTP fixtures: primary, small model, selector/switch, cycler candidate, and subagent override each prove provider ID, protocol, origin, model/body, and credential header. Disabled native paths in each consumer assert zero secret lookup and zero HTTP request. Add regressions for unresolved/missing-credential candidates and for no silent primary fallback; a safe transient fallback records both dispatch attempts and observes the shared `MaxAttempts` cap. Exercise small/summarizer construction through the same injected resolver and assert a malformed/disabled/missing-scope request makes zero provider requests and cannot silently fall back to primary.

R0 lane 已验证 native identity 即使带 `BaseURL` 也在构造/HTTP 前失败；unknown name + explicit URL 仍是用户明确指定的 OpenAI-compatible custom endpoint。维持这一安全边界。`openai` 与 `eino` 虽使用同一类 OpenAI-compatible adapter，也不是自动等价的凭据身份；只允许 descriptor 明确列出的真实 alias 共享 provider scope。

## 解析契约：ProviderSpec、ModelSpec 与 ModelRequest

可用现有 `ModelProvider`、`ProviderRegistry`、`ProviderComposer` 和 `ModelRegistry` 最小演进；建议把不可变规范信息集中在 `internal/llm`，而不是新增一层 Runtime。字段名为设计表达，不要求逐字照搬：

```go
type ProviderSpec struct {
    ID              ProviderID
    Aliases         []string
    Protocol        ProtocolID
    Adapter         AdapterKind
    DefaultOrigin   *url.URL
    Auth            AuthPolicy
    Enabled         bool
    Capabilities    WireCapabilities
}

type ModelSpec struct {
    ProviderID      ProviderID
    ID              ModelID
    Capabilities    ModelCapabilities
    ContextLimit    Sourced[int]
    OutputLimit     Sourced[int]
    Pricing         Sourced[Price]
}

type ModelRequest struct {
    ProviderID      string
    ModelID         string
    Endpoint        EndpointOverride
    Credential      CredentialRef // bound to ProviderID + ProtocolID + normalized origin; never loggable/raw model-list data
    Sampling        SamplingConfig
    Purpose         TaskType
}
```

`ProviderSpec` 是下列内容的唯一权威：provider identity/严格 alias、协议族、adapter、默认 origin、认证形态和“可否构造”的门禁。`ModelSpec` 是 provider-scoped model identity 及模型限制/价格/能力的统一查询结果。`ModelCatalog` 可从 provider 内置列表与可选 models.dev enrichment 构成，但对外只暴露一次合并解析；metadata 带来源、采集时间、known/unknown。Provider-level wire capability 不能由 models.dev、任意配置名或某个模型字符串自行推断。

`ModelRequest` 经 `ResolveModel(ctx, request)` 后返回的 `ResolvedModel` 至少包含 canonical provider/model、protocol、验证后的 origin、credential binding/source、sampling defaults、provider/model capability snapshot 和模型 metadata provenance。CLI model list、doctor、build/switch、small/summarizer model、cycler 与 subagent 都从同一个 assembly 注入的不可变 resolver/spec snapshot 解析完整 request。先根据 provider identity 和 protocol gate 决定是否支持，之后才访问 credential source；disabled/unknown protocol 的失败必须发生在取 key、构造 adapter、发送 prompt 前。

配置解析维持 Loader 的层优先级；命令行 provider/model override 仍优先于 config，model 选择保持 `Provider.Model` 优先于兼容字段 `Model.Name`。凭据查找必须形成同一张可测试优先级表：明确且完整 scoped 的请求/provider config 凭据优先于匹配 provider/protocol/origin 的 keychain、auth file 和 provider-specific environment entry；legacy models.json 只能按 canonical ID 查找。全局 keychain 项 `go-cli/api-key` 与无 provider 的 `auth.json {"api_key":...}` 是 unscoped legacy credential，resolver 永不把它绑定到当前 default，也不把它作为 primary/candidate 的 fallback。仅有这些值时 fail closed 并提示用户迁移到显式 provider/protocol/origin scoped 配置；如果另有合法 scoped source，则仍按既定 precedence 使用该 source。显式 provider 块中的 key 只属于该块可证明的 provider/protocol/origin scope；provider 或 endpoint override 不能重解释该 key。密钥不得出现在 trace、日志、候选 model entry 或 `ModelSpec`。

### provider 与 protocol 矩阵

| 用户选择 | 规范 protocol/adapter | 当前 CLI 状态 | 允许的解析行为 |
|---|---|---|---|
| `eino` | OpenAI-compatible / Eino | 默认 active | 无 URL 时使用 Eino 默认 OpenAI-compatible origin；key 用 Bearer auth。 |
| `openai` | OpenAI-compatible / Eino-compatible path | R0 仅显式 config key 或 URL 可路由 | 无 URL 且无 key 不能假装由 fresh registry 构造 `openai`；名字和凭据 scope 保持 `openai`。 |
| 用户自定义名称 + 明确 `BaseURL` | OpenAI-compatible / Eino | active custom-compatible | URL 是用户显式声明的协议边界；key 只送该 resolved origin，不推断 native 协议。 |
| `claude` / `anthropic` | Anthropic Messages native | disabled | 构造前返回可诊断 unsupported-protocol error；即使提供 URL/key 也不 fallback 到 Eino。 |
| `gemini` | Gemini generateContent native | disabled | 构造前返回可诊断 unsupported-protocol error；即使提供 URL/key 也不 fallback 到 Eino。 |
| 未知 ID，无明确 URL | 未知 | disabled | fail closed；未知 ID + key 但无 URL 也拒绝。 |

该表仅承诺目前 R0 已允许的 OpenAI-compatible 路由与 native 默认 fail-closed。它不是对 Claude/Gemini 产品支持范围的决定，也不把有 `NewClaudeProvider`/`NewGeminiProvider` 构造函数等同于可用支持。

## Native adapter 的启用门槛

`internal/llm/native.go` 已有独立的 Claude/Gemini encoder/decoder，但当前测试/源码并不能证明完整 runtime wire contract。审计确认：Claude request 没有 `tools`，普通 decoder 忽略 `tool_use`，历史中的 tool/tool_result 角色没有正确映射；Gemini request 没发 `tools`，`system` 被合并为普通 user content，且两种 native adapter 都会忽略非 data-URI image URL。对应审计 finding 为 LLM-02、LLM-07、LLM-12。R1 的 stream parser/read/timeout 终态缺陷是额外前置条件。

每个 protocol 必须单独满足下列 wire fixtures；不能用 encoder 单测、编译通过、某一个 text response 或 “native provider class exists” 替代：

1. `httptest.Server` 接收真实 adapter HTTP 请求，断言 URL/path、HTTP method、必须的版本/content headers、认证 header/query location；canary secret 只在预期 origin 出现，错误 origin、请求日志和报错均无 secret。
2. 断言完整 request JSON：模型 ID、system instruction 的专用位置、temperature/max tokens、所有提供的 tool schema、图片输入。无法支持的 image/content 必须返回 typed unsupported error，不能静默删除。
3. 返回带文本、一个以上 tool call、tool result 再续问的 fixture；检查请求编码、provider response 解码和重建后的 `ToolCallID`/name/args 对应。Native 开启前必须以 Core 的真实工具闭环验证一次 serial 和并发情况（由 R1/R2 已定义的约束裁定）。
4. 固定 finish reason 的 normalization（stop/length/tool-calls/content-filter 或该协议等价值）、成功/失败 usage mapping、SSE 中途坏 JSON/read error/EOF-without-terminal。stream 必须遵守 R1 唯一 terminal outcome；错误不能伪装为成功或可执行的不完整工具参数。
5. 同样的协议 fixture 至少覆盖公开支持矩阵内每个 provider major wire version；fixture 的 provider/model/version 支持声明由产品 owner 明确审核。

只有 protocol fixture 全绿且相应 R1/R2 合同已集成并通过独立验收后，提交一个独立的 registration change 把该 protocol 从 disabled 改为 enabled；同时更新 provider support matrix 并由 R3 reviewer 审查。CI 测试不会通过运行时反射、外部 metadata 或 config 开关自动启用协议。任何 fixture 缺失、版本未知、stream terminal 未收敛、unsupported content 静默丢失或 credential scope 不明，都保持 disabled。

## 主模型、小模型与凭据范围

Canonical credential scope 至少由 `(canonical ProviderID, protocol, normalized origin)` 组成。origin 比较使用 scheme、host、effective port；路径不改变 origin，但协议 endpoint path 仍由 ProviderSpec/用户显式 BaseURL 校验。`CredentialRef`、merged secret 与 resolver 输出的 credential binding 都携带并校验完整 scope。对 OpenAI-compatible custom alias，只有用户提供 BaseURL 才能路由；native provider 的 URL override 不会让 disabled protocol 变 enabled。切换 provider/protocol/origin 必须重新解析对应 scoped credential，绝不把 primary secret 以新 identity 再解释。

| 场景 | 是否可用 primary credential | 行为 |
|---|---:|---|
| Primary 本身 | 是 | 先校验 provider/protocol，再从该 ProviderID scope 解析凭据并绑定到 resolved origin。 |
| Small model 未指定 provider/endpoint/key | 是，按 R0 规则 | 继承 primary provider/endpoint/key；记录它实际使用的独立 model/attempt identity。 |
| Small model provider 与 primary 不同 | 否 | 只解析 small provider 自己的 scoped key；缺少必需 key 则报配置错误。 |
| Small model endpoint 不同 origin | 否 | 即使 provider 名相同也不可把 primary key 送到新 origin；small model 必须自己的 key 或 descriptor 明确支持匿名。 |
| Small model endpoint 同 origin、路径变化 | 可按 R0 规则 | provider identity 仍相同、origin 相同才允许继承；必须断言 key 只进入该 origin。 |
| Cycler candidate | 默认否 | Candidate 存 `CredentialRef`/provider-scoped resolver input，不复制 primary 明文 key；构建失败须可观察并依明示 fallback policy 决定是否 fallback。 |
| Subagent 默认 | 使用 parent resolved model | 不重新解析/共享一个全局 key。 |
| Subagent model override | 只有匹配显式授权 scope | 必须含 provider + model identity；另一 provider 不能仅通过给出模型字符串而获得 primary credential。 |
| Onboarding legacy key | 不自动绑定任何 provider | 全局 `go-cli/api-key` 或无 provider 的 `auth.json` 值不可动态绑定当前 default；缺少其他合法 scoped credential 时，返回可诊断的 fail-closed 错误并要求显式 scoped 配置迁移。合法 scoped source 仍按 precedence 使用；不新增迁移向导。 |

`AuthChain` 当前声明 CLI > keychain > auth.json > env > models.json，但没有 CLI 生产路径调用它；其 KeychainSource 忽略 provider 参数，auth file 的单 key 格式也会匹配任意 provider (`internal/config/keychain.go:88-93`, `internal/config/auth_chain.go:123-154`)。R3 不应直接把该链插到 `buildModel` 并以 provider 参数表面调用，因为那会保留全局凭据跨 provider 的问题。先统一 secret source scope 和 precedence，再由 resolver 获取 `CredentialBinding`。既存的全局 keychain key 与 provider-free auth.json 值均保持 inert：不推断、不动态绑定 default，也不向候选 provider 扩散；缺少合法 scoped source 时只提示用户迁移到显式 provider/protocol/origin scope。该修正不阻塞 primary：当其它合法 scoped source 存在时，resolver 仍按配置层优先级使用该 source，不需要额外 migration wizard。

## Usage、cost 与 attempt 语义

给每次 provider dispatch 一个唯一 `AttemptID` 和 parent logical operation/run ID。每个 terminal record 包含实际 resolved ProviderID、ModelID、protocol、dispatch outcome、reported usage、usage source、可计算 cost/source；不要用“初始配置模型”代替真正被 cycler/fallback 选择的模型。attempt 在 dispatch 前失败时标 `not_sent`，不记实际 provider usage；发出请求后即使 provider error 没有 usage，也要将 `usage=unknown`，不能把它当零消费。

Usage 对一个 attempt 只能累计/提交一次：provider 返回的累积 usage event 先按协议归一为该 attempt 的最终 usage；`Final`/R1 terminal 收口后以 AttemptID 去重、写入 CostTracker。对没有 usage 的成功响应保留 usage unknown。model price 只有在 `ModelSpec` 有明确来源和当前值时才可算 estimated cost，并标成估算；models.dev 价格不能伪装成 provider 的实际发票。Provider 明确报告实际费用时保留其 provenance。

一次逻辑 provider operation 的 `MaxAttempts` 是 retry、cycler/fallback 与 overflow-triggered compact-and-retry 合并后的**总 provider dispatch 上限**；切换 candidate 或进入 overflow recovery 不会重置计数。长度 continuation 是一个新的 provider operation，但仍受该 run 现有固定 continuation 上限约束。每个实际 provider dispatch 都有唯一 `AttemptID` 和自己的 usage/cost 记录，包括 retry、fallback、overflow retry 与 continuation；禁止只在 Generate 成功返回时计数。Stream 的 usage、terminal error 里携带的有效 usage 也必须送入同一计量出口。无 usage/价格就报告已知部分与未知状态，不能 `Cost=0` 伪装未消费。成本展示不是权威资金账本，R3 不引入硬预算：PRD-04 只有在产品显式决定 soft budget/hard admission、默认值和重置周期后才能成为另一个合同；PRD-10 streaming undercount 是必须修复的缺陷。

建议的验证至少包括：

- `TestStreamUsageAppearsInCostReport`：成功 stream 的 final usage 只入账一次；无 usage stream 保持 unknown。
- `TestRetryCostsEachDispatchedAttemptOnce`：第一次被 provider 接收后 transient failure、第二次成功，按两个实际 attempt 记 usage/cost，不把逻辑请求折为一次。
- `TestCandidateSwitchDoesNotResetMaxAttempts`：retry 后切到 cycler/fallback candidate 仍消耗同一个 operation 总 dispatch 上限。
- `TestContinuationIsSeparateProviderAttempt`：length continuation 有独立 attempt/model/usage，最终展示合计和来源明细。
- `TestPreDispatchFailureHasNoProviderUsage`：resolver/auth/protocol fail-closed 不产生 provider usage；确认请求未到 fixture server。
- `TestCyclerCostUsesActualSelectedProvider`：成本标签取实际 candidate 或显式 fallback provider，不取原 primary 配置。
- `TestRetryFallbackOverflowShareMaxAttempts`：retry、显式 fallback 和一次 overflow-triggered compact-and-retry 的全部 provider dispatch 共同消耗一个 operation cap。
- `TestContinuationCapDoesNotResetAfterRetryOrSwitch`：length continuation 有独立 attempt 与 operation cap，但既有 run-level continuation cap 不因 retry 或 provider switch 重置。
- `TestRestartDoesNotReplayProviderRequestAfterInputSync`：重启恢复已同步 input 的 incomplete run 时不调用 provider；仅仍由原 Loop 持有的安全 retry 可复用 input/projection。
- `TestUnscopedLegacyCredentialRequiresScopedMigration`：只有全局 keychain/auth.json key 时 zero provider request 并返回迁移提示；同时提供合法 scoped source 时仍按 precedence 成功，且该 scoped secret 不随 provider/origin override 转移。
- 对 terminal error-with-usage、partial usage、重复累计 usage event、usage-after-final、cancel before/after dispatch 加协议 fixture，定义唯一终态前允许保留什么 evidence。

## Retry、fallback、continuation 边界

现有 `LoopAgent`/仍持有该 run 的 Loop 调度边界是唯一 retry/fallback 决策方，并持有每个逻辑 provider operation 的一个 `MaxAttempts` 计数。重试 middleware、failover、cycler 和 overflow recovery 可分类失败或提供明确候选，但不得各自启动一套重置计数的 dispatch loop；它们只能把候选/typed failure 交回该 owner。R1 跨边界的最小 typed attempt facts 至少包含唯一 `AttemptID`、是否已 dispatch、typed terminal outcome、是否发布 candidate、是否开始 tool dispatch、是否有 partial-result durable ack，以及旧 attempt 的 owner quiescence。安全重试必须由仍持有 run 的 Loop 根据这些事实判断，不能从 channel 是否创建、自由文本错误或 middleware 局部状态推断。R2 crash recovery 恢复为 incomplete/interrupted 状态，绝不自动重发该 run 的 provider request；已同步的 input/compaction projection 只可由原 run 的安全重试复用。

`MaxAttempts` 是一次逻辑 provider operation 中 retry、显式 fallback/candidate switch 与 overflow-triggered compact-and-retry 合计的**总 provider dispatch 次数**；任何内层重试、换 candidate 或 overflow 层都不得重置计数。每次实际 dispatch 分配唯一 `AttemptID` 并独立记录 usage。`finish_reason=length` continuation 是新的 provider operation，但 run 的既有固定 continuation 上限不会因重试、overflow 或换 candidate 重置；每次 continuation dispatch 同样使用新 `AttemptID`。分类使用 `ProviderError` 等 typed failure，不根据自由文本猜测；context error 优先判定。

| 事实状态 | 自动 retry/fallback | 处置 |
|---|---|---|
| `ctx.Canceled`、deadline 已到或上层取消 | 否 | 返回 cancellation/deadline terminal；不再排新请求。 |
| typed transient network / 429 / retryable 5xx，且 run-owned typed facts 证明本次 attempt 尚无 caller-visible candidate、无 tool dispatch、无 partial-result durable ack，旧 attempt 已 stopped/quiescent | 可，受 operation-wide retry+fallback+overflow `MaxAttempts`、deadline 与 backoff 限制 | R2 已提交的输入与 compaction projection可由仍运行的同一 Loop run 复用；无需重新提交 user input。每次新 dispatch 单独计量；provider 可能已收取第一次请求费用，缺 usage 标 unknown。进程重启后的 R2 recovery 不再发起 provider request。 |
| non-retryable auth / protocol / schema / malformed response | 否 | 返回原始 typed terminal evidence；不绕到另一个协议/provider 掩盖配置错误。 |
| streaming error 且已有 partial assistant text/候选被发布 | 否（禁止透明重播） | 将部分内容和 error 标为 incomplete；用户可决定另起新请求。partial 不能事后称为成功。R1 CompleteResponseGuard 会影响“何时可见”，应依据实际发布点，而非只见到 provider token。 |
| streaming error 发生在任一内容可见前 | 仅当仍持有 run 的 Loop 收到 typed facts，确认 stream/readers 停止且无工具派发/partial-result ack 时可能允许 | 由同一 Loop operation budget 判定；不可凭 `Stream()` 初始化返回 nil 或 middleware 本地次数自动重试。 |
| 完整 assistant tool-call terminal 已收到但尚未 dispatch | 不重新生成该 terminal | 按已验证 tool protocol 将这一组调用送入 R1/R2 action lifecycle；provider 响应本身不是 failed attempt。 |
| 工具已 dispatch 且有 known terminal receipt | 不重放该 tool | 根据 receipt 继续后续对话；下一次 LLM 请求是新的 attempt。 |
| tool receipt 为 `unknown`、执行/外部副作用结果不明确 | 不 retry tool，也不重放含该 action 的整个 run | 保留 unknown/quarantine 事实，等待 reconciliation/用户新请求；R1/R2 不授权自动重放。 |
| `finish_reason=length` 等合法截断后 continuation | 新 provider operation 和新 provider attempt | 只有 R3 context policy 保留合法 transcript/tool group、R1 仍掌握 run、没有未知 side effect 时按明确 continuation policy 继续；run 的固定 continuation 上限持续生效，前次 usage/cost 保留。continuation 失败返回 incomplete error，不能吞错保留成功结果。 |
| Unsupported/native-disabled identity、candidate unresolved 或 candidate 缺少 scoped credential | 否 | 返回 typed resolution/configuration error，provider call 与 secret lookup 均为零；不重试、不静默换 primary。若其它显式 scoped source 对当前请求有效，则仍由统一 precedence resolver 使用它。 |
| Cycler candidate build/dispatch error | 绝不静默换 primary | build/resolution 错误直接可见；仅在显式 fallback policy 指明下一候选、Loop typed facts 证明 dispatch retry 安全且 shared operation budget 有余量时才可 fallback。原始与 fallback attempts 均可观测/计量。 |

Provider POST 请求一旦 dispatch，远端可能已执行推理并计费；无论可否从外部副作用角度安全重试，都必须保留每次 dispatch 的账务 unknown/usage evidence。此模型重试与工具 side effect retry 不同：模型调用可能重复费用，工具调用可能重复外部效果，后者在 R1/R2 unknown 时绝不自动 replay。

## Transcript 与 compaction 接口边界

R3 provider 设计不创建第二份会话历史，也不将成本/summary 伪装为对话事实。请求编码前，检查实际发送的 assistant tool-call 与 `ToolCallID` receipts 组闭合；每个协议 adapter 做明确的 assistant/tool result round trip。截断、continuation、summary 与 restore 不得拆开 tool-call group。按 `r3-compaction-design.md` 的现有方案，R1 `CanonicalRunResult.Transcript` 是当前 run 唯一 transcript delta，R2 负责 durable append/recovery 和 unknown receipts，R3 compaction 只使用经 R2 定义的 projection/cursor seam；此设计稿不改该 seam。

Loop/仍持有 run 的 owner 为每次实际 provider attempt 汇总唯一 typed facts：AttemptID、是否 dispatch、terminal protocol outcome、stream/resource quiescence、是否发布 candidate text、是否有工具 dispatch/receipt、以及是否存在 partial-result durable ack。Retry/failover/overflow middleware 只提供 typed classification/候选，不另自判安全或持有独立重试预算。R2 `AgentMessage`/JSONL/action receipts 需要在 restart 后保留 assistant call ↔ tool receipt 的精确 ID/group 与 `unknown`；restart 恢复 incomplete run 后绝不重新派发旧 provider request 或 tool action。成本 attempt IDs 不是 action receipt IDs，R3 不让 CostTracker 承担 action durability。

## 最小实施与验证顺序

这些是后续 builder 范围建议；R1-REV-01 已通过，R2-REV-01 的实现/restart-safety 验收仍须通过后才能开始依赖它的 R3 builder，也不把这份 design 标成完成：

1. **R3-BLD-01：解析与协议 gate。** 在 `internal/llm` 为现有 provider/interface 增加 spec、model catalog、resolver；更新 `internal/cli/assemble.go` 的 build/list/small/switch；由同一 assembly resolver 注入 cycler/subagent；对 model list、doctor 和 composer 的同名/排序语义做一致收敛。实现前先为每个实际分流写行为红测。必需任务用例沿用 R3 计划：`TestProviderFactoryConsistentAcrossEntrypoints`、`TestProviderRejectsUnsupportedToolProtocol`、`TestProviderPreservesSystemAndImageInputs`、`TestProviderComposerDeterministicWinner`。
2. **凭据与配置切口。** 在配置/Onboarding 层为 provider identity 和 secret source 形成单一路由，保留旧 config/YAML/environment 优先级并显式测试；scoped keychain/auth file、onboarding 保存/后续恢复、primary/small/cycler/subagent 的同 origin/异 origin case 均用 canary `httptest` 校验真实 HTTP origin/header。全局 `go-cli/api-key` 与 provider-free `auth.json` 值不迁移绑定到 default；仅提示用户写入显式 provider/protocol/origin scoped 配置，且不阻塞其它合法 scoped source 按既定 precedence 使用。不新增迁移向导。测试不得使用真实 API key 或网络。
3. **Native protocol 注册分 commit。** 只在上述 native fixture 全套过关且产品/协议版本审批后，单独 enable Anthropic/Gemini。需要补 `TestNativeProviderToolCallResultRoundTrip`、对应 system/image/finish/usage/stream fixture；disabled failure path 必须断言 HTTP request count 为 0、credential source lookup 为 0。
4. **R3-BLD-02：retry/accounting。** 依赖 R1-REV-01 终态事实。按 attempt ID 实现 typed retry、stream terminal breaker/usage-cost、失败边界和可观测 fallback；保留 exact total `MaxAttempts`。至少测试上节 attempt accounting 和 retry matrix 中每一行。
5. **R3-BLD-03：request integrity/compaction。** 依赖 R2-REV-01，沿用 `r3-compaction-design.md`；context preflight 发生在 provider dispatch 前，压缩只在完整 `ToolCallID` 组边界上，并通过 restart fixture 验证不出现不完整 tool protocol transcript。
6. **R3-REV-01：独立 review。** Code-free 验证 provider support matrix、fake local provider 的真实 CLI assembly、credential scopes、native registered paths、attempt counts、cost source 与 transcript/tool-group 合法性。执行任务要求的 `internal/llm`、`internal/core`、`internal/compaction` 包 race 套件，并保证所有测试命中而不是 0 tests。

R3 当前进入条件：`R1-REV-01` 独立验收通过；`R2-REV-01` restart-safety review 通过；R3-DES-01 的支持矩阵及 credentials/retry/cost 决策 review 完成。当前文本已吸收 R3 review 的三项 P1 设计 blocker，仍待独立 R3-DES-01 复核；在这些门禁完成前，R0 的 OpenAI-compatible/default fail-closed 是当前事实，新增模型/协议/entrypoint routing 不得凭设计草稿宣称可实施或可发布。

## 本次只读核对记录

- 主审计确认 LLM-01 provider resolution/credential routing、LLM-02 native tool loop、LLM-06 primary sampling、LLM-07 Gemini system prompt、LLM-08 tool-group continuation、LLM-09 stream retry/failover、LLM-10 cycler credential routing、LLM-12 native image omission、LLM-14 finish reason、LLM-15 composer order、PRD-02/03 retry 与 PRD-10 stream usage 都是本设计必须涵盖的来源 finding；PRD-04 仍是待产品裁定的可选硬预算。
- 已有 R0 provider lane 证明 native `claude`/`anthropic`/`gemini` fail-closed，custom explicit BaseURL 保留 OpenAI-compatible，small model 不跨 provider/origin 继承 key。此处作为不回退的 baseline。
- R1-REV-01 已独立通过；R2 design 已在 `4f8d1a0d72eebc25fcf06c1fcb897d64bf73d41c` 批准，R2-BLD 实现与 R2-REV-01 restart-safety 验收仍未完成。R3 review 列出的三项 P1 已在当前 proposal 收敛，等待 R3-DES-01 独立复核；R3-BLD-02/03 保持其 R1/R2 implementation-review gates。
- 本次没有修改源码或运行测试；实现 RED/GREEN 与包 race 证据由对应 builder/reviewer 产生。本文仅为 R3-DES-01 的 source-grounded proposal。
