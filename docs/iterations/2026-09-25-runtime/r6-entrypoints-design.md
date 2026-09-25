# R6 entrypoint, identity, trust, and wire-version contract

状态：设计稿，供 R6-DES-01 独立评审。它不批准 R6 builder 开工；`R1-REV-01`、`R2-REV-01`、`R4-REV-01` 尚未完成时，R6 实现仍须等待这些依赖。本文只定义本仓库可声明的入口与协议边界，不把已有构造器、测试 fake 或包级 API 当作已交付的产品能力。

审查基线：`codex/runtime-stabilization`，源码快照 `78a3683`；相关事实来自 [CLI/config/ACP 审计](../../audits/2026-09-24/06-cli-config-acp.md)、[Extension/MCP/Tracing 审计](../../audits/2026-09-24/07-extensions-tracing.md) 和 R6 任务定义。实现时如源代码已变更，应重新核对实际调用路径。

## 决策

1. 每种入口都必须声明它实际启动的执行器、会话身份、审批路径和 stdout/stderr 语义。共享 `AssembleAgent` 不代表 prompt、REPL、serve 和库级 adapter 已有相同的运行契约。
2. MCP 本轮冻结 `2025-06-18` 为唯一标准互通验收目标。协商到不支持的版本必须清晰拒绝；不得把 `2025-11-25` 或上游 `2026-07-28` 的线协议能力默认为已支持，也不以“支持最新版本”为本轮目标。若保留更早版本兼容，必须有独立、可执行的版本化 wire fixtures；没有 fixture 就不能宣称该版本符合标准。
3. `OfficialSDKAdapter`、`Mark3labsAdapter` 和 `NewGRPCAdapter` 这些名字不能替代传输事实。当前两个 MCP adapter 共用本仓手写 JSON-RPC transport；`NewGRPCAdapter` 实际发送 JSON/HTTP。保留已有 Go API 或配置名时，文档、帮助文本和测试必须说明实际 transport；如增添准确名称，旧名称可以作为注明的 deprecated alias，不能静默改变既有配置含义。
4. 远端身份、session、approval 和工具 dispatch 必须沿同一受控身份快照。registry、permission 或 Plan 版本在审批期间变化时，R4 的 dispatch commit 校验负责使旧审批失效；不得以更宽松的重试或重用逻辑绕过该边界。

## 入口能力矩阵

| 入口 | 当前源码能力 | R6 可声明的契约 |
|---|---|---|
| `prompt` | 单次 Harness 提交；支持文本、JSON、stream 输出。普通 CLI 构造 Runtime；机器输出与 trace/log 分流问题在审计中有记录。 | 本地一次性 Agent 请求。完整组装失败时返回失败；stdout 仅含所选输出协议，诊断写 stderr 或日志。多个 positional 参数按一个 prompt 处理或明确报 usage 错误，不得静默丢弃其余参数。`approve ask` 仅在可用的本地交互终端使用；无交互决策面时拒绝等待 stdin。会话持久化只有在 store 可用且成功写入时才可报告成功。 |
| `interactive` / REPL | `TurnRunner` 多轮循环；按配置组装 session store；TTY 模式接入 TUI 审批。 | 本地用户身份绑定一个会话。session store 未配置或打开失败时，在启动或首次保存时明确告知持久化不可用，不能把内存历史称为已保存。TUI 审批只对当前未过期请求生效，过期或取消的批准不得被缓存成 `AlwaysAllow`。 |
| TUI 审批面 | 是 REPL/交互入口中的 UI callback，不是独立 Agent runtime。 | UI 展示 request ID、工具名、参数摘要、权限来源和失效状态；批准/拒绝仅作用于当前 request token。远程 `serve` 不得借用 TUI 的 stdin/stdout callback。 |
| `serve` | `POST /connect`, `/send`, `/disconnect`；`GET /stream`, `/events`。Bearer token 默认开启；当前一个 token 被映射为固定 `cli` subject。若 Agent 组装失败，源码会退回 echo/ack handler。 | 定义为本仓库的受认证 JSON/HTTP agent-messaging 服务。Auth token 识别 principal，服务端签发的 session ID 是路由权威；caller 的 `sender_id` 只能是该 principal 下的别名，不能选择其他主体或会话。Agent assembly 失败必须启动失败，或启动时清楚标成不提供 Agent dispatch 的显式诊断模式；不能以 echo/ack 伪装成 Agent 成功。无远程 approval broker 时，`approve ask` 必须拒绝启动或对该请求明确 fail closed，不能读取服务进程 stdin。 |
| ACP HTTP client | `NewGRPCAdapter` 使用 JSON/HTTP routes，没有 token 配置；目前与 `serve` 默认认证及 session subject 映射不能闭环。 | 作为本仓自定义 HTTP client 声明，完成 token 配置和 connect→send→stream→disconnect 回环后才声称与本仓 `serve` 兼容。不得声称 gRPC 互通。 |
| ACP stdio client | `StdioAdapter` 以 JSON 行收发 `ACPMessage`，是包级 API；装配时可能直接拿进程 stdin/stdout。 | 独立且专用的 reader/writer 才可启用。与 REPL、prompt、approval 或机器 stdout 共用 fd 时禁用。帧大小有上限；每个写帧原子串行；取消必须关闭/取消底层 reader 或由 owner 关闭 pipe，并等待 reader、writer、dispatch worker 收敛。 |
| Web UI | `internal/web` 提供 echo/SSE placeholder；生产入口未接 Agent，页面也没有传递配置的 auth token。 | 本轮标记为 prototype / unavailable。不得在 UI、README 或 CLI 输出中称其为可用 Agent UI；可显示明确不可用状态。只有完成 Agent、身份、事件隔离、approval、认证和实际端到端验证后才可改变声明。 |
| MCP stdio / HTTP | 两种 adapter 均为本仓自写实现；HTTP 路径偏向旧 SSE endpoint-event 子集，尚无外部 peer 的标准互通证据；CLI 配置缺 HTTP credential 字段，assembly 也未映射。 | 仅声称本轮声明并通过 fixtures 的 MCP transport / 版本。stdio 与 2025-06-18 Streamable HTTP 分开验收。不得把 SSE endpoint-event 子集说成 Streamable HTTP，也不得把 adapter 类型名说成 SDK 互通。 |

## 共用身份与项目信任

Runtime 启动时形成一个不可变身份快照，至少包含：规范化的 project root、当前本地用户或已认证 remote principal、runtime session/task ID、项目配置 fingerprint、registry generation、permission generation 和 Plan generation。该快照供配置加载、审批、工具执行、事件路由和审计日志使用；各入口不得各自再推导一个不一致的“当前用户”。

R4 规定的工具 dispatch commit 是副作用边界：工具定义必须由受控 registry 捕获；等待审批后，在短临界区重检捕获的 definition identity、registry/permission/Plan generation，并标记 dispatch committed；不得持锁等待用户或执行工具。commit 以后发生的撤销不能被描述为已阻止已经发出的副作用，取消仅作为 in-flight cleanup 报告。

项目信任须由同一个明确注入的 `TrustManager` 实例供 `LoadTrusted`、`AssembleAgent` 和权限决策使用。生产入口不得继续把互不相干的内存 manager 当成同一个信任域。CLI 提供可审计的 trust/revoke 路径；持久文件放入用户配置目录、权限限为当前用户，并用原子更新。存储错误、缺失记录或 fingerprint 不匹配均 fail closed。

Fingerprint 必须由源码实际加载并影响运行时行为的项目输入构成，不得只哈希单个 MCP 文件后就代表整个项目配置。DES/BLD 交付时需列出确切 manifest：配置文件、被加载的 hooks/custom tools/MCP server definitions、skills 或其他会进入 Runtime 的项目文件；以规范相对路径和内容摘要构成稳定哈希，并明确 symlink、删除和新增文件的处理。受信任项目任何输入改变后，原记录失效，要求用户再次信任；不能自动把新配置纳入旧授权。撤销要持久化并增加 trust generation，从而使未 dispatch 的旧审批失效；已经 commit 的执行依 R4 in-flight 规则收敛。

Provider credentials 从 onboarding 所写的 keychain/auth 文件、环境变量或配置读取后，必须进入 Runtime 实际调用的同一个 provider factory。诊断和 trace 只能保留 provider、错误类别、HTTP 状态等安全 metadata；不得记录凭据或自由响应正文。CLI `--help`、`--version` 在完整配置校验之前工作；token 文件新建和覆写后都必须为私有权限；stdout 的 JSON/NDJSON 不得夹入日志、trace 或 onboarding 文案。

## ACP 路由、事件和审批契约

当前 `internal/acp.HTTPServer` 是仓库自定义 JSON/HTTP 协议，源码没有版本握手或官方 ACP conformance 证据。R6 保留现有 routes/API 名称以兼容已配置客户端，但所有对外说明须称其为 go-cli ACP JSON/HTTP transport；不称为 gRPC，也不据包名声称与独立 ACP 产品互通。

1. **认证与 session。** Bearer token 验证后得到 principal。`/connect` 的响应给出 server-generated session ID；后续 `/send`、`/disconnect`、`/stream`、`/events` 必须显式关联该 ID。服务端在每个请求核验 `principal + session ID`。旧 `sender_id` 可作为同一 principal 下的兼容客户端标签，不是授权主体，也不能覆盖已认证身份。当前单 token 部署可映射一个 operator principal，但事件仍须按其独立 session/task 路由。
2. **事件与游标。** 每个 dispatch 分配 task ID，并将 task ID、session ID、principal 一起附着到事件。SSE 只发送该 session/task 授权可见的事件。历史回放和实时订阅之间需使用原子游标或等效无丢失协议；`Last-Event-ID` 仅在服务端具备对应的稳定、session-scoped 游标时接受。事件 ID 至少在一个 session 内稳定唯一，不能靠全局 EventBus 广播后不筛选。
3. **远程审批。** `approve ask` 使用 server-owned `ApprovalBroker`，不读本机 stdin。请求由不可猜测的 approval request ID 标识，携带 session/task、工具调用 ID、创建时的 permission/registry/Plan generation 和 deadline；pending request 可经受认证的 approval query/event 得到，决策 endpoint 接受 `approve` 或 `deny`。决定只对创建该请求的同一 principal/session 和未过期 generation 有效。取消、超时、session 断开、policy generation 改变、重复回答均产生明确的 canceled/expired/stale 结果；不能缓存为持久 allow。broker 未配置时 `ask` 不能无限挂起。
4. **界限资源。** 并发限制必须包住真正的 dispatch 生命周期，而不是只包住迅速返回的 HTTP handler。每 principal/session 的 dispatch 数、pending approval 数和等待队列均有上界；满额返回可诊断的拒绝。HTTP header/body、read/write/idle timeout、SSE watcher 和 worker 生命周期有显式限制；停止服务时取消并 join 自己拥有的 dispatch、订阅和清理 worker。
5. **失败路径。** `/send` 收到已授权消息后，如果 Agent 组装失败或 dispatch 未执行，返回可区分的错误；不得以 echo 回复冒充模型输出。远程调用方丢失响应不能自动再次派发不具幂等语义的消息，结果状态遵循 R1 的 `ReceiptOutcome` / `unknown` 与 `ExecutionDone` 正交契约。

ACP stdio 目前是本仓自定义 `ACPMessage` JSONL，不作为 MCP JSON-RPC，也不宣称 ACP 标准互通。每行是独立 UTF-8 JSON object 且不得含内嵌换行；stdin/stdout 仅承载协议，诊断只写 stderr。合同的帧上限应使用现有 HTTP body 上限 `1 MiB`，或给出更严格的显式值；超限必须拒绝整帧而不截断执行。共享 writer 由一把锁完整保护 marshal+write；读帧器须有 owner 可关闭的底层流、有限 context deadline 和 join 确认。进程信号结束时先关输入/取消请求，给子进程有限宽限，之后 TERM/KILL 并 wait，回收所有 pipe 与 worker。

## MCP `2025-06-18` wire 合同

### 版本与声明

- 唯一标准互通验收目标为 `2025-06-18`。客户端 initialize 请求声明该版本，只有协商结果也受支持时才发送 `notifications/initialized`、`tools/list` 或 `tools/call`；不支持的协商值返回带版本信息的错误并断开，不能猜测或按 `LatestProtocolVersion` 自动路由。
- `2025-11-25` 与 `2026-07-28` 在本次迭代明确 unsupported。上游当前标注为 `Current` 的 revision `2026-07-28` 已改变 Streamable HTTP（移除 GET stream 和 protocol session，并要求新的 per-request metadata）；实现这些语义属于后续单独版本工作，不是本次补齐测试时顺带扩展的功能。
- 源码当前还列出 `2024-11-05`、`2025-03-26`。本设计不把它们算作 `2025-06-18` 的相同 wire 流程；若要保留兼容，必须有独立版本分支和覆盖其 initialize、transport、version/session 行为的 fixtures。没有该证据不得宣称兼容；对配置继续可读，遇到不支持的协商要给出可操作错误，不得错误路由到其他 adapter。

### stdio

- MCP stdio 是 JSON-RPC UTF-8 单行帧：每个 request/notification/response 一行，行间 LF，JSON 内容不能有未转义内嵌换行；stdout 上只能出现 MCP 帧，stderr 才可用于子进程日志。
- 每帧字节数有显式上限；读写均可取消。单个 owner 管理 `exec.Cmd`、stdin/stdout/stderr pipes、scanner/reader、request loop 和 wait。ctx 取消必须能关闭/杀死实际阻塞的 pipe/进程，而不只在同步 `ReadString` 前检查 context。Disconnect 关闭 stdin、有限等待、TERM/KILL，并 wait；所有重连/清理 worker 都可 join。
- request ID、JSON-RPC error、result shape、response size 均验证；同一 transport 同一时刻只允许一个 request owner 读 response，或显式采用可关联多路复用器；不可让并发调用拿到别人的 response。

### Streamable HTTP

- 每个 server 使用配置的单一绝对 MCP endpoint。先向该 endpoint POST `initialize`，`Accept` 同时包含 `application/json` 和 `text/event-stream`；按 HTTP status/content type 读取一个 JSON result 或该请求的 SSE response stream，验证 JSON-RPC ID、result/error、协商 protocol version，再发 `notifications/initialized`。
- 后续 HTTP 请求带 `MCP-Protocol-Version: 2025-06-18`。若 initialize response 带 `Mcp-Session-Id`，存储后续请求并在每次 subsequent request 带回。若带 session 的请求返回 404，允许重新 initialize 一条新 session，但不能将“工具调用响应丢失”当成安全重发依据。正常退出尽力发送 HTTP DELETE + session ID；405 表示 peer 不支持 DELETE，可以正常关闭本地连接。
- HTTP status 非 2xx、JSON-RPC error、`202 Accepted` 通知响应、JSON body 与 SSE body 分别按规范解析。SSE 消息必须与原 request ID/流关联，先到的 progress/notification 不能当最终成功。GET event stream 是可选能力：收到 405 表示 peer 不提供独立监听流，不表示 connect 成功或失败；本轮实现不使用旧式 `endpoint` event 探测作为 Streamable HTTP 握手。
- configured URL 的 scheme、host 和 effective port 定义 credential origin。redirect、endpoint URL 或响应中的任何后续 URL 若跨 origin、降级 HTTPS 或改到不允许的 scheme，必须在发出带 Authorization/custom headers 的请求前拒绝。默认禁止向跨源地址转发 bearer/OAuth/custom headers；不能依赖 DNS/IP 层 SSRF 防护代替 origin check。
- GET/初始化/能力读取等可重复的操作按明确有限策略处理；`tools/call` 一旦请求可能已发出、响应却未知，必须以 typed ambiguous/unknown 结果返回，绝不自动 replay。无副作用的 provider/协议错误与执行结果 unknown 区分；这些边界依赖 R1/R2 代码 seam，R6 fixture 必须验证首次执行后丢响应时远端 side-effect counter 恰为 1。
- HTTP request/response/SSE 总量、等待时间和连接 worker 有确定上限。Disconnect 或 context cancel 关闭当前 body/stream、取消请求并等待 owner 退出。每个 client 的 teardown 应对正常退出、connect 成功后 list 失败、hot reload 替换/删除和 shutdown 等路径恰好收敛一次。

## R6 可执行验收

在允许 R6 builder 开工后，至少用以下真实入口/fixtures 验证；静态接口测试或仅在包内 mock 调用不能代替：

1. 用隔离 HOME、config、工作目录执行真实二进制：onboarding 写入的 key 在重启后进入同一 provider factory；help/version 在故意损坏的配置旁仍工作；token file 新建和覆写均为私有 mode；`prompt` stdout 保持严格 JSON/NDJSON 可解析且 stdout 中没有日志/trace/审批文案。
2. 同一持久 TrustManager 从 CLI config loading 一路注入 Assembly；trust→重启→可用、改一项被加载的项目输入→失去信任、显式 revoke→重启后仍失去信任。验证目录新增/删除与 symlink 改动会改变 fingerprint；信任存储读取/写入错误均拒绝受信项目配置执行。
3. 真实 `serve` 测 connect→send→stream/events→disconnect；不同 session/task 并发时不串事件；带错误/伪造 sender ID 的调用不能跨 principal/session 读取或派发。让 dispatch 满队列并证明拒绝、响应断开时 outcome 可表示 unknown；`approve ask` 通过认证的远程 approval round trip，不读取服务 stdin，覆盖 stale/expired/canceled/duplicate decision。
4. ACP stdio 用真实 pipe/subprocess fixtures 覆盖 fragmented line、并发 writer、超长行、静默 reader cancel、EOF、期限到期及最终 worker/process 回收；检查协议 stdout 没有诊断输出。
5. MCP 2025-06-18 使用独立可运行 peer fixture，覆盖 initialize/initialized、协商未知版本拒绝、POST JSON result、POST SSE notification+result、HTTP status/JSON-RPC error/id mismatch、协议版本 header、可选 session header、session 404、新 session、DELETE 405、timeout/cancel、cross-origin redirect/endpoint auth canary、list tools 与 name round-trip、`isError`/nil result、span parent 和 cleanup。
6. MCP ambiguous call fixture 在远端先执行一次副作用、再丢弃 HTTP response/stdio response；客户端必须返回 unknown 并保持 side-effect count 为 1。对 initialize/list 等可安全重试的路径单独断言有界行为，不以一个全局 retry 开关混淆两类操作。
7. Web 生产路径若仍未连接 Agent，则真实二进制/页面需显示 unavailable，不接受 echo 却标记成功的测试。若接入，则另加 auth token、session identity、事件隔离、审批以及 Agent 输出端到端 fixture。

R6-BLD-01 仍依赖 `R3-REV-01`；R6-BLD-02 依赖 `R2-REV-01` 与 `R4-REV-01`。所有实现审查应对照本设计中的入口声明、wire version 和安全边界；发现代码无法满足时先修合约或明确延期，不能降低 acceptance 到名称检查或单元 fake。

## 官方 MCP 参考

- [2025-06-18 Lifecycle](https://modelcontextprotocol.io/specification/2025-06-18/basic/lifecycle)：初始化顺序、版本协商、initialized 通知、超时与 stdio shutdown。
- [2025-06-18 Transports](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)：stdio 行 framing、Streamable HTTP POST/GET、SSE result、会话头、protocol version header 与旧 HTTP+SSE 兼容要求。
- [2026-07-28 Streamable HTTP](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http) 与 [官方版本发布说明](https://blog.modelcontextprotocol.io/posts/2026-07-28/)：用于标记本轮明确不支持的现代行为边界；不得据此把本轮目标扩为 2026 wire implementation。
