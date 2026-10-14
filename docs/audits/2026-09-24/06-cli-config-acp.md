# CLI、Config、ACP 与 Web 接线审计

范围：cmd/cli、internal/cli、internal/config、internal/acp、internal/web/server.go 和嵌入页。重点检查启动、prompt、interactive、serve、ACP stdio/HTTP、信任/凭据、配置验证、输出协议和 Runtime 接线。仅做源码检查和一次短暂的本机监听地址复现；没有运行全套测试，也没有改产品源码。

## 结论

AssembleAgent 是现有的有价值组合根：按固定顺序组装模型、工具、审批、生产中间件、输出 guard、子代理、loop、compaction、session、Harness 与 TurnRunner（[assemble.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/assemble.go:301)）。prompt、REPL、serve 都从它开始，应保留这个基础。

但共用组装不等于共用运行契约。prompt 通过 Harness 一次性运行；REPL 用 TurnRunner 驱动多轮并自行写会话；serve 只把 Dispatcher/EventBus 接到 ACP HTTP handler；独立 WebServer 不接 Agent。因此审批交互、历史提交、输入输出协议和事件归属随入口变化。Runtime 审计发现的 Loop/Agent 工具结果配对缺陷会沿共用 Agent 路径影响 interactive、prompt 和 serve。

严重度：P1 表示安全、正确性或主要用例阻断；P2 表示功能不一致、可用性或运行可靠性问题；P3 表示局部设计/文档问题。证据级别 A=已最小复现或路径直接证明；B=控制流直接推出、未动态复现。

## 发现

### CLI-01 — P1：wildcard 地址被当作 loopback，显式禁用认证时可监听全部网卡

**触发：** go-cli serve --addr :9090 --no-auth。默认 127.0.0.1 地址仍启用自动 token；此项只针对显式 no-auth 与 wildcard 组合。

**证据：** [http_server.go](/Users/pengjunchen/Documents/github/go-cli/internal/acp/http_server.go:684) 将 net.SplitHostPort 得到的空 host 视为 loopback；[serve_cmd.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/serve_cmd.go:93) 据此决定是否拒绝 no-auth；listener 仍直接使用传入地址（http_server.go:194）。

**影响：** :port 通常绑定 wildcard，非 loopback 拒绝检查被绕过，ACP routes 可由其他网卡客户端免 bearer token 访问。approve auto + no-auth 仍被独立检查拒绝，因此本报告不声称它能自动执行任意工具，但无认证 HTTP 控制面确实打开。

**证据 A：** 临时程序调用 IsLoopbackAddr(":0") 得到 true，net.Listen("tcp", ":0") 实际地址为 [::]:<port>。临时文件已清理，未开放公网服务。

**回归：** 地址表覆盖 IPv4/IPv6 wildcard、空 host、localhost、loopback 和外网地址；通过 serveCmd.Run 验证 wildcard + no-auth 返回 UsageError。

### CLI-02 — P1：仓库 ACP HTTP client 与 serve 的认证和 session identity 不兼容

**触发：** 将 NewGRPCAdapter 连接到 go-cli serve。

**证据：** Adapter POST 只设 Content-Type，无 bearer token 配置（[grpc_adapter.go](/Users/pengjunchen/Documents/github/go-cli/internal/acp/grpc_adapter.go:208)）；/stream GET 也不带 Authorization 或 sender_id query（grpc_adapter.go:243-251）。serve 默认启用 token、subject 固定为 cli（[serve_cmd.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/serve_cmd.go:119)）。/connect 和 /send 按消息 SenderID 建 session（[http_server.go](/Users/pengjunchen/Documents/github/go-cli/internal/acp/http_server.go:376)、[http_server.go](/Users/pengjunchen/Documents/github/go-cli/internal/acp/http_server.go:416)），带认证的 /stream 则强制改用 subject（[http_server.go](/Users/pengjunchen/Documents/github/go-cli/internal/acp/http_server.go:483)）。

**影响：** 默认模式下 Adapter 的 connect 收 401；no-auth 下 stream 因缺少 sender_id 收 400；只补 token 也不够，因为 Adapter 默认 sender 为 grpc、认证 stream 读 cli session。README 并列描述 ACP client/server 能力（[README.md](/Users/pengjunchen/Documents/github/go-cli/README.md:205)），当前契约无法闭合。gRPCAdapter 名称也易使人误以为是 gRPC，实际实现是 JSON-over-HTTP。

**证据 A（源码契约）：** 请求头、URL、服务端校验和 session key 逐项不匹配；未跑端到端。

**回归：** 用仓库 Adapter 连接 HTTPServer，覆盖默认 auth、no-auth、subject；完整断言 Connect → Send → Stream 取回相同响应。统一 credentials、subject/session 映射和协议命名。

### CLI-03 — P1：SSE 全局事件会混入其他并发任务；这属于路由隔离缺陷，不等于不同权限租户越权

**触发：** serve 成功组装 Agent 并设置 EventBus，多个 ACP 请求同时运行并打开 /events。

**证据：** [core_handler.go](/Users/pengjunchen/Documents/github/go-cli/internal/acp/core_handler.go:193) 的 bridge 将事件按 taskID 写入当前 session，同时发布到共享 EventBus（:199-205）；[http_server.go](/Users/pengjunchen/Documents/github/go-cli/internal/acp/http_server.go:584) 先读取该 session history，再订阅全局 bus（:607-610），实时路径不检查 task/session id（:622-630）。

**影响与范围：** 当前 serve 使用一个 bearer token 和固定认证 subject cli，没有已知的多租户/不同权限主体承诺；不把此项描述为跨权限租户越权。明确的问题是同一服务/凭据下并发任务可能收到彼此的事件正文，且固定 subject 使流不能按客户端区分。历史读取完成到订阅建立之间发布的事件也会丢失。

**证据 A（源码路径）：** 事件归属未过滤，快照先于订阅。跨不同授权主体的泄露不是本报告结论。

**回归：** 两个 task/session 同时 dispatch，各自 SSE 只收到自己的事件；在 history 与 Subscribe 间发布事件并断言不丢。采用事件 correlation id 与原子订阅游标。

### CLI-04 — P1：服务端默认 ask 审批仍读本机 stdin，没有远程决策通路

**触发：** serve 默认 approve ask 执行需要审批的工具；headless prompt 或非 TTY REPL 也会走相同分支。

**证据：** serve 用 approve mode 调 AssembleAgent（[serve_cmd.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/serve_cmd.go:168)）；[assemble.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/assemble.go:1014) 对无 TUI approval channel 的路径创建 InteractiveApprovalCallback(os.Stdin, os.Stderr)。approval channel 只在 interactive TTY 设置（[repl_session.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/repl_session.go:254)）。ACP routes 没有审批 request/reply API。

**影响：** HTTP 请求触发待批工具后，后台 dispatch 会等本机 stdin；远端无法批准/拒绝，可能挂起或与操作者输入竞争。服务端默认 ask 没有可用的 server-side 审批 UI。

**证据 A（接线）：** 没有其他 callback 或审批路由。

**回归：** HTTP 触发审批工具时服务不应读 stdin；Runtime 提供可取消 ApprovalBroker 和带 request id 的 approve/reject API；超时/断连产生明确 denied/canceled result。

### CLI-05 — P1：Loop/Agent 取消或错误后的 tool-call history 不成对，影响共用 Agent 的入口

**触发：** assistant 返回 tool call，随后工具因取消或失败未完成；下一轮复用 Agent history。

**证据：** Runtime 审计核实 Loop 只在局部 messages/events 中合成错误分支 tool result（internal/core/loop.go:491-507），AgentImpl.Run 只持久最后 assistant event、不持久 tool-result events（internal/core/agent.go:142-153）。CLI 入口共用 AssembleAgent：prompt 经 Harness.Submit（[prompt.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/prompt.go:151)）；REPL 用 TurnRunner 和同一 Agent；serve 用 assembly Dispatcher/EventBus，不能修复 Agent history。

**影响：** 后续模型请求可能重放没有 result 的 ToolCall history，造成协议错误、工具重复或上下文损坏；compaction 不是一致性修复层。

**证据 A（Runtime 源码）：** Runtime 审计提供实现路径；本子审计不重复临时复现。

**回归：** prompt 连续提交、REPL 按 ESC 取消后继续、ACP serve 取消子任务，检查 Agent history 与 transcript 中每个 assistant tool call 都有对应 result。

### CLI-06 — P2：onboarding 保存的 keychain/auth.json 未进入模型构造，持久 key 可能重复询问

**触发：** 新用户完成 onboarding 后重启，或配置文件存在但没有明文 API key。

**证据：** onboarding 将 key 写进 OS keychain 或 ~/.config/go-cli/auth.json（[onboarding.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/onboarding.go:359)、:453-485）；config 有 keychain/auth.json/env/models.json 的 AuthChain（[auth_chain.go](/Users/pengjunchen/Documents/github/go-cli/internal/config/auth_chain.go:45)），但 production 调用点只有定义，没有 NewAuthChain/Resolve 的生产调用。buildModel 只传 rc.Provider.APIKey 或调用默认 provider registry（[assemble.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/assemble.go:1470)）。已有 config 且 key 缺失时，onboarding 注释和代码说明只更新本进程 cfg、不写文件（onboarding.go:149-152）。

**影响：** onboarding 持久化凭据不会经 AuthChain 回读；key 缺失时可能每次都再询问，禁用 onboarding 后也可能找不到所存凭据。具体 Provider 或环境变量可能有旁路，结论限定在持久 auth sources 未接入 buildModel。

**证据 A（静态调用图）：** production 源码无 AuthChain 消费者。

**回归：** 隔离 HOME，保存后重启，断言不再询问且 buildModel 得到解析 key；覆盖 keychain/auth.json/env/models.json 优先级。

### CLI-07 — P2：正式 CLI 没有可持久的 trust 启用路径，项目配置长期 fail-closed

**触发：** 工作目录有项目 config、skills 或 custom tools。

**证据：** [cmd/cli/main.go](/Users/pengjunchen/Documents/github/go-cli/cmd/cli/main.go:22) 每次运行创建 NewDefaultTrustManager(nil)；nil store 被换为进程内存 store（[trust.go](/Users/pengjunchen/Documents/github/go-cli/internal/approval/trust.go:43)）。CLI 没有持久 TrustStore、trust/revoke 命令或 TrustProject 调用。LoadTrusted 不可信时跳过配置（[config.go](/Users/pengjunchen/Documents/github/go-cli/internal/config/config.go:154)）；project skills/custom tools 另经 TrustManager 检查（[assemble.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/assemble.go:1867)、:1937）。main 局部 manager 与 assembly 查询的全局 registry 也不是同一实例。

**影响：** deny-by-default 合理，但没有产品路径信任项目，重启也不可能保存信任，项目配置/扩展功能实际长期不可用。

**证据 A（调用图）：** 初始化和生产调用点已核实。

**回归：** 注入同一 TrustManager，添加可审计 trust/revoke 流程与持久 store；修改 fingerprint 后要求重信任；覆盖首次、重启、撤销。

### CLI-08 — P2：tracing exporter 文档、validator 和 CLI 不一致；stdout trace 可污染机器输出

**触发：** 配置 exporter otlp/kafka，或 prompt --output json 配 exporter stdout。

**证据：** README 声称 stdout/jsonl/otlp/kafka（[README.md](/Users/pengjunchen/Documents/github/go-cli/README.md:269)）；validator 只允许 jsonl/stdout/none（[validator.go](/Users/pengjunchen/Documents/github/go-cli/internal/config/validator.go:41)），测试明确断言 otlp 无效（[validator_test.go](/Users/pengjunchen/Documents/github/go-cli/internal/config/validator_test.go:45)）。Assembly helper 支持 otlp、不支持 kafka（[assemble.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/assemble.go:2188)）；进程级 newTracing 也没有 otlp/kafka 分支（[main.go](/Users/pengjunchen/Documents/github/go-cli/cmd/cli/main.go:118)）。stdout traces 与 prompt NDJSON 写同一 stdout（prompt.go:185-205）。prompt.go:97 又将全部参数作为普通 span attribute；redaction 只遮 SensitiveAttribute（[tracing.go](/Users/pengjunchen/Documents/github/go-cli/internal/tracing/tracing.go:42)），而 REPL 对用户消息显式标敏（[repl_session.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/repl_session.go:649)）。

**影响：** README 中 otlp/kafka CLI 入口不可用；stdout traces 破坏 NDJSON/raw stream；prompt 原文可能写进 trace。

**证据 A（配置/调用链）：** validator、assembly、main 直接证明；未运行输出集成测试。

**回归：** exporter 支持集合、validator 和 docs 共用单一来源；机器模式 traces 走 stderr/file/collector；断言 stdout 仅含指定协议；prompt 原文标敏并测 redaction。

### CLI-09 — P2：help/version 前无条件加载和验证配置，坏 config 时无法求助

**触发：** 工作目录有无效 config 后运行 go-cli --help 或 --version。

**证据：** [main.go](/Users/pengjunchen/Documents/github/go-cli/cmd/cli/main.go:55) 在 runCLI 前无条件加载配置；CLI 之后才解析 help/version（[cli.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/cli.go:94)）。config 错误直接返回 1。

**影响：** 不依赖模型配置的本地命令也被挡住，用户无法用 help 得到恢复指引。Load 注释所称裸 help/version 不需要 config 只对没有无效 config 成立。

**证据 B（控制流）：** 加载在命令分发之前。

**回归：** help/version 在 config load 前短路；坏 JSON/YAML fixture 下断言它们成功，模型命令仍报告配置错误。

### CLI-10 — P2：prompt 只使用第一个 positional arg，运行失败仍把 spans 记成成功

**触发：** go-cli prompt inspect this repo 未加引号；或 Harness/result/output 失败。

**证据：** [prompt.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/prompt.go:80) 只取 fs.Args()[0]。prompt.run defer 无条件设 OK（:94-103）；stream.Result 后也先标 dispatch span OK 再检查错误（:165-172）。

**影响：** 未加引号的自然 shell 请求被静默截断；监控把失败记成功。

**证据 A（源码）：** 参数和状态路径直接证明。

**回归：** 明确 join 参数或拒绝多参数并提示 usage；模拟 submit/consume/result 错误，断言 span error。

### CLI-11 — P2：REPL 传 persistence=true，但缺 store_path 时静默没有持久历史

**触发：** 未配置 Session.StorePath 启动 REPL 或传 --resume。

**证据：** REPL 传 WithSessionPersistence(true)（[repl_session.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/repl_session.go:258)）；session 仅在 enableSession 且 store path 非空时打开（[assemble.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/assemble.go:1454)）。打开失败仅 warning 后清空 store（:1457-1461）；无 store 时 resume 静默继续（assemble.go:342-349）。

**影响：** --resume 可静默无效，默认交互不跨进程保留历史。tree/resume 的其他风险见 session 专项审计。

**证据 A（接线）：** 配置条件直接证明。

**回归：** 创建默认私有 store path；无 store resume 显式提示/失败；open 失败在 UI 中展示降级原因。

### CLI-12 — P2：ACP 并发槽不限制后台 dispatch，队列和 HTTP timeouts 无界

**触发：** 连续 POST /send，或授权客户端保持慢连接/重复请求。

**证据：** handleSend 启动 go handler.ProcessMessage 后立即返回（[http_server.go](/Users/pengjunchen/Documents/github/go-cli/internal/acp/http_server.go:423)）；64 槽只限制 HTTP handler，handler 返回后槽释放（:67-70、:214）。session pending slice 无上限（[core_handler.go](/Users/pengjunchen/Documents/github/go-cli/internal/acp/core_handler.go:57)）；http.Server 无读写/header/idle timeout（[http_server.go](/Users/pengjunchen/Documents/github/go-cli/internal/acp/http_server.go:216)）。TTL 只清理空闲 session，不限制创建和 dispatch 速率（:645-673）。

**影响：** request semaphore 限制短 handler，不限模型/工具工作；可累积 goroutine、LLM 调用、事件与消息。有效 token 缩小暴露面，但没有资源上界。

**证据 B（控制流）：** goroutine 与 slot 释放分离是源码事实，资源规模取决于部署。

**回归：** 限制 dispatch/身份级并发、请求速率和 pending 队列；满额明确拒绝；设置 HTTP timeouts/MaxHeaderBytes；压力测试验证资源上界。

### CLI-13 — P2：覆写已有宽权限 token file 不会收紧权限

**触发：** 指定已存在 0644 文件作为 --token-file。

**证据：** [serve_cmd.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/serve_cmd.go:279) 创建目录 0700，随后 os.WriteFile(...,0600)；已有文件 mode 不会被改。测试只检查新文件（[serve_cmd_test.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/serve_cmd_test.go:176)）。

**影响：** 注释承诺的 0600 不一定成立，其他本机用户可能读取 bearer token。

**证据 A（文件模式语义及测试条件）：** 代码不 chmod。

**回归：** 写私有临时文件再原子 rename，或写后 Chmod；先建 0644 再写，断言最终 0600。

### CLI-14 — P2：ACP stdio 与 REPL/prompt 共用 stdin/stdout，协议和用户交互互相污染

**触发：** 任一入口配置 acp.transport: stdio 和 endpoints。

**证据：** 所有模式在 AssembleAgent 组装 ACP（[assemble.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/assemble.go:1197)），适配器绑定 os.Stdin/os.Stdout（:1200-1203）；Connect 会写 JSON 帧（[stdio_adapter.go](/Users/pengjunchen/Documents/github/go-cli/internal/acp/stdio_adapter.go:70)）；REPL、headless approval 和 prompt 也使用相同 stdin/stdout。

**影响：** TTY 输入、approval 和 ACP reader 抢同一输入；protocol 帧混入人类输出或机器 NDJSON。

**证据 A（绑定路径）：** 由代码直接共享 stream。

**回归：** ACP 使用独立专用输入输出，或注入独立 Reader/Writer；断言协议和用户流在不同 fd。

### CLI-15 — P2：StdioAdapter Disconnect 可能留下阻塞 reader；并发 Send 没有 writer lock

**触发：** reader 阻塞在无换行 ReadBytes 时调用 Disconnect；或并行 SendMessage。

**证据：** [stdio_adapter.go](/Users/pengjunchen/Documents/github/go-cli/internal/acp/stdio_adapter.go:216) reader goroutine 阻塞调用 ReadBytes，读取返回后才 select done（:218-223）；Disconnect 关闭 done 但不关闭 reader（:93-128）。Connect 写帧有锁，SendMessage 无写锁（:133-159、:181-197），且没有最大行长度。

**影响：** 不可关闭 reader（例如 stdin）可令 goroutine 泄漏；并发写可能竞争，超长无换行输入可扩大缓冲。

**证据 A（生命周期/锁路径）：** 直接源码路径；未跑 race/leak tests。

**回归：** 可控阻塞 reader Disconnect 后测 goroutine 收敛；并发发送验证每行完整 JSON；使用 bounded reader。

### CLI-16 — P2：Web UI 只是 echo/SSE 原型，未接 Agent；启用 auth 后页面请求 401

**触发：** 将 WebServer 当正式 Agent dashboard 使用，或开 WithAuthToken 后操作页面。

**证据：** [server.go](/Users/pengjunchen/Documents/github/go-cli/internal/web/server.go:28) 明确称 placeholder；/api/chat 返回 Echo（:213-236），/ws 是欢迎事件/keepalive（:239-260）。production 路径没有 NewWebServer 调用。auth middleware 对 API 要求 Authorization（:88-96），但 [index.html](/Users/pengjunchen/Documents/github/go-cli/internal/web/index.html:203) fetch 和 EventSource（:229）都不带 token。

**影响：** 页面无法执行 Agent 请求；开 auth 后首页可以加载，但发送/SSE 均失败。并发 Start 可能开两个 listener 但对象只保存后一个（server.go:114-135）；也无 HTTP timeouts。

**证据 A（实现与引用扫描）：** 原型行为直接可见，production 无入口。

**回归：** Web 若为正式目标，应接统一 Runtime/capability/auth/session contract 并实现安全浏览器认证；完成前标为 prototype。加入并发 Start lifecycle test。

### CLI-17 — P3：prompt flag parse 错误与 stdout trace 会破坏机器模式输出

**触发：** prompt --output json --unknown，或机器输出同时启用 stdout tracing。

**证据：** prompt FlagSet 将诊断 output 设为命令数据 writer（[prompt.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/prompt.go:57)）；FlagSet 错误/usage 写 stdout。进程和 assembly trace exporter 也写 stdout（[main.go](/Users/pengjunchen/Documents/github/go-cli/cmd/cli/main.go:127)、[assemble.go](/Users/pengjunchen/Documents/github/go-cli/internal/cli/assemble.go:2192)）。

**影响：** stdout 混入 usage 或 trace records，NDJSON/raw stream 消费方无法解析。

**证据 B（writer 接线）：** 路由明确，未跑 CLI集成测试。

**回归：** flag diagnostics 固定 stderr；机器模式禁用 stdout traces 或重定向；错误场景断言 stdout 为空、stderr 有诊断。

## 合理设计与不完善处

合理设计：AssembleAgent 是 Runtime 组合根候选，共享 model retry/circuit breaker、工具、中间件、输出 guard、subagent 和 compaction；配置 default/file/env/flag/override 优先级清楚；项目 config 默认 deny；serve 默认 loopback + bearer token，有 body limit、常量时间 token 比较、HTTP slot、session TTL、SSE ring history；Web 页面以 textContent 渲染聊天，避免把用户消息拼接为 HTML。

不完善：入口独立选择 Harness、TurnRunner、Dispatcher，审批/持久化/事件语义没有统一契约；认证主体、Adapter sender、session key 和 SSE event key 各自为政。配置 schema 广于真实注入面，AuthChain、Settings、FeatureFlag 等没有进入 CLI 主路径。trust fail-closed 缺少 trust 生命周期。WebServer 仍未接入正式路径。

## 实际入口装配

| 入口 | 实际 Agent 路径 | 审批 | 会话/事件 |
|---|---|---|---|
| 默认/interactive | REPLSession → AssembleAgent → TurnRunner → Agent/Loop | TTY approval channel + Tea；非 TTY 读本机 stdin | 只有配置 store_path 才持久化；REPL 自行写 user/assistant entries；ACP stdio 可抢 stdin |
| prompt | AssembleAgent → Harness.Submit → Agent/Loop | auto/deny/ask；ask 依赖 stdin | 默认不持久；仅 --fork 读取 session；NDJSON/raw stream 可被 stdout 混写破坏 |
| serve | AssembleAgent 后只把 Dispatcher/EventBus 接进 CoreHandler | ask 等服务进程 stdin，无远端决策 | ACP session 内存态；无对话历史；全局事件未过滤 |
| stdio ACP config | 宿主入口仍运行相同 Agent Loop | 跟宿主入口相同 | 直接绑定进程 stdin/stdout |
| WebServer | 无 Agent/Loop 接线 | 无 | 独立 echo + placeholder SSE，无 conversation store |

建议单一 Runtime 显式持有模型、Agent、ToolRegistry/Dispatcher、SessionStore、Compactor、ApprovalBroker、CapabilityPolicy、event stream、I/O channels 和 cleanup。入口只适配统一 RunRequest 并选择 capabilities，不再自行拼审批、持久化和历史语义。事件 envelope 应包含 run/session/task/correlation id 与 sequence；tool call、tool result、final answer 和中止状态由单一协调器提交，避免历史双写和取消后不成对。

## 迭代顺序建议

1. **I0：解除发布阻断。** 修 wildcard/no-auth；闭合 ACP client/server token 和 subject/session 路由；按 task 隔离 SSE 并消除 history/subscribe 漏事件；限制后台 dispatch/队列/HTTP timeout；为 serve ask 增加可达审批回复或默认明确拒绝。
2. **I1：统一 Runtime contract。** AssembleAgent 演进为 Runtime 构造器与 RunRequest；显式 workspace、network、shell、persistent session、remote call、tool approval capabilities；interactive/prompt/serve 变为入口 adapter；effective policy 统一构造、审计，不被入口覆盖。
3. **I2：会话与 turn 原子性。** 同一提交包含 assistant tool calls、tool result、final answer、interrupted/error state；确定唯一 transcript authority；默认私有 session path、resume/fork 语义和错误反馈；结合 session 专项报告。
4. **I3：闭合 credentials/trust/config。** AuthChain 接入 model construction；注入共享持久 TrustManager，提供可审计 trust/revoke 和 fingerprint 重核；配置、validator、runtime、docs 共用能力矩阵；坏 config 不阻挡 help/version。
5. **I4：完成或收缩网络产品面。** ACP 明确 JSON/HTTP 或真正 gRPC；隔离 identities/sessions/events，提供远程审批、资源限额和 TLS 策略。Web 只有接同一 Runtime contract 后才作为可用产品宣传。
6. **逐迭代验证：** 所有入口跑相同权限、取消、tool call/result、持久化 contract cases；ACP 验证端到端和事件隔离；CLI 检查 NDJSON/raw stdout。测试闭合、UAT/E2E、生产发布分别记录。

## 已审源码 manifest 与覆盖边界

以下 production 源码均列入范围；核心入口与报告涉及路径作控制流追踪，其余实现文件作结构/调用路径阅读。未逐句审查每个平台 helper 的所有边界。

- cmd/cli：main.go。
- internal/cli：assemble.go、cli.go、command.go、command_loader.go、compaction_adapter.go、completer.go、disk_unix.go、disk_windows.go、doctor.go、extension_bridge.go、history.go、interactive.go、interrupt.go、line_editor.go、lsp_completer.go、mention.go、mention_resolver.go、models_cmd.go、onboarding.go、output_guard_adapter.go、prompt.go、registry.go、registry_adapters.go、repl_session.go、resize_other.go、resize_unix.go、serve_cmd.go、slash.go、slash_context.go、slash_handlers.go、slash_mcp_reload.go、slash_memory.go、slash_registry.go、slash_revert.go、slash_worktree.go、types.go、user_errors.go。
- internal/config：auth_chain.go、config.go、context_files.go、feature_flag.go、keychain.go、lazy_path.go、loader.go、model_alias.go、prompt_template.go、settings.go、types.go、validator.go、yaml_loader.go。
- internal/acp：bridge.go、client.go、core_handler.go、grpc_adapter.go、http_server.go、middleware.go、sse_writer.go、stdio_adapter.go。
- internal/web：server.go、index.html。
- 测试源码 inventory 已定位：cmd/cli/main_test.go；internal/cli/*_test.go；internal/config/*_test.go；internal/acp/*_test.go；internal/web/server_test.go。全仓文件台账在 docs/audits/2026-09-24/source-inventory.csv。

**覆盖缺口：** 未运行全量 tests/verify、真实 LLM/MCP/ACP/browser E2E 或 race/leak 检查。Web 视觉/交互品质由 UI 专项审计负责；工具沙箱路径、approval classifier/cache 由 tools/security 专项报告负责。本报告仅描述 CLI 入口的实际接线。

