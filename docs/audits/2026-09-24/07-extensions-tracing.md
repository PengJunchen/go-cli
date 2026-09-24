# Extension、MCP、Skill 与 Tracing 源码审计

审计日期：2026-09-24  
范围：`internal/extension`、`internal/mcp`、`internal/skill`、`internal/tracing` 的全部源码与测试，以及 `cmd/cli`、`internal/cli`、`internal/core`、`internal/config` 中的实际接线。  
证据：源码静态追踪和测试代码审阅；没有运行测试、benchmark、第三方 MCP 互通验证或生产运行时复现。下文“确认”表示源码路径可以直接推出该结果，不代表已经做动态复现。

## 结论

四个包的基础构件质量不均但有不少可取之处：skill/MCP 注册表有并发保护，MCP/插件 HTTP 有响应体上限与默认 SSRF 安全 client，MCP 工具输出有注入模式包装，tracing 有 JSONL、脱敏 wrapper、异步队列和关闭接口。当前最需要处理的是凭据边界、请求可取消性、工具副作用重放、trace 默认落盘的隐私风险，以及 extension hooks/middleware 的确定性和错误反馈。

MCP 的 `OfficialSDKAdapter`、`Mark3labsAdapter` 均复用本仓自写的 `JSONRPCLineTransport`（`internal/mcp/adapter.go:18-25,515-541`），没有导入对应官方 SDK。stdio 的 JSON-RPC 行协议有基本实现，但本次没有外部服务互通证据；HTTP/SSE 则明确只做了自定义子集，不能据当前测试声称标准 MCP transport 互通。测试 server 都由本仓测试代码搭建，没有标准 server/client conformance fixture。

Tracing 配置默认开启 JSONL（`internal/config/loader.go:281-292`）。CLI prompt 原文会进入 span attribute 和 slog 文件，脱敏 wrapper 不覆盖未标记为 sensitive 的值；`exporter: none` 也无法一致关闭 AssembleAgent 内部 tracer。CLI 根 tracer 和 AssembleAgent 又各建一套 trace ID/exporter，导致父子 span 跨 trace、分落两个文件。

## 发现

### MCP

| ID / 优先级 | 类型 | 证据与影响 | 建议回归验证 |
|---|---|---|---|
| MCP-01 / P1 | 确认缺陷：HTTP 凭据可能被跨源转发 | `HTTPClientAdapter.Connect` 从初始 SSE 响应接受 `event: endpoint` 内的绝对 URL，并直接赋给 `a.endpoint`，没有校验 scheme/host/origin（`internal/mcp/http_adapter.go:124-157`）。后续 JSON-RPC POST 会向该地址发送配置的自定义 header、Bearer/OAuth token（`249-267,330-343`）。配置可信服务可将客户端引到攻击者控制的 HTTPS 服务，使 MCP 请求内容和凭据离开配置的 origin。默认 SSRF 防护不阻挡公开地址。CLI 当前还遗漏这些凭据字段（见 MCP-04），但 adapter 公共 API 本身允许启用凭据。 | 两个 `httptest` server：源站返回攻击者域名 endpoint event；第二站记录 POST。带 Bearer/custom header 调用 `Connect`、`ListTools`，断言绝不请求第二站或 Connect 拒绝跨 origin。覆盖相对 URL、同 origin、端口变化和 HTTPS downgrade。 |
| MCP-02 / P1 | 确认缺陷：断线后重放非幂等工具调用 | `CallTool` 对 `tools/call` 使用通用 `callWithReconnect`（`internal/mcp/adapter.go:439-443`）；连接错误后重连并以相同 method/params 重试（`359-395`），没有幂等键或“结果未知”状态。若远端已执行副作用但响应在网络中丢失，发送邮件、写工单等工具会被执行两次。当前 reconnect 测试验证连接失败/重连，但未模拟“执行一次后丢掉第一次响应”。 | 假 MCP server 在首次 `tools/call` 增加执行计数后丢连接；验证客户端不自动重放，或使用明确幂等 token 才能重试，并将结果标成 unknown/reconciliation required。 |
| MCP-03 / P1 | 确认缺陷：stdio context 无法中断阻塞读取 | `Request` 在读前检查 `ctx.Done()`，随后直接同步执行 `ReadString('\n')`（`internal/mcp/adapter.go:84-99`）。若 pipe 保持打开却没有任何字节，超时/取消不会唤醒 ReadString；同一 transport 的 mutex 还会让后续请求排队（`65-67`）。现有预取消测试在进入 read 前便取消（`internal/mcp/adapter_test.go:258-277`）；deadline 测试持续发送不匹配行，让循环能再次检查 context（`453-484`），没有覆盖静默阻塞。单行 JSON 也没有大小上限。 | 用无数据的 `io.Pipe` 发起 Request，等确认进入 read 后取消 ctx，要求有界时间内返回且无 goroutine 泄漏；另测未匹配的大行上限与排队请求取消。 |
| MCP-04 / P2 | 确认缺陷：CLI 未暴露 MCP HTTP 认证配置；MCP 客户端没有统一 teardown owner | 低层 `mcp.MCPServerConfig` 支持 `Headers`、`BearerToken`、`OAuthConfig`，但面向用户的 `config.MCPServerConfig`/MCP JSON shape 只声明 Name/Command/Args/URL/Env（`internal/config/types.go:154-171`），assembly 也只映射这五项（`internal/cli/assemble.go:1677-1699`）。因此这是 CLI 尚未暴露认证能力，不是已声明配置被丢弃。另 Connect 成功、ListTools 失败时直接返回而不 Disconnect；只有 ListTools 成功的客户端才放入 `connected`（`assemble.go:1701-1717`）。最终清理只停 hot reloader，不 disconnect MCP clients（`631-655`）；主配置路径返回空 config path，因而也不会建 hot reloader（`1783-1786`）。CLI 一次性进程退出时 CommandContext 最终会杀掉 stdio child，但 `AgentAssembly.Cleanup` 和失败路径本身不释放资源；作为库调用时父 ctx 仍活着即可观察到子进程残留。 | 决定 CLI 是否支持凭据：若支持则端到端配置到 adapter；若不支持则文档说明 adapter API 限制。每条 Connect/ListTools/注册失败、正常 Cleanup、ctx cancel 路径均断言 Disconnect 恰好一次并回收 child。 |
| MCP-05 / P2 | 确认缺陷：hot reload 观察文件变化但不采用新配置，Stop 未等待全部重连工作 | watcher 只读取 hash 并触发旧 client `Disconnect→Connect→ListTools`（`internal/mcp/hot_reload.go:289-307,403-419`）；重新创建 client 时的 command/url/env 来自 assembly 一次性转换（`internal/cli/assemble.go:1677-1699`），未重新解析 config。因此修改 endpoint、command、args、env 实际只重启旧配置。另 `scheduleReconnect` 启动未加入 WaitGroup 的 goroutine（`hot_reload.go:313-330`），`Stop` 只等 poller（`216-235`）；重试 timer 分支不选 `ctx.Done()`（`350-363`）。Stop 可能在 Connect/ListTools/注册回调仍运行时返回。 | 改 command/url/env 后断言新客户端配置生效；在 Connect 阻塞和重试等待期间 Stop，断言返回后没有任何后台调用或 tool re-registration。 |
| MCP-06 / P2 | 确认不支持当前 MCP 版本；旧版互通未验证，GET 400 测试有假阳性 | 客户端只声明 `2024-11-05`、`2025-03-26`、`2025-06-18`（`internal/mcp/mcp.go:13-23`），不支持当前 `2025-11-25` handshake 版或 `2026-07-28` modern 版。当前版已移除 initialize handshake 和 GET stream，并要求每个 HTTP POST 携带版本元数据和 `Mcp-Method`、适用时的 `Mcp-Name`；本实现固定 initialize、GET 探测 + POST，不发送这些请求元数据 header，也没有按版本分支（`internal/mcp/http_adapter.go:108-157,297-319`；[当前 Streamable HTTP 规范](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)、[版本兼容规范](https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning)）。旧版 Streamable HTTP（2025-03 至 2025-11）也要求初始化后按会话携带 session ID，本实现未保存响应 header；旧版互通需要实测。对 legacy HTTP+SSE 的 GET event endpoint 做了子集支持，但初始 SSE 读到 endpoint 后即关闭，不维持长连接（`124-154`）。此外 GET 网络错误时直接置 `connected=true` 而跳过 initialize（`116-122`），测试把 GET 400 后 direct POST 可用当成 Connect 成功（`internal/mcp/http_adapter_test.go:191-207`），不能据此证明标准连接有效。 | 实现 current/legacy 显式版本探测与 transport 分支；用官方协议向量及至少一个外部 server/client 覆盖 modern headers/discovery、2025 session header、legacy endpoint event、错误 status/id、SSE 多事件/取消。连接探测应验证握手或能力响应，不把任意 GET 失败算连接成功。 |
| MCP-07 / P2 | 确认缺陷：MCP `isError` 没有传给 core ToolResult | 适配器记录 trace 的 success 时考虑 `result.IsError`，但构造 `tools.ToolResult` 只传 Output/ToolCallID（`internal/mcp/tool_adapter.go:128-158`），因此远端的 `isError:true` 会呈现为普通成功输出。该层也未处理 client 返回 `nil, nil`，随后访问 `result.Content` 会 panic（`141-144`）；interface 没禁止此返回。 | MCP client 返回 IsError true 时断言 ToolResult.IsError 且 span 为错误；返回 nil,nil 时要求得到明确定义的工具错误，不 panic。 |
| MCP-08 / P2 | 未证实互通；标识符映射不能无损 round trip | `NormalizeToolName` 以 `mcp__{server}__{tool}` 拼接；`ParseToolName` 固定把首个 `__` 前的片段当 server（`internal/mcp/mcp.go:132-152`）。测试同时宣称 server 可包含 `__`（`internal/mcp/name_test.go:31-36`），但该输入不能还原原 server/tool 边界；例如 `(a__b,c)` 和 `(a,b__c)` 产生相同注册名。 | 对任意 server/tool 名做 property/fuzz round-trip；协议允许的名称要么编码边界，要么验证拒绝歧义输入。 |
| MCP-09 / P2 | 确认缺陷：MCP 外层工具 span 没有成为远端调用 span 的 parent | `MCPToolAdapter.Execute` 创建 `tool.call` span 但丢弃返回的 span context，然后把原 `ctx` 传给 `client.CallTool`（`internal/mcp/tool_adapter.go:128-136`）。实际 `adapterCore.CallTool` 再从传入 context 创建 `mcp.tool.call`（`internal/mcp/adapter.go:429-437`），两 span 成为同级而非嵌套，调用树无法表达 adapter→remote transport。 | 导出 span 后断言 `mcp.tool.call.ParentSpanID == tool.call.SpanID`，并覆盖工具错误/重试。 |

### Extension

| ID / 优先级 | 类型 | 证据与影响 | 建议回归验证 |
|---|---|---|---|
| EXT-01 / P1 | 确认缺陷：hook 与 middleware 次序不确定 | `DefaultExtensionRegistry` 用 map 保存条目，并由 map range 生成 `AllHooks`/`AllMiddleware`（`internal/extension/registry.go:167-186`）。CLI 把返回 slice 原序装进 `HookChain` 和 `MiddlewareChain`（`internal/cli/assemble.go:931-935,1388-1399`）；核心明确 first-listed middleware 是最外层（`internal/core/middleware.go:77-96`），HookChain 也按次序执行（`internal/core/hook.go:51-68`）。相同 plugins 每次启动可能出现不同前后置行为，影响拦截与策略叠加。 | 用多个有可观察副作用的 hook/middleware，以固定注册顺序重复读取并断言顺序不变；实现中保存注册序列。 |
| EXT-02 / P2 | 确认缺陷：扩展生命周期错误丢失；失败 Init 后仍调用 Shutdown | `PluginManager.Init`、`Shutdown` 记录 warning 但始终返回 nil（`internal/extension/plugin_manager.go:57-87`），CLI 又忽略 Load/Init 返回值（`internal/cli/assemble.go:921-925`），调用者无法知道部分失败或把装配标成降级。Shutdown 对所有已 Load 扩展调用，包括 Init 失败的扩展（`plugin_manager.go:76-85`）。协调器仅在成功之后写 Running 状态（`internal/extension/manager.go:60-74`），但 shutdown 不检查当前状态（`79-95`），重复 Shutdown 会重复执行扩展副作用。现有 `TestPluginManagerInitPartialFailure` 明确要求 Init 返回 nil（`plugin_manager_test.go:218-231`），属于已锁定的吞错语义。 | 聚合并返回阶段性错误；只 teardown 成功初始化的对象；覆盖同名并发 Init/Shutdown、重复 Shutdown 和部分失败组装结果。 |
| EXT-03 / P2 | 部分能力仅有写 API，没有运行时读/接线 | ExtensionRegistry 声明可注册 Command 和 Provider（`internal/extension/registry.go:20-30`），读取函数为包内私有且无 AllCommands/AllProviders（`128-140`）；PluginManager/CLI 只取 Tools/Hooks/Middleware（`plugin_manager.go:106-130`；`assemble.go:926-936`）。这些 command/provider 经插件注册后不可从公开 manager 取得，也不会进入 CLI/provider runtime；标为“代码存在、实际 dormant”，除非文档明确限定此 registry 是供扩展内部使用。 | 若要支持，则公开只读快照并接入命令/provider 装配；若不在产品范围，文档标 unsupported 并退役相应注册方法。 |
| EXT-04 / P2 | HTTP“远端扩展”只读 descriptor，不执行远端扩展 | HTTP bundle 只包含 name，loader 为每个名字构建 `rpcExtension`（`internal/extension/plugin.go:166-174,249-261`）；`rpcExtension.Init/Shutdown` 只日志、没有 RPC/tool/hook 注册（`144-164`）。`grpc://` 明确返回 unsupported（`34-39,93-105`）。因此远端可被列举但不是可运行插件。此处是实现范围不足，不是 SSRF 缺陷：默认 HTTP client 有 HTTPS/loopback gate、dial-time SSRF 检查和 10 MiB 限制（`202-247`）。 | 将远端方式标为 descriptor discovery，或定义并集成有认证/协议/超时/取消的 RPC 执行契约；验证远端注册的 tool 可以真实调用。 |
| EXT-05 / P2 | 生命周期并发与 observer 数据隔离不完整 | `extensionCoordinator.initExtension` 在调用用户 `Init` 前不占据“initializing”状态、调用外部代码时不串行同名 lifecycle，结束后才写 map（`internal/extension/manager.go:60-74`）；并发同名 Init 可同时执行并覆盖状态。`HookEvent` 声明 immutable，但 `Data` 类型为 `any`（`internal/extension/types.go:73-83`）；adapter 把 `Submission` 结构体值直接塞进 Data（`internal/cli/extension_bridge.go:42-49,58-69`），其 Metadata map 与 History slice 仍共享底层数据（`internal/core/types.go:127-140`）。扩展 observer 修改 map/slice 会产生未通过 HookResult 表达的副作用。Hooks/middleware/plugins 全在主进程执行，Go `.so` 是任意本地代码，框架本身不构成沙箱隔离（`internal/extension/plugin.go:108-135`）。 | 加入同名并发状态机测试；传递只读/深拷贝事件 DTO 并测试原 Submission 不变；文档明确 `.so` 是可信代码边界，远端插件必须进程隔离才可视为不可信。 |
| EXT-06 / P3 | 确认不完整：Hook action 语义未实现；default config provider 是空壳 | `HookActionBlock/Terminate/Replace` 都是未导出的常量（`internal/extension/types.go:59-70`），包外插件难以引用。即使构造非 Pass action，CLI bridge 也一概转换成 generic error，不区分 block/terminate/replace 或使用 Replacement（`extension_bridge.go:39-73`）。`defaultConfigProvider.Load/Watch` 固定返回 not implemented（`internal/extension/default.go:9-32`），未发现其生产注册/读取路径。 | 明确 hook action 真正支持的集合；移除未支持公开字段或实现语义；默认 config provider 要么有实际 backend，要么不作为可用默认实现暴露。 |
| EXT-07 / P3 | 确认缺陷：EventBridge 取消时可能丢失已读事件 | Bridge 文档声称已从 source 读取的事件一定送达（`internal/extension/bridge.go:18-27,55-59`），但实现读取后在 `out <- ev` 与 `ctx.Done()` 间 select，取消时返回（`63-75`）；缓冲 64 项满时，已经从 source 消费的那条 event 会丢。现有 no-loss 测试不覆盖取消边界（`internal/extension/bridge_test.go:52-85`）。 | 明确 shutdown 语义是 drain 还是 abandon；用满缓冲区后取消，验证可保证交付的事件有界 drain，或更新契约/统计丢弃。 |

### Skill

| ID / 优先级 | 类型 | 证据与影响 | 建议回归验证 |
|---|---|---|---|
| SKILL-01 / P2 | 不完善：自定义 frontmatter 子集缺少资源/路径策略 | loader 用 Scanner 读全文件并累加所有行，仅限制单行 4 MiB，没有总字节/文件数/总技能数上限（`internal/skill/skill_loader.go:107-118,141-180`）。context 只在 Open 前和 Walk 起始检查；一次文件读取及每个 WalkDir 项都不检查取消（`92-118,146-169`）。手写 parser 只认简单冒号 scalar、列表、`prompt: |` 和一级 parameters（`251-359`），不处理 YAML quoting/escape/comment/folded scalars 等常用语法；BOM 测试明确固定为失败（`internal/skill/skill_hardening_test.go:96-109`）。WalkDir 不跟随目录 symlink，但 Load 使用 os.Open，因此树内 `.md/.yaml` 文件 symlink 可指到配置目录外（`98-105,151-169`）。这不是完整 YAML loader，应该明确范围并限制资源/路径。 | 总大小、文件数和 cancellation 上限；对文件 symlink root escape 拒绝；有效 YAML fixture（含注释、冒号、引号、BOM、block scalar）与无效 fixture；保留“坏技能跳过”时输出可审计汇总。 |
| SKILL-02 / P2 | 确认不完整：参数和 declared tools 只是描述，执行完全忽略 args | `SkillAdapter.Description` 把 tool 名和参数名拼入自然语言（`internal/skill/skill_adapter.go:34-52`）；`Execute` 不读 `call.Args`，只返回固定 prompt 与 metadata（`57-97`），也未实现 `tools.Parameterized` 的 schema 接口。技能定义中的工具名既不做校验，也不约束 agent 的全局工具集。若用户期待 skill parameter schema 或 least-privilege tools，这些字段现在不会产生约束效果。 | 建立端到端调用测试：向 skill 传参数、缺参/错型应校验；model 可见结构 schema；declared tool allowlist 若作为安全/能力边界必须由 dispatcher 强制，而不是只写描述。 |
| SKILL-03 / P2 | 确认缺陷：skill metadata 原样插入 XML prompt | system prompt 以字符串插入 name/category；description 用 Go `%q`，不是 XML escaping（`internal/core/system_prompt.go:268-281`）。用户可编辑的 skill 文件可包含引号、`/>`、`</skills>` 等，令模型看到结构损坏或伪造条目。项目 auto-discovered skills 有 trust gate，但明确配置目录跳过此 gate（`internal/cli/assemble.go:1853-1874`）。 | 对 XML attribute 做 `xml.EscapeText` 或改用可靠结构序列化；恶意 name/category/description 输入测试保证不能闭合节点/注入额外 skill。 |
| SKILL-04 / P3 | 已实现能力 dormant：Registry 的注入/匹配接口没进入 runtime | `SkillRegistry` 有并发注册、分类、匹配和注销（`internal/skill/skill_registry.go`），但 CLI 每次直接 `NewYAMLSkillLoader`、`LoadDir` 并注册所有 skill tool（`internal/cli/assemble.go:1877-1907`），没有调用 `GetSkillLoader`、`GetSkillRegistry`、`Match`。全局替换 API 仅在测试中使用。现 runtime 是“启动加载全部技能作为工具”，不是 progressive disclosure 的运行时匹配；应以文档支持边界标注，避免误认为已连入 agent loop。 | 若保留匹配能力则用用户意图/工具搜索接入并测量注入规模；若保持全部 skills 为工具，删除/注明未接入的全局 registry abstraction。 |

### Tracing

| ID / 优先级 | 类型 | 证据与影响 | 建议回归验证 |
|---|---|---|---|
| TRACE-01 / P1 | 确认缺陷：不同入口将完整用户 prompt 持久化，敏感标记没有覆盖实际 main tracer | 默认配置为 enabled=true、exporter=jsonl（`internal/config/loader.go:28-29,281-292`）。one-shot `promptCmd.Run` 把包含原文的 `args` 写入 `prompt.run` attr（`internal/cli/prompt.go:94-99`），其 exporter 来自 main `newTracing`，没有 RedactingExporter（`cmd/cli/main.go:115-147`）。随后 Harness 又把 `user_message` 写入 span，并在 slog 写全文 `msg`（`internal/core/harness.go:131-135`）；assembly tracer虽包 RedactingExporter，但这项 attribute 没设 `Sensitive`，不会被遮盖（`internal/cli/assemble.go:761-773`；`internal/tracing/exporter.go:118-150`）。REPL 的 `interactive.turn` span 使用 `SensitiveAttribute`（`internal/cli/repl_session.go:649-654`），但上下文来自 main tracer，REPL 通过 `TurnRunner.RunTurn` 直接运行（`231,698-705`），Sensitive bit 不会自动脱敏。main 把 slog 持久化到 `.go-cli/logs/go-cli.log`（`cmd/cli/main.go:47-52`）；JSONL 创建失败时还会保留默认 stdout exporter且不报错（`127-140`）。所以 prompt.run args、Harness user_message/msg，以及 REPL turn 输入仍可能落 trace/log 或终端；assembly 的脱敏不是全局保护。 | 默认禁记内容、默认 redact 或显式 opt-in；创建 secret sentinel，分别走 `prompt` 和 REPL 入口，检查所有 trace JSONL、stdout 和 slog 文件均不含原文；模拟 trace 文件打开失败并确认不会退化为明文 stdout；验证 SensitiveAttribute 确实经过 RedactingExporter。 |
| TRACE-02 / P2 | 确认缺陷：CLI 与组装层创建不同 tracer，跨 trace parent 链断裂 | main 创建 tracer/root span，prompt/dispatch 子 span由它创建（`cmd/cli/main.go:61-77`；`internal/cli/prompt.go:94-110`）；AssembleAgent 又建 `NewTracer(s.sessionID, ...)`（`internal/cli/assemble.go:761-773`），Harness 注入第二个 tracer（`359`、`internal/core/harness.go:123-129`）。`Tracer.Start` 使用自身 traceID，但仅从父 context 取 parent spanID（`internal/tracing/span.go:112-134`），导致 harness traceID 与 parent span 属于不同 trace；两 tracer 还各自写不同文件（main 按时间戳命名，assembly 用 sessionID）。`LoadTrace` 单文件重建，无法把父子接回；读树又以 SpanID 单独索引且 map 遍历产生不确定顺序（`internal/tracing/load.go:48-85`）。这是可直接验证的 observability/data grouping 错误，但没有证明会阻断业务流程。 | 由一个 runtime-owned Tracer/Exporter 从 invocation 贯穿 Assemble/Harness/工具；验证所有 span traceID 相同且 LoadTrace 能跨业务完成重建；树索引使用 `(trace_id, span_id)`。 |
| TRACE-03 / P1 | 确认缺陷：none 与实际 builder 分歧；可用 exporter 接线不一致 | CLI `newTracing` 在 exporter=none 时返回 nil（`cmd/cli/main.go:118-125`），但 AssembleAgent 仍仅按 Enabled 判断并调用 builder；builder 的 default 分支把 none 当 JSONL（`internal/cli/assemble.go:761-775,2188-2215`）。所以 CLI 无 root tracer 仍可能由 assembly 记录 JSONL。validator 允许 `jsonl/stdout/none`，拒绝 `otlp`（`internal/config/validator.go:41-44`）；main 的 builder 只分流 stdout/jsonl，未知值默认 stdout（`cmd/cli/main.go:127-144`）；另一个 builder 有 OTLP 分支而 kafka builder 完全未接入。这造成重复且不一致的配置路径。 | 对 Enabled×Exporter 的组合做配置/组装测试，none 必须在所有层完全不写出；选定唯一 builder，只有配置 validator 接受且文档列出的 exporter 才能构造。 |
| TRACE-04 / P2 | 确认缺陷/设计冲突：同步落盘与静默 drop 同时存在 | 单个 span `End` 同步调用 exporter，等待最多 5 秒（`internal/tracing/span.go:253-272`）；AssembleAgent 的 JSONL exporter 没包 AsyncExporter（`internal/cli/assemble.go:761-773`），因此热路径会等 I/O。main root 才包异步 exporter（`cmd/cli/main.go:142-147`）。异步队列满时丢 span、仅 slog warning 且返回 nil（`internal/tracing/async.go:57-66`），不符合 `TraceExporter` 声明的 at-least-once delivery（`internal/tracing/exporter.go:16-25`）；Shutdown 后仍可继续成功入队，worker 已退出则这些新 span永远无人处理（`async.go:119-124`）。Shutdown 等待 worker 时不观察传入 ctx，队列中每个 span 的单独 flush 又可等 5 秒（`async.go:75-84,119-124`），高负载下关闭时间可能很长。无阻塞目标可以接受有界采样/丢弃，但必须公开丢弃计数并定义关闭后的拒收语义。 | 队列满返回可计量 error/drop counter；Shutdown 使用状态锁拒绝新 span，并在 deadline 内完成 flush；以慢 exporter 测延迟、丢失计数及并发 shutdown 行为。 |
| TRACE-05 / P2 | 确认缺陷：网络 exporter 失败后整批被丢弃；两个 exporter 的协议标识超出实际支持 | OTLP flush 在发送前清空 buffer（`internal/tracing/exporter_otlp.go:147-158`），失败后只记日志不回队；Kafka 同样清空 buffer 后直接 send（`internal/tracing/exporter_kafka.go:139-152`）。`KafkaTraceExporter` 文档说明它是长度前缀 JSON-over-TCP，不是 Kafka wire protocol；`snappy/lz4` 实际用 gzip 但 frame 仍宣告原算法（`exporter_kafka.go:18-29,190-193`）。写 frame 对 header/body 各调用一次 `conn.Write` 且忽略 n（`224-247`），通用 Writer 可短写。OTLP/HTTP 可以发送 Protobuf JSON，但当前发的是顶层 `{spans:[...]}`，其对象为 `SpanData` 的 snake_case `trace_id/span_id` 等字段（`internal/tracing/exporter_otlp.go:16-29,168-181`；`internal/tracing/tracing.go:55-68`），不是 `ExportTraceServiceRequest` 的 Protobuf JSON 表达（[OTLP specification](https://opentelemetry.io/docs/specs/otlp/)）。因此当前 exporter 不具标准 OTLP JSON 兼容性。 | 改名为 internal/debug sinks，或实现 OTLP Protobuf/Protobuf JSON 与真实 Kafka wire protocol；失败 batch 保留、重试并具备边界；`writeFull`/`io.Copy`；启动真实 collector/broker conformance test。 |
| TRACE-06 / P2 | 确认缺陷：错误 span 默认被写成 OK，MCP 失败分支只记 success=false | `localSpan.ToSpanData` 把未设置状态转换成 `SpanStatusOK`（`internal/tracing/span.go:275-295`）。MCP `CallTool` 错误分支和 tool adapter 错误分支只设 success=false、不设置 Error 状态（`internal/mcp/adapter.go:439-447`；`internal/mcp/tool_adapter.go:128-136`）；hot reload 同样如此（`internal/mcp/hot_reload.go:403-415`）。检索这些 export 路径后，trace 常显示 OK 即使工具失败。 | 统一 span helper：任何 error return 设置 Error/status message；针对 MCP connect/list/call/HTTP status 失败验证导出 SpanData 状态。 |

## Dormant 与受支持边界

以下功能有源码/测试，但本次没有找到完整的产品接线。建议在 roadmap 与 CLI help 上区分“库能力”与“当前 runtime 功能”。

| 能力 | 当前状态 | 依据 |
|---|---|---|
| Extension Command/Provider 注册 | 已定义/可写，PluginManager/CLI 无 getter 和装配 | `internal/extension/registry.go:20-30,128-140`; `plugin_manager.go:106-130`; `internal/cli/assemble.go:919-941` |
| HTTP/RPC Extension 执行 | HTTP 只返回 no-op `rpcExtension`；gRPC unsupported | `internal/extension/plugin.go:34-39,144-164,193-261` |
| Extension config provider 默认实现 | 默认 Load/Watch 总返回 not implemented，未见运行时 consumer | `internal/extension/default.go:9-32` |
| MCPClientRegistry | Registry 测试完整，但生产组装直接闭包持有 client，没有使用这个 registry | `internal/mcp/registry.go:13-110`; `internal/cli/assemble.go:1660-1734` |
| Skill 全局 loader/registry 与 Match | 测试调用；CLI 创建 loader、读取所有 skill 并全部注册为 deferred tool | `internal/skill/registry.go`; `internal/cli/assemble.go:1845-1907` |
| Async exporter | 仅 CLI root tracer 使用；Assembly 直接同步 exporter | `cmd/cli/main.go:142-147`; `internal/cli/assemble.go:761-773` |
| Kafka tracing exporter | 实现/测试存在，没有 CLI/config builder 调用 | `internal/tracing/exporter_kafka.go`; 全仓非测试代码无 `NewKafkaTraceExporter` 调用 |
| `LoadTrace` 追踪树 | 工具函数/测试存在，当前 CLI 没有 trace viewer 调用 | `internal/tracing/load.go`; 全仓非测试代码无 `LoadTrace` 调用 |
| `no_plugin` build tag | 有可编译 stub；本次未运行 build-tag 组合验证 | `internal/extension/plugin_stub.go` |

Extension 执行边界必须明确：本地 `.so` factory 运行在当前进程，MCP 子进程才是单独进程；manager/registry 没有强制隔离任意插件代码的能力。按可信配置插件运行是可接受的设计取舍，但不应对不可信第三方插件宣称沙箱隔离。远端 descriptor-only 插件和 `no_plugin` stub 需要明确标注为当前不支持执行。

## 可接受/合理的设计

- `DefaultSkillRegistry` 和 `MCPClientRegistry` 对读写同步，并保留注册顺序；MCP `WithConnection` 可将 wire adapter 与连接实现分离，便于测试。
- MCP HTTP body 有上限，初始 SSE scanner 也设置上限；MCP/Plugin 默认 HTTP client 使用 DNS/dial-time SSRF 防护；MCP tool 对检测到的 prompt injection 会包装成 untrusted external content。这些是值得保留的防线，但不能抵消 endpoint 跨源发送配置凭据的问题。
- Skill loader 允许目录里坏文件跳过、单文件 Load 返回错误，适合部分可用的 skill 目录；需要同时报告跳过数量和原因，避免静默缺功能。
- Trace JSONL 使用 0600 文件权限，目录 0750；`RedactingExporter` 在副本上处理 attribute/event，避免改动原 span。当前问题是敏感字段缺乏默认保护，且 root tracer 没套这层。
- PluginManager 逐个加载/初始化，单个扩展失败不会阻止其它插件，这是合理的部分可用策略；前提是返回聚合错误/可查询健康状态，而不是把部分失败伪装成全成功。

## 源码与测试逐文件覆盖

下表表示逐文件读取并纳入结构/调用关系审计。它不等于执行了相应测试。

### Extension

| 文件 | 覆盖/缺口 |
|---|---|
| `internal/extension/bridge.go` | FIFO 转发与取消关闭路径；取消发生在 source 已读、destination 阻塞时会丢掉已取事件，与“不丢失”注释不一致。 |
| `default.go`, `extension.go`, `types.go` | 默认对象、config contract、extension/hook action 数据类型；config 默认 provider 是未实现；包外 hook action 可用性不足。 |
| `manager.go`, `plugin_manager.go` | coordinator 并发状态、聚合错误、init/shutdown 配对；当前只测试不同 extension 并发，同名 lifecycle 未测。 |
| `middleware.go`, `registry.go` | 包装与注册/get；hook/middleware map 枚举顺序不确定；commands/providers 无公开消费端。 |
| `plugin.go`, `plugin_stub.go` | `.so` 工厂、HTTP descriptor、gRPC 不支持、SSRF/size-limit、no_plugin stub；没有远程实际执行或隔离。 |
| `bridge_test.go`, `extension_extended_test.go`, `types_test.go`, `lifecycle_test.go` | event FIFO、middleware onion、action/value 与 registry 顺序；缺取消边界和包外 action/随机 map order。 |
| `config_internal_test.go`, `config_provider_test.go` | config 数据结构和测试 provider 的 load/watch；没有生产 provider 接线测试。 |
| `coordinator_extended_test.go`, `manager_test.go`, `plugin_manager_test.go` | 状态、span、partial failure、reverse shutdown；测试明确接受 Init partial failure 返回 nil。 |
| `extension_extended_test.go`, `middleware_intercept_test.go` | pass-through/middleware interception 与顺序；没有从 CLI assembled runtime 运行 plugin hook/middleware 的 E2E。 |
| `plugin_extended_test.go`, `plugin_ssrf_test.go`, `plugin_test.go` | HTTP descriptor、错误、SSRF、大小限制；未测远端 extension 执行（实际实现 no-op），未测 `no_plugin` build。 |
| `registry_extended_test.go`, `registry_test.go` | 重复注册、并发读写、getter；没有 AllHooks/AllMiddleware 确定性顺序测试。 |

### MCP

| 文件 | 覆盖/缺口 |
|---|---|
| `internal/mcp/adapter.go` | 自制 stdio JSON-RPC transport、process connect/disconnect、initialize、reconnect；阻塞 read cancellation、JSON 行大小限制、初始化失败 Wait、非幂等 replay 缺口。 |
| `mcp.go`, `registry.go` | 类型、version/name helper、client registry；名称编码边界有歧义，生产组装未使用 registry。 |
| `http_adapter.go` | 自定义 GET/SSE+POST、HTTP auth、OAuth、response parsing；跨源 endpoint 凭据、状态/id/header 校验、SSE 生命周期不完整。 |
| `tool_adapter.go` | JSON schema 暴露、工具适配、prompt-injection 包装；IsError/nil result 没映射。 |
| `hot_reload.go` | hash poll、backoff、reload cycle、global registry；只重连旧配置、后台 goroutine Stop join 缺失。 |
| `adapter_test.go`, `initialize_test.go`, `mcp_reconnect_test.go` | stdio、初始化版本、取消预检、持续帧 deadline、重连；缺静默阻塞和执行后断线重放。 |
| `http_adapter_test.go` | SSE parse、400 direct POST、OAuth state、body size、SSRF；400 假阳性与 endpoint cross-origin credential test 缺失。 |
| `hot_reload_test.go` | 文件变化、重试、Stop poller、span、registry；没有断言新 config 生效或等到所有 reconnect work 完成。 |
| `name_test.go`, `registry_test.go`, `tool_adapter_test.go` | 命名、registry concurrency、adapter/injection；server 名双下划线测试只能拆分但不能 round-trip。 |

### Skill

| 文件 | 覆盖/缺口 |
|---|---|
| `internal/skill/skill.go` | 定义和 option 构造；slice/map getter 可被外部修改，option 只浅复制参数 map。 |
| `skill_loader.go` | 扫目录与手写 YAML subset；总体资源限制、逐项取消、symlink 根约束、YAML 完整性不足。 |
| `skill_adapter.go` | 工具元数据/执行；参数没有 schema/validation，declared tools 不 enforcement。 |
| `skill_registry.go`, `registry.go` | 注册、分类、匹配、全局替换；组件不进入 CLI runtime，定义对象仍可变。 |
| `skill_test.go`, `skill_extra_test.go` | 正常解析、匹配、注册并发、metadata；测试中的声明 metadata 只是展示，未验证实际约束。 |
| `skill_hardening_test.go`, `skill_loader_internal_edges_test.go` | 目录边界、BOM、parser edge、symlink/文件失败场景一部分；BOM 当前被断言为失败，缺总资源限制和 root escape。 |
| `skill_internal_test.go`, `skill_registry_edges_test.go` | 内部 parser/registry edge；未测并发读取外部可变 map/slice。 |

### Tracing

| 文件 | 覆盖/缺口 |
|---|---|
| `internal/tracing/tracing.go`, `span.go` | tracer/context/span/status、同步 End export；trace id 固定在 tracer 上，跨 tracer parent id 可错链，error 默认 OK。 |
| `exporter.go`, `exporter_config.go` | JSONL/stdout、多 sink/redaction/config；只根据 Sensitive bit redact，没有默认 prompt 策略。 |
| `async.go` | bounded queue、batch、flush/shutdown；queue drop nil、shutdown 后仍接受、worker 每个 span单独 flush。 |
| `exporter_otlp.go`, `exporter_kafka.go` | 两种非标准 wire-format 实现；失败整批丢失，不能和标准 collector/broker 互通；kafka frame 写入未处理 short write。 |
| `load.go`, `slog.go` | JSONL 重建和 trace-aware log；LoadTrace 不是 CLI runtime 接线；重建索引不含 traceID且 map 遍历无序。 |
| `tracing_test.go`, `span_extra_test.go`, `slog_test.go` | tracer、span status/context、logger attr；未测一个完整 CLI trace 跨 root/assembly。 |
| `exporter_test.go`, `exporters_extra_test.go`, `exporter_config_test.go` | JSONL/stdout/redaction/config 等；未覆盖 CLI 配置在两个 builder 间的一致性。 |
| `async_test.go`, `bench_test.go` | async drop、drain、吞吐等；未覆盖 shutdown 并发入队、超时返回或 drop accounting。 |
| `exporter_otlp_test.go`, `exporter_kafka_test.go` | 本仓 mock receiver/frame 测试；不能证明与标准 OpenTelemetry Collector/Kafka broker 互通。 |
| `load_test.go` | 文件解析、树构造；未测跨 trace SpanID 冲突、父链错误与稳定顺序。 |

## 建议迭代顺序

1. **ITER-01 安全与生命周期止血（P1）**：拒绝跨 origin MCP endpoint；给 stdio transport 实现可取消 reader/process生命周期；禁止无幂等保障重放 `tools/call`；trace 默认不记录 prompt/敏感 args，把一条 Tracer/Exporter 从 CLI 根贯穿 Harness，并统一 `none`。这是先消除凭据外送、重复副作用、输入落盘和永久挂起。
2. **ITER-02 MCP runtime 完整性（P1/P2）**：给连接建立显式 owner/cleanup；错误路径释放进程；配置映射完整；hot reload parse 新配置并等后台工作；响应验证补上 status/version/id/isError；添加标准服务互通矩阵，区分 stdio、legacy SSE 与 streamable HTTP 实际支持边界。
3. **ITER-03 Extension API 收敛（P2）**：按登记顺序快照 hooks/middleware；生命周期返回聚合错误并只 shutdown 初始化成功者；同名 lifecycle 串行化；完成命令/provider或将接口定为 dormant/deprecated；明确本地插件可信边界与远端 RPC 支持状态；给 Hook action 选择“实现/删减”决策，并隔离只读 hook payload。
4. **ITER-04 Skills 约束收敛（P2）**：选择真正 YAML parser 或正式记录 frontmatter 子集；设全树资源预算、取消和 symlink policy；XML 安全编码 metadata；按产品语义决定参数是否成为 schema/强校验、是否实行 declared tool allowlist；使 SkillRegistry/Match 要么接入 agent progressive disclosure，要么标成库 API。
5. **ITER-05 Trace delivery 与互操作（P2）**：统一 exporter builder、真正执行异步导出或明确同步限制；drop/失败变可观测可恢复；区分标准 OTLP/Kafka sink 与自定义 debug framing；错误路径设置统一 Error status；基于 `(trace_id, span_id)` 稳定构树，并提供脱敏 sentinel 端到端测试。

当前建议新增的最小回归组：HTTP origin credential canary、stdio silent-reader cancellation、side-effect-then-disconnect call、assemble cleanup with child process、hot reload new command/config、Hook/Middleware insertion-order determinism、skill metadata XML injection+symlink escape+size cap、CLI prompt canary absent from every log/trace output、CLI trace single-ID tree、exporter none produces zero output。

## 审计限制

本次没有运行 `go test`、`make check`、`make verify`、race detector、`no_plugin` build，也没有接标准 MCP server、OTLP collector 或 Kafka broker。缺少这些验证时，我只对代码路径可直接推导的缺陷作“确认”标注；协议兼容、性能、race/runtime 实际复现仍需后续迭代验证。没有更改产品源代码。
