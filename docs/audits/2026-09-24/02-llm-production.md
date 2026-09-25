# LLM 与 Production Runtime 审计

日期：2026-09-24  
范围：`internal/llm`、`internal/production`，以及 CLI / Core 对它们的实际装配和调用。  
证据边界：源码追踪为主；没有调用真实模型、没有访问外部服务或写入外部系统。本轮没有执行完整测试套件。

## 结论

底层有可复用的模型接口、OpenAI/Claude/Gemini 原生适配器、结构化 ProviderError、流式解析器和一组 production middleware，但这些部件还没有组成可依赖的 Runtime：CLI 实际默认只注册 Eino/OpenAI-compatible provider；Native provider/composer 没接入实际 provider resolution；流中断、超时和输出安全在真实 streaming 路径上没有一致的失败语义；production 的 retry、loop detector、预算、统计等也有配置或事件接线缺口。

最高优先级是先闭合实际请求路径。下表区分确认缺陷与依赖产品验收目标的能力缺口：

证据等级：A = 本轮 targeted repro；B = 源码控制流/数据流直接证明；C = 协议契约或需结合运行策略作出的判断。多级并列表示证据组合。

| ID / 优先级 | 发现 | 当前实际影响 | 证据 |
|---|---|---|---|
| LLM-01 / P1 | CLI provider resolution 不会选择原生 provider；凭证或配置可能被送入 Eino 的 OpenAI-compatible 路径 | 默认 CLI 无法解析未注册 provider；Claude API key + 空 BaseURL 会走 OpenAI endpoint/Bearer header | A+B |
| LLM-02 / P1（原生接入前门槛） | Claude/Gemini 原生 tool calling 请求/响应闭环不完整 | 当前 CLI 默认不可达 native adapters；接入前必须补协议闭环，否则 agent 工具循环失败 | B+C |
| LLM-03 / P1 | SSE 读取/解析错误可被表示为正常 stream 关闭 | 当前 CLI 默认 streaming 可把部分回复当完整结果，甚至执行不完整 tool call | B |
| LLM-04 / P1 | 默认 stream timeout 把超时变成无错误截断 | 当前 CLI 默认 streaming 超时后可能按成功处理部分结果 | B |
| LLM-05 / P1 | CLI streaming 路径绕过 output guard，Generate guard 出错时 fail-open | 主 agent 实时可见输出未检查；Generate 错误也可放行原文 | A+B |
| PRD-01 / P1 | 默认 loop detector 的事件类型和 Core 不一致，且 Run 结束后才检查 | 当前防循环保护不能控制当轮 agent loop | B |
| PRD-02 / P1 | production retry 把结构化 5xx/network ProviderError 判为 fatal | 当前默认 retry policy 不恢复常见 transient 错误 | B |
| PRD-03 / P1 | MaxAttempts 有 off-by-one | CLI 配 3 次时可执行 4 次尝试 | B |
| PRD-04 / 能力缺口 | CLI 未暴露 CostTracker budget 配置；库没有承诺默认硬预算 | 只有产品要求硬预算时才需加入配置与 admission policy | B+C |
| PRD-10 / P2 | Stream usage 没有进入 CostTracker，`/cost` 对流式会话低报 | 当前交互主要走 Stream，会话成本统计不完整 | B |

## 实际 Runtime 链

Prompt 默认 provider 是 `eino`，最终以 flag、配置或默认值解析（`internal/cli/prompt.go:20-25,326-335`）。`buildModel` 只构造 `EinoProvider`，或从 `NewProviderRegistry()` 取 provider；该 registry 只预注册一个 `eino`（`internal/cli/assemble.go:1465-1496`、`internal/llm/registry.go:32-45`）。如果提供了 API key，即使没提供 BaseURL 也进入自定义 Eino 分支。EinoProvider 的默认 endpoint 是 `https://api.openai.com/v1/chat/completions`，通过 Bearer 头发送 key（`internal/llm/provider.go:50-62,90-104,468-485`）。Native adapters 虽有真实 provider 类，`DefaultProviderComposer` 也枚举 `eino/openai/claude/gemini`，但 CLI 未调用 composer，也没有将原生 providers 注册到上述 registry（`internal/llm/composer.go:218-224`；`rg` 未发现 CLI/Core 调用 `ProviderComposer` / `Compose`）。

Core 会在每轮把工具定义经 `llm.WithTools` 传给模型，然后通过 `model.Stream` 生成；收到错误 chunk 才会返回错误，而 stream channel 无错误地关闭时，只要曾收到任意 chunk 就会构造成功响应（`internal/core/loop.go:401-405,795-860`）。CLI 给这个模型套用 production wrapper、LLM middleware chain、telemetry、circuit breaker 和 output guard（`internal/cli/assemble.go:1107-1115,1276-1284`；`internal/cli/output_guard_adapter.go:211-237`）。因此 Generate 的单元测试不能代表真实交互：主要 agent 运行使用 stream。

## 做得合理、应保留的设计

- `BaseChatModel` 统一 Generate/Stream，`MessageChunk` 已为最终标记、tool calls、usage 和 middleware error 留出字段，适合承载明确的 stream 状态协议（`internal/llm/llm.go:75-103,195-203`）。
- Native provider 共享 transport 和 tracing 逻辑，同时将 OpenAI、Anthropic、Gemini 请求编码/响应解码分开；这比在一个混合编码器里按字符串分支更便于做 provider contract tests（`internal/llm/native.go`）。当前问题是接线和闭环不完整，不是拆分本身。
- Native HTTP 错误有 ProviderError 分类，能保留 status/provider/error category；LLM retry/failover middleware 有结构化错误识别能力（`internal/llm/provider_error.go:12-48,75-103`、`internal/llm/middleware_retry.go`）。production retry adapter 当前绕开了该能力，见下文。
- SSE parser、OpenAI stream accumulator 与各原生 provider 的事件处理拆分清楚。`DefaultModelMiddlewareChain` 锁保护注册和读取，并按注册顺序形成固定 onion 顺序（`internal/llm/middleware_chain.go:46-96,99-180`）。需要把 parser/stream 的失败终态补齐。
- Retry 使用 context-aware backoff wait，Production stats/tracker 使用锁及快照保护共享状态；这些实现基础可以保留（`internal/llm/middleware_retry.go:277-281,315-319`、`internal/production/stats.go`、`internal/production/cost_tracking.go`）。
- Tool idempotency 有容量边界，Audit 单实例写入有串行化及链校验；PromptInjection 工具包装和 `OutputGuardMiddleware` 自身采用 fail-closed。CLI 的 output-guard adapter 没有继承这些保证（`internal/production/idempotent.go`、`internal/production/audit.go`、`internal/production/output_guard_middleware.go`、`internal/cli/output_guard_adapter.go`）。

## 已确认缺陷

### LLM-01 / P1 / 证据 A+B：Provider resolution 不符合 CLI 暴露的 provider 语义，存在凭证错投风险

**位置：** `internal/cli/assemble.go:1470-1496`、`internal/llm/registry.go:32-45`、`internal/llm/provider.go:50-62,90-104,468-485`、`internal/llm/composer.go:218-224`。

**触发与后果：** 用户选择 `claude`，且配置有 API key 但没有 BaseURL 时，CLI 进入 `NewEinoProvider(WithProviderName("claude"), WithAPIKey(...))`。`WithProviderName` 只改显示名，不改 endpoint、请求 JSON 或 auth scheme；请求仍 POST OpenAI chat-completions endpoint 并把凭证放入 Bearer header。无 API key/BaseURL 时，fresh registry 查找 `claude`/`openai`/`gemini` 会返回 provider not found。若 BaseURL 配成兼容网关，请求协议仍固定为 OpenAI-compatible，并非 Claude/Gemini 原生 API。

**适用范围：** 这是当前 CLI provider resolution 的默认可达缺陷。  
**证据：** 临时 targeted repro 直接调用真实 `buildModel`，以 `providerName=claude`、空配置触发 `llm: provider not found: claude`；复现测试源码/完整输出保存在 `evidence/llm-cli-provider-guard-repro.go.txt` 与 `evidence/llm-cli-provider-guard-repro.log`，临时产品测试已清理。凭证错投风险由 BaseURL/API key 组合及 Eino 请求实现直接推导。

**回归建议：** CLI 通过统一 provider factory/composer 注册原生 provider；新增 mock HTTP transport tests 逐 provider 断言 endpoint、headers、wire-format 和模型名。Claude key 不得出现在 OpenAI 域名请求；无 provider 配置的 Claude/Gemini 必须能从对应原生 adapter 构建。`listAvailableModels` 与 `buildModel` 必须共用 resolver。

### LLM-02 / P1 / 证据 B+C（原生接入前门槛）：Claude/Gemini 的 tool-call 协议没有完整闭环

**位置：** `internal/llm/native.go:1081-1095,1125-1205,1207-1229,1292-1323,1367-1420,1460-1505`；Core 传递 tools 的位置见 `internal/core/loop.go:401-405`。

**适用范围：** 当前 CLI 默认构建路径不会到达这些 native adapters；该缺陷是把 native provider 接入默认 CLI 前必须关闭的协议门槛。其他直接使用 native adapter 的消费者仍会受影响。  
**触发与后果：** Core 总会通过 `WithTools` 提供当前工具定义，但 Claude request type 没有 `tools` 字段，编码器不读取 `genOpts.Tools`；普通 Generate 的 decoder 只拼 text block 并忽略 `tool_use`。Anthropic 对话中 `tool_use` / `tool_result` 的消息角色和 content block 结构也未映射，`RoleTool` 被原样编码成 role=tool。Gemini request type 同样没有 `tools`；虽能解码 response 里的 `functionCall`、能把 tool result 编成 `functionResponse`，但没有把 function declarations 发送给模型。因此原生请求不能告知模型可用工具，Generate 路径也无法可靠还原 tool calls。

协议依据：[Anthropic 官方 tool use 文档](https://docs.anthropic.com/en/docs/agents-and-tools/tool-use/implement-tool-use)说明请求需声明 tools，并以 assistant `tool_use` / user `tool_result` 完成一轮；[Gemini generateContent API](https://ai.google.dev/api/generate-content)定义了 `tools[]` 与 `systemInstruction` 字段。

**回归建议：** 每个 adapter 增加逐字节 JSON request fixtures 和 assistant-tool-use/tool-result multi-turn contract test，覆盖一次以上工具调用及 parallel calls；在测试 server 上验证 Core 能取回 tool call、执行工具、编码工具结果并继续生成。

### LLM-03 / P1 / 证据 B：SSE/stream 中途失败被静默降级为成功关闭

**位置：** `internal/llm/sse_parser.go:40-114`、`internal/llm/provider.go:250-257,349-365`、`internal/llm/native.go:214-230,427-430,518-520,695-697`、`internal/core/loop.go:813-860`。

**触发与后果：** `bufio.Scanner` 的最大行长固定为 1 MiB，但 parser 不检查 `scanner.Err()`；reader I/O error 或超长 SSE 行会退出扫描，parser 仍正常关闭 channel。畸形 SSE JSON 在 accumulator/provider event loops 中被跳过，底层 provider 最后仍可能发 Final chunk。OpenAI-compatible 和 native provider 的 stream channel 注释也说明非 2xx 连接后错误不再返回。Core 只认 `MessageChunk.Error`，对无 error 的关闭会把已收集的部分 content/tool arguments 当成功 assistant message；不完整 JSON arguments 有执行风险。

**证据：** parser 明确注释返回错误始终 nil；各 stream caller 丢弃 Parse 的 error 返回值；Core 的成功条件只检查“有没有 chunk”，没有要求 Final。源码路径贯通，静态证据强。

**回归建议：** 流必须有唯一 terminal outcome：complete、provider error、parse error、timeout、cancel。测试 reader 在部分 event 后返回错误/超过行限制/返回坏 JSON，要求 stream 返回 terminal error，不得产生成功 Final，不得执行工具；Core 对 EOF-without-Final fail closed。

**计划去重：** 与 LLM-04 共用“Stream 缺少统一 terminal contract”这一 Runtime 根因，统一放在 R1 实现；本项保留 SSE parser/read error 这个独立来源，避免把两个源头重复拆成两套终态设计。

### LLM-04 / P1 / 证据 B：默认模型 timeout 把超时变成无错误截断；30 秒不适用于长生成

**位置：** `internal/llm/middleware_timeout.go:44-55,100-159`、CLI 默认装配 `internal/cli/assemble.go:1107-1115`、Core stream 收口 `internal/core/loop.go:813-860`。

**触发与后果：** CLI 默认使用 30 秒总时长、10 秒 chunk idle 超时。Stream middleware 超时后取消 context 并仅关闭输出 channel，不送 `MessageChunk.Error`。对于合法的长推理或长工具参数生成，消费者看到的是成功结束；这与上一个无 Final 的 stream parser 问题叠加后，可静默截断并错误保存/执行部分结果。长时间 tool call 等待也会被 idle deadline 误杀。

**回归建议：** timeout 映射成明确 terminal timeout error；总时长/idle 时长按操作类型和配置分离，长连接由 context budget 控制。验证超时发生在首 token 前、文本中间、tool arguments 中间和 final 前时均失败关闭，且取消后 goroutine/body 被回收。

**计划去重：** 与 LLM-03 共用 R1 terminal contract；本项单独保留 timeout policy、时限配置和超时资源清理的工作，不另建一套 stream completion 语义。

### LLM-05 / P1 / 证据 A+B：CLI 流式输出没有应用 OutputGuard；Generate guard 错误时 fail-open

**位置：** `internal/cli/output_guard_adapter.go:32-72,211-237`、实际装配 `internal/cli/assemble.go:1276-1284`。

**触发与后果：** `outputGuardModel.Stream` 直接透传 inner stream；主 agent Core 正常走 Stream，因此 UI 上逐块可见的回复没有经过 output guard。Generate 路径也只有在 `Check` 返回 nil error 且非 nil result 时才替换/阻断；guard 运行错误时原始文本按 nil error 返回。

**证据：** 临时 targeted test 用拒绝 `sensitive-output` 的 fake guard 包住 stream model，断言敏感串不应离开 wrapper；当前实现将其原样输出，测试失败。复现源码/完整输出保存在 `evidence/llm-cli-provider-guard-repro.go.txt` 与 `evidence/llm-cli-provider-guard-repro.log`。实现时只能采用输出可见前 buffer/delay 或明确 typed streaming policy；已经显示给用户的内容不能通过事后撤回补做保护。

**回归建议：** 定义输出可见前的 streaming 安全策略（缓冲/延迟输出，或带明确约束的 typed policy）；保证 Guard error 采取明确的 fail-closed 错误，不再继续传原始文本。测试 Generate/Stream 两路径、guard allow/block/error 和 chunk boundary。

### PRD-01 / P1 / 证据 B：Loop detection 没有接到 Core 实际事件，也不能中断当前 Run

**位置：** `internal/production/loop.go:50-67,195-219`、`internal/cli/assemble.go:1368-1400,2018-2040`、`internal/core/loop.go:625,687,703`、`internal/core/types.go:87-90`。

**触发与后果：** 默认 detector 等待 `tool` / `edit` / `test_failure`，Core 发的是 `tool_call` / `tool_result`，失败由 `tool_result.IsError` 表达；未找到默认 edit/test-failure 事件生产点，默认计数不增长。即使事件名修好，runtime adapter 也是整次 `Run` 完成后才调用 Observe，不能停止本轮重复执行；`DispositionTerminate` 未应用，Warn 也无条件注入 reminder。

**回归建议：** 为 core 和 production 定义同一 typed event schema；每次工具调用/结果实时送 detector，检测结果同步交给 loop controller，使 terminate 在下一次 tool execution 前生效。使用真实 LoopAgent + 重复工具 mock 做闭环测试，不只单测 detector。

### PRD-02 / P1 / 证据 B：生产 RetryPolicy 不识别 ProviderError 的 server/network 类型

**位置：** `internal/cli/assemble.go:1093-1110`、`internal/production/retry.go:123-145,243-255`、`internal/llm/provider_error.go:23-44,75-103`、`internal/llm/middleware_retry.go:250-282`。

**触发与后果：** assembly 把 production retry policy 经 adapter 注入 LLM middleware。Native provider 对 5xx / 网络错误返回 `ProviderError{ErrorType: server|network}`；production classifier 不按这个类型分派，错误文本如 “server error ... HTTP 500: internal error”/“network error ...”不符合有限的 transient 文本关键字，被归类 fatal 而跳过重试。LLM 内置 middleware classifier 已能读取结构化 ProviderError，但被 production policy 覆盖。

**回归建议：** 统一共用结构化错误分类接口；测试 assembly 后的实际 wrapper 对 429、500、network、401/403、context canceled 各调用次数和重试/不重试行为。

### PRD-03 / P1 / 证据 B：MaxAttempts 与总尝试次数不一致

**位置：** `internal/production/retry.go:35-37,164-174`、调用循环 `internal/llm/middleware_retry.go:250-282`；当前策略测试 `internal/production/retry_test.go:60-69`。

**触发与后果：** `MaxAttempts` 文档表示总尝试次数，但 ShouldRetry 使用 attempt=0 起算，0 到 MaxAttempts-1 都放行重试。CLI 设为 3 时可最多调用 4 次。现测试验证策略 decision，没有验证调用总数。

**回归建议：** 对固定失败 mock 调用组装后的模型，断言 `MaxAttempts=3` 总请求数最多 3；分别覆盖第一次成功、最后一次成功、所有失败和 context cancel。

### PRD-04 / 能力缺口 / 证据 B+C：CLI 没有预算配置，库未承诺默认硬预算

**位置：** `internal/cli/assemble.go:1098-1104`、`internal/production/cost_tracking.go:133-175`、`internal/production/llm_integration.go:129-153`、`internal/cli/output_guard_adapter.go:140-168`、`internal/cli/slash_handlers.go:54-81`。

**触发与后果：** runtime 创建 `NewCostTracker(nil)`，没有从 CLI/config 设置 budget。`CostTracker` 接受可选 budget 并提供记录/超额信号；现有 wrapper 对记录返回的错误只 warn 后继续，因此硬预算需要显式配置和请求前 admission gate。源码没有说明 CLI 必须默认启用硬限额，因此不能仅凭 nil budget 判定为当前实现 bug。若产品要求管理员可设置硬预算，则需要新增配置面、定义预算耗尽后的请求行为和用户可见语义。

**回归建议：** 产品验收需先决定预算是否为必需能力；若需要，应明确软告警与硬 admission 的区分、默认值和重置周期，再测预算触发、并发最后一笔支出和取消边界。Stream usage 记录属于独立的已确认缺陷 PRD-10。

### PRD-10 / P2 / 证据 B：Stream usage 没有计入 cost，`/cost` 低报

**位置：** `internal/production/llm_integration.go:129-153`、`internal/cli/output_guard_adapter.go:140-168`、`internal/core/loop.go:841-845`、`internal/cli/slash_handlers.go:54-73`。

**触发与后果：** `MessageChunk` 有 Usage，Core 也会从 final chunk 读取 usage；但 CostTracker wrapper 与 telemetry adapter 的 Stream 直接返回 inner channel，记录逻辑只在 Generate 执行。主要 agent 请求走 Stream，导致 `/cost` 不含这部分 tokens/cost，即便 provider 已返回 usage。

**回归建议：** Stream 与 Generate 共用一次 terminal usage accounting，区分 API-reported usage 和估算值；测试成功 stream、无 usage stream、failed/canceled stream 的计费边界，避免重复记账。

### LLM-06 / P2 / 证据 B：主要模型的配置 sampling 参数被丢弃

**位置：** `internal/cli/assemble.go:1470-1484`；small model 对比 `1527-1531`。

**触发与后果：** 主模型只创建 `llm.ModelConfig{Model: modelName}`，没有传 Provider/Model 的 Temperature 和 MaxTokens；small model builder 则明确读取 SmallModel 配置。用户配置主模型采样或 token 上限不生效，影响输出随机性、成本和长度。

**回归建议：** 配置解析/优先级（per-call override > Model > Provider > defaults）覆盖零值与显式 0；测试 mock transport 收到实际 sampling 字段。

### LLM-07 / P2 / 证据 B+C（原生接入前门槛）：Gemini system prompt 被降为普通 user 内容

**位置：** `internal/llm/native.go:1292-1296,1460-1505`。

**触发与后果：** 编码器把 system role 改写成 user content，而 request type 没有 `systemInstruction`。基础 system prompt 失去 system 级别位置；对于有工具权限的 agent，这会削弱指令优先级边界并产生行为偏差。Gemini 官方 `generateContent` API 支持独立 systemInstruction 字段，见上面的 API link。

**回归建议：** 在 Gemini request struct 中显式映射 system instruction；多条 system 指令按有序规则处理；验证请求 JSON 的字段与 role 序列。

### LLM-08 / P2 / 证据 B：Overflow recovery 会切断工具对话并掩盖 continuation 失败

**位置：** `internal/llm/middleware_overflow.go:188-223,262-308,328-356`。

**触发与后果：** 重试时按固定比例从任意消息边界删除旧消息，仅保护 index 0 system message；可能保留 assistant tool call 却删掉对应 tool result，或反过来。Continue 失败只记录日志，仍返回部分 `result,nil`；stream continuation 的失败也会仅 warn 后保留先前输出。调用者无法区分完整回答与截断半成品。

**回归建议：** 用 token-aware compaction 只删除完整 user/assistant/tool exchange；tool call 与 result 作为原子组；continuation 最终失败返回明确 incomplete error，并携带部分内容作为诊断数据但不提交成功。

### LLM-09 / P2 / 证据 B：Stream 重试和 Failover 只覆盖连接初始化

**位置：** `internal/llm/middleware_retry.go:285-321`、`internal/llm/middleware_failover.go:110-133`；CLI 仅创建无 fallback 的 `NewFailoverModelMiddleware()`：`internal/cli/assemble.go:1107-1110`。

**触发与后果：** 一旦 Stream 返回 channel，后续 provider error/断线不会尝试 retry/fallback；当前底层也没把 stream read error 放进 chunk。且 CLI 未提供 fallback models，因此 runtime failover 实例实际只有 primary。单纯自动重放整个 stream 会重复已显示内容/重复副作用，须先定义 resumable boundary。

**回归建议：** 首先让 stream 可靠报告失败终态；只在首 token/首副作用前重试，或实现明确 resume/token dedupe；装配可用的 fallback provider 配置并测 fallback 只重试一次。

### LLM-10 / P2 / 证据 B：ModelCycler 在 CLI 中缺少候选 provider 凭证与路由

**位置：** `internal/cli/assemble.go:673-689`、`internal/llm/cycler.go:90-105,232-265,274-290`。

**触发与后果：** 启用 cycler 时 CLI 填入的 registry 是刚创建的 `NewProviderRegistry()`（仅有默认 eino），配置 entry 只有 Provider/Model，动态构建也只传 Model 不传 APIKey/BaseURL。Claude/Gemini 候选构建必失败后静默使用 primary；即使选中 eino，也会使用 registry 的空 key 和默认 baseURL。实际表现与“模型轮换”配置不一致。

**回归建议：** 候选项绑定经过安全处理的 provider instance/credential reference，不能在普通 ModelEntry 里复制明文 key；选中失败要暴露 telemetry/error event，不得静默假装路由成功。以两个 httptest provider 验证策略选择、凭证和回退。

### LLM-11 / P2 / 证据 B：Sanitizer 可能删除合法文本，且逐 chunk 正则漏掉跨块 artifact

**位置：** `internal/llm/middleware_sanitize.go:61-85,104-137`；runtime 无条件装配见 `internal/cli/assemble.go:1107-1115`。

**触发与后果：** GPT pattern `【.*?】` 删除任何全角方头括号内容，可能改坏日文文本、引用或代码中的合法字符。Stream 对每个 chunk 分别运行正则，跨 chunk 的标记无法匹配完整，危险/内部 artifact 可漏过。Claude pattern 用 `<antThinking>`；需要与当前模型协议真实标记进行 fixture 对照，而不是假设所有模型输出形态一致。

**回归建议：** 只在带来源/类型的结构化 reasoning metadata 上过滤内部内容；普通文本不做全局删除正则。增加中文/日文 bracket、代码、chunk split、真实 Claude/Gemini/OpenAI fixture 测试。

### LLM-12 / P2 / 证据 B（原生接入前门槛）：Claude/Gemini silently drop URL images

**位置：** `internal/llm/llm.go:60-73`、`internal/llm/native.go:1125-1156,1357-1420`。

**触发与后果：** 公共 `ImageURL` 明确允许 URL 或 base64 data URI；原生 Anthropic/Gemini adapter 只解析 data URI，其余 URL block 直接 continue。URL-only 内容块可能变成空用户消息，用户提供的图片被静默丢弃；OpenAI adapter 则直接保留 URL，形成 provider 行为不一致。

**回归建议：** 对不支持的 URL 输入返回 typed unsupported error，或在有明确安全策略时由受限 fetcher 下载并校验 MIME/size/SSRF。绝不能静默去掉输入。

### LLM-13 / P2 / 证据 B：模型 error/JSON body 无大小上限，原始响应会进入 error chain

**位置：** `internal/llm/native.go:199-211,267-278`、`internal/llm/provider.go:493-509`、`internal/llm/sse_parser.go:117-135`、`internal/llm/provider_error.go:30-44`。

**触发与后果：** 成功 Generate、非 2xx body 和非 SSE JSON fallback 都可能无上限 `io.ReadAll`。Eino 错误在截断到 512 字节前已经读完整 body；Native `ProviderError.Message` 保留全量响应且 Error() 输出全量 message。恶意/故障 endpoint 可造成内存消耗和大错误串传播，响应体也可能包含需避免进入日志/trace 的数据。

**回归建议：** 所有响应按 endpoint 合理大小设置 `io.LimitReader` 并识别超限；只保存状态码、provider request id、截断且脱敏的摘要；测试大 body 与 read error。

### LLM-14 / P2 / 证据 B：Eino Generate 丢弃 finish_reason

**位置：** `internal/llm/provider.go:228-247`；对比 Stream 解析 `finishReason` `340-357` 及 Native decoder `internal/llm/native.go:1005-1017`。

**触发与后果：** Generate 只映射 content/tool calls/usage，没有将 choice.FinishReason 复制到 Message。依赖 finish reason 判断长度截断的 Generate middleware（尤其压缩/标题等非 Core Generate 用例）无法触发 continuation 或标识不完整结果。

**回归建议：** 所有 provider 的 Generate/Stream finish reason 映射使用同一 normalized enum；对 stop/length/tool_calls/content_filter 固定 fixture。

### LLM-15 / P2 / 证据 B（原生接入前门槛）：ProviderComposer winner 顺序不确定

**位置：** `internal/llm/composer.go:299-331`。

**触发与后果：** winners 先从 map 遍历构造 slice，再仅按 Source 做 stable sort。相同 Source 的次序继承随机 map 迭代，不遵守注释中的 deterministic registration order；`ProviderRegistry.Default()` 若取 List 首项，则默认 provider 可能变化。composer 当前未接 CLI，因此是库能力缺陷与接入前置项。

**回归建议：** 加 sequence tie-breaker/名字次序，重复 Compose 100 次断言 List/Default 完全一致；再连接 CLI provider resolution。

### LLM-16 / P2 / 证据 B：Circuit breaker 只观察 Stream 初始化，不观察流终态

**位置：** `internal/cli/output_guard_adapter.go:110-137`、`internal/production/circuit.go:117-147`。

**触发与后果：** `Execute` 只在传入的函数返回时记录 outcome；Stream adapter 的函数仅调用 `inner.Stream` 并立即把 channel 返回。真正的 provider 读取失败、timeout 或无 Final 关闭发生在 channel 后续消费中，breaker 已把 Stream 初始化记成 success。即使后续补齐 stream error，当前 breaker 仍不会累计这些失败，也不会针对实际 provider stream 故障打开。

**回归建议：** 给流调用建立由 terminal event 驱动的 breaker outcome；成功只在 Complete 后记录，Failed/TimedOut 记录失败，用户主动取消按策略忽略或单独计数。测试连续中途失败后 circuit open、半开探测以及每次 request 只计一次。

### PRD-05 / P2 / 证据 B：production 工具缓存日志输出原始参数，并可能退化为错误 key

**位置：** `internal/production/idempotent.go:60-79`、`internal/cli/assemble.go:2079-2090,2119-2127`。

**触发与后果：** cache key 由 tool name + JSON(args) 组成，完整 key 被作为 trace attr/debug log 输出，MCP/其他工具参数可能包含 query、文件路径或凭证。参数不能 JSON marshal 时 `toolCacheKey` 退化为工具名，导致同名的不同调用共享 key 并可能返回无关结果。

**回归建议：** 可观测日志仅保留工具名、hash 和 hit/miss；构造 key 失败应绕过 cache 并报诊断，不能退化为碰撞 key。对 secret marker 和不可 marshal 参数编写用例。

### PRD-06 / P2 / 证据 B：Telemetry 标签序列化存在确定碰撞

**位置：** `internal/production/telemetry.go:112-133`。

**触发与后果：** metric key 用未转义的逗号和等号拼接 labels，`{a:"x,b=y"}` 与 `{a:"x", b:"y"}` 会变成同一 key，错误合并计数。

**回归建议：** 用长度前缀、稳定 JSON 或结构化 comparable key 编码 labels；加入包含分隔符、Unicode 和空标签值的碰撞测试。

### PRD-07 / P2 / 证据 B：Audit 写失败可使后续 hash chain 无法验证，filter 多值契约失配

**位置：** `internal/production/audit.go:126-148,169-199,206-251,257-320`。

**触发与后果：** `Log` 在持久化 append 成功前更新 `lastHash`；文件创建/写入失败后，后续记录 PrevHash 指向不存在的一项，链验证失败。另 Operation filter 注释称支持逗号分隔，而 Matches 做整个字符串相等。轮转只留一个 `.1` 并重置 chain；Query/VerifyChain 只读当前文件，审计证据无法跨轮转查询或验证。

**回归建议：** append 成功后再提交 hash state；模拟失败后恢复并 VerifyChain。明确多值 filter 行为；若审计日志用于长期留证，应在轮转文件保存可验证 anchor 并实现跨文件查询。

### PRD-08 / P2 / 证据 B：Runtime stats 只记录 token，CLI 还展示始终为零的指标

**位置：** `internal/production/stats.go:60-84`、`internal/production/llm_integration.go:136-153`、`internal/cli/slash_handlers.go:74-81`。

**触发与后果：** 非测试调用点只记录 token；未发现 runtime `RecordTurn` / `RecordToolCall` 调用。slash command 展示 turns、tool calls、duration、errors 和 cost，用户会将零值误解为真实统计。Stream usage 还旁路现有 Generate 统计路径。

**回归建议：** 明确统计字段的数据 owner，在 agent turn、tool start/result、模型 terminal 和错误边界埋点；没有可靠来源的字段应展示 N/A 而不是 0。

### PRD-09 / P2 / 证据 B：loop detector 的连续失败/编辑计数生命周期不正确

**位置：** `internal/production/loop.go:195-219,254-289`。

**触发与后果：** 即便 runtime event mapping 修好，testFailure 只增加不在成功测试或新 cycle 时清零；注释所称 consecutive failures 并不连续。edit count 也跨整个 session 累积，多次正常编辑可误触发。

**回归建议：** detector 使用明确 session/turn/cycle window 和成功事件重置规则；测试失败—成功—失败不会被当作连续失败，窗口外的正常重复编辑会衰减或清零。

## 建议的 Runtime 迭代顺序

### R0：Provider 与配置解析闭环

建立唯一 provider factory：CLI prompt、serve、list models、ModelCycler、subagent 都解析成同一个有来源的 `ResolvedModelConfig`。它要显式带 provider protocol、model、endpoint、credential reference、sampling 参数、capability 和 fallback policy。Native provider、OpenAI-compatible gateway 的身份不得通过改 provider name 混为一谈。验收：三类原生 provider + 一个 OpenAI-compatible mock gateway 的 endpoint/header/body contract tests；任何 secret 都不能跨 provider host；CLI list/build 结果一致。

### R1：统一 Stream terminal contract 和请求生命周期

为每个请求定义 Started / Delta / ToolCall / Usage / Completed / Failed / Canceled / TimedOut 终态，且只能有一个 terminal event。parser scanner error、畸形 JSON、HTTP body read error、timeout、cancel、EOF-without-final 都进入 Failed/TimedOut；Core 默认拒绝不完整 assistant/tool response。按首 token、工具执行、副作用、final 输出明确 retry 安全边界。验收：httptest SSE server 逐阶段断流/延迟/超长事件/坏 JSON；确认无 goroutine 泄漏、无假成功、无重复工具调用。

### R2：Provider protocol tool loop 和多模态契约

逐 provider 实现 tools schema、tool call id、assistant call、tool result、system/developer 指令和 finish reason 的双向 mapping；无法表示的语义要 typed error。图片 URL 要支持或明确拒绝。验收：OpenAI/Claude/Gemini 使用本地 fixture server 执行至少两轮工具、并发工具调用、工具错误、图文输入；比较同一 canonical Conversation 得到的规范化输出。

### R3：把 resilience policies 收敛到一个可执行请求控制器

统一 structured classification 与 RetryPolicy；准确限定总 attempts、最大总耗时、per-attempt timeout、jitter、Retry-After、request idempotency 和 per-provider circuit breaker。只对安全的操作重试；错误必须能从 Runtime 到 TUI/CLI 清楚呈现。给 failover 注入真实、 credential-scoped candidate registry。验收：结构化 HTTP 429/500/401/403/network、stream failure 和 concurrent circuit-open mock tests；调用次数与 policy 完全一致。

### R4：安全输出、成本与运行审计闭环

为 streaming 定义可见前 guard 策略，采用 buffer/delay 或明确的 typed streaming policy，guard error 明确 fail-closed/fallback semantics；切勿先展示敏感输出、再尝试撤回，也不要假设逐 token regex 等价于完整输出过滤。对 Stream/Generate 统一记 usage、估算来源、成本、turn/tool/error/duration；若产品要求预算，再定义软告警与硬 admission 的配置和运行契约。审计链仅持久化成功后推进 hash，redact/hash 敏感 idempotency data。验收：PII/injection、guard error、stream usage 和日志轮转一致性 mock tests。

### R5：Context recovery 与路由质量

使用 provider tokenizer 或有界估算预算，按完整 conversation/tool exchange 做压缩和重试；保存压缩 provenance，continuation 失败报告 incomplete。修正 ModelCycler provider credential routing/fallback signal 和 ProviderComposer determinism。验收：带 tool call 的历史超限后仍有协议闭合；不同 provider selection 可观测且与 config 一致；composer/registry 冷启动顺序稳定。

## 最小复现与验证记录

- 临时文件：`internal/cli/audit_repro_test.go`。执行 `rtk go test ./internal/cli -run '^TestAudit(ProviderResolutionForClaude|OutputGuardProtectsStream)$' -count=1`，两个 audit repro 均失败：`provider not found: claude`；`unguarded model content escaped through Stream`。这是预期的审计失败，表明重现条件成立，不代表原有测试套件失败。
- 临时文件随后已删除。本记录生成时应确认工作树没有 `internal/cli/audit_repro_test.go`。
- 未执行真实模型请求和完整测试套件。生产审计代理对内部/production 与关联调用点采用源码静态追踪；测试文件存在不等于本轮已运行/通过。

## 源码与测试文件清单

### `internal/llm` 源码清单（25）

`composer.go`、`conversion.go`、`cycler.go`、`default.go`、`llm.go`、`loop_adapter.go`、`middleware_chain.go`、`middleware_failover.go`、`middleware_loopdetect.go`、`middleware_overflow.go`、`middleware_retry.go`、`middleware_sanitize.go`、`middleware_timeout.go`、`middleware_validate.go`、`model_registry.go`、`model_selector.go`、`modelsdev_client.go`、`native.go`、`provider.go`、`provider_error.go`、`registry.go`、`sse_parser.go`、`stream_accumulator.go`、`task_type.go`、`thinking.go`。

### `internal/llm` 测试清单（38，已枚举；本轮未执行全套）

`bench_test.go`、`composer_edges_test.go`、`composer_e2e_test.go`、`composer_extra_test.go`、`composer_test.go`、`conversion_test.go`、`cycler_stream_test.go`、`cycler_test.go`、`e2e_cycler_test.go`、`e2e_thinking_test.go`、`extra_test.go`、`llm_test.go`、`loop_adapter_test.go`、`middleware_chain_test.go`、`middleware_failover_test.go`、`middleware_loopdetect_stream_test.go`、`middleware_loopdetect_test.go`、`middleware_overflow_stream_test.go`、`middleware_overflow_test.go`、`middleware_retry_test.go`、`middleware_sanitize_test.go`、`middleware_timeout_test.go`、`middleware_validate_test.go`、`modelsdev_client_test.go`、`multimodal_test.go`、`native_extra_test.go`、`native_test.go`、`provider_error_test.go`、`provider_extra_test.go`、`provider_hardening_test.go`、`provider_test.go`、`registry_and_conversion_hardening_test.go`、`registry_edges_test.go`、`registry_test.go`、`stream_accumulator_test.go`、`stream_usage_test.go`、`thinking_test.go`。

### `internal/production` 源码清单（17）

`api_key_redaction.go`、`audit.go`、`circuit.go`、`cost_tracking.go`、`idempotent.go`、`llm_integration.go`、`loop.go`、`options.go`、`output_guard.go`、`output_guard_middleware.go`、`output_guard_redact.go`、`output_guard_registry.go`、`prompt_injection_guard.go`、`registry.go`、`retry.go`、`stats.go`、`telemetry.go`。

### `internal/production` 测试清单（21，已枚举；本轮未执行全套）

`api_key_redaction_test.go`、`audit_test.go`、`circuit_test.go`、`cost_tracking_subagent_test.go`、`cost_tracking_test.go`、`idempotent_test.go`、`llm_integration_test.go`、`loop_test.go`、`output_guard_extra_test.go`、`output_guard_redact_test.go`、`output_guard_registry_test.go`、`output_guard_security_test.go`、`output_guard_test.go`、`prompt_injection_guard_test.go`、`production_e2e_test.go`、`registry_test.go`、`resilience_extra_test.go`、`resilience_hardening_test.go`、`retry_test.go`、`stats_test.go`、`telemetry_test.go`。

### Runtime 接线审计的额外文件

`internal/cli/assemble.go`、`internal/cli/output_guard_adapter.go`、`internal/cli/prompt.go`、`internal/cli/slash_handlers.go`、`internal/core/loop.go`、`internal/core/types.go`，以及 `internal/llm/llm.go`、`internal/llm/middleware_retry.go`、`internal/llm/provider_error.go`。报告所列实际调用链依赖这些文件的行级证据。Core 其他子系统的逐文件覆盖由总审计中的对应分项报告负责。
