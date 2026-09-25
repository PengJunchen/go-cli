# Runtime 源码审计（`internal/core`）

审计范围是 40 个 `internal/core` 生产源码文件及其测试文件，重点沿 `AssembleAgent → TurnRunner/AgentImpl → LoopAgent → Tool/SubAgent → EventStream/EventBus → Session` 检查实际控制流。审计以当前源码、定向测试和实际装配调用点为证据；旧设计稿只作检索线索，不作为结论依据。没有运行全套测试。三个缺陷有保存的最小回归复现，分别验证全局拦截器串会话、取消后历史缺少 tool result、续流错误被当成功；另有并行取消和 turn 终态问题由直接控制流审查确认。完整文件台账见 [01-runtime-files.txt](01-runtime-files.txt)。

## 架构判断

现有 Go Runtime 的主要切分可以保留：`Assembly` 负责依赖组装，`LoopAgent` 负责单次 ReAct 执行，`AgentImpl` 维护会话内存历史，`TurnRunner` 持有 turn 状态，`Harness`/`EventStream`/`EventBus` 负责事件消费投影，Session 层负责恢复与持久化。这套结构足以继续迭代，不需要另造 composition root，也不需要把整条事件流升级为权威 event sourcing。

问题出现在几个边界没有一个共同的提交/生命周期契约。运行时至少应保证：同一会话只有一个写入者；每个 turn 只有一个在执行和子工具停止后提交的终态；工具调用一经接受最终可观测为 `done`、`failed` 或 `unknown`；送给模型的 assistant tool-call 必须与全部 tool result 配对；UI、EventBus 和 SSE 消费的是可丢弃投影而非事实来源；关闭 turn/session 时要等待它拥有的 goroutine 退出。内存运行完成和 session durable commit 是两个阶段，取消时外部工具副作用应能落为“结果未知”，不能把取消请求直接当成已终止。

## 确认问题

### RT-01. 拦截器是进程全局的，多个 Assembly 互相影响

**严重度：中（P2）｜证据：已复现。作用域：多 Assembly / 嵌入式 SDK。** `assemble.go:1383-1385` 每次装配都把该实例自己的 Plan Mode 和 hook interceptor 追加到进程全局列表。`pre_tool_call.go:20-43` 显示列表不带 session/loop 标识，没有卸载句柄；`PreToolCallEvent.IsCancelled` 对每个工具调用都遍历整份全局列表（`:72-95`）。因此 Assembly A 激活 Plan Mode 后，Assembly B 的写工具也会被 A 的 controller 拦截；结束 A 不会释放 interceptor，新 Assembly 还继续累加。Hook 的 pre-tool-use 也受相同作用域问题影响。默认单进程、单 Assembly 的 CLI 操作不会触发跨实例干扰；多 Assembly 同时存在时会。

可复现测试源与结果：[plan-mode-cross-assembly-repro.go.txt](evidence/plan-mode-cross-assembly-repro.go.txt)、[日志](evidence/plan-mode-cross-assembly-repro.log)。复现模拟两次实际 Assembly 注册，A 的 Plan Mode active、B inactive，B 的写调用仍被拦截。修复应把策略绑定到本次 Assembly 的 Loop/ExecutionEnvironment，或显式传入 per-call policy；不应依赖全局 `ClearToolInterceptors` 做生产生命周期管理。

### RT-02. 取消 tool call 后 Agent history 留下不完整的模型上下文

**严重度：高（P1）｜证据：已复现并交叉核验入口。** `loop.go:486-507` 在取消时把合成 tool result 追加到 Loop 的局部 `messages` 并发出 `tool_result` event；这让本轮 Loop 自己保持协议完整。但 `AgentImpl.Run` 只从 events 找最后一个 assistant message，并仅追加其 content、usage、tool calls（`agent.go:138-153`），不会把 tool-result events 写进 `AgentImpl.history`。下一轮会把这段 history 的 ToolCalls 原样交给模型（`loop.go:463-480`）。所以取消/工具错误之后，Agent 内存历史可包含 assistant tool_calls，却没有相配的 tool-role message；provider 可能拒绝下一次请求，或不能可靠恢复。`LoopAgent` 还会在迭代间将 steering/follow-up channel drain 为 user message（`loop.go:526-540, 1046-1075`），但 `AgentImpl` 仅追加最初的 `submission.Content` 和最后 assistant event（`agent.go:124-153`）。入口核对确认没有外层持久化补录：REPL `persistSession` 只追加原始用户输入和 `turnResult.Message`（`repl_session.go:894-957`），steer/follow-up handler 只调用 TurnRunner 并记日志（`:794-806, 870-891`）；serve CoreHandler 对 ACP 消息 dispatch 后只排队回复（`internal/acp/core_handler.go:231-277`）。因此 follow-up/steering 也会从之后重建的对话上下文中消失。

最小复现由真实 `AgentImpl → LoopAgent → blocking tool` 调用链组成：[canceled-history-pairing-repro.go.txt](evidence/canceled-history-pairing-repro.go.txt)、[日志](evidence/canceled-history-pairing-repro.log)。现有 `cancel_turn_cleanup_test.go:158-170` 验证从本轮 events 重建时结果成对，但不检查 `AgentImpl.Messages()`，所以没有覆盖提交边界。建议每轮产出一个规范的 turn transcript，在成功、失败、取消时按同一规则合并 assistant/tool 消息，并在交给模型或持久化前做 tool-call/result 配对校验。

### RT-03. 并行工具取消后立即返回，未等待仍在执行的工具

**严重度：高（P1，工具若不服从 context 时影响最高）｜证据：控制流已确认。** `executeToolsParallel` 为每个工具启动 goroutine，并另起 waiter 等 `wg.Wait()`（`parallel_tools.go:71-98`）。context 取消分支只复制已完成结果，其余标为 `ctx.Err()` 后立即返回（`:100-125`），没有 join worker。`LoopAgent` 随后返回 error（`loop.go:628-654`），turn/UI 可以显示取消完成，而一个忽略 context、正在执行不可中断 I/O，或工具本身无法取消的操作仍会继续产生副作用。重试可能与旧操作重叠，调用方也不知道操作究竟失败还是已成功。

当前取消测试的阻塞工具都在 `ctx.Done()` 后立即退出（`condition_parallel_test.go:166-202, 218-252`），没有验证不服从 context 的工作在返回之后继续执行，也没有侧效状态。工具执行应有明确 accepted / done / failed / unknown 记录；turn 取消首先表示 `cancel_requested`，只有 worker join 或转入显式 unknown 后才能发终态。对于不能强制停止的工具，不能谎报“已取消副作用”。

### RT-04. continuation stream 出错会丢失错误并返回成功的截断答案

**严重度：高（P1）｜证据：已复现。** `generateWithContinuation` 首次拿到 `finish_reason=length` 后，续流错误或 nil response 在 `loop.go:933-935` 直接 `break`；随后只按“尝试次数用尽”的路径删除残缺 tool calls 并返回 `resp, nil`（`:955-972`）。因此网络失败或取消续流会被包装成成功，返回不完整回答，调用方无法区分这是模型完整答案还是 partial。

保存的最小复现使用第一响应 length、第二响应 stream error：[continuation-error-repro.go.txt](evidence/continuation-error-repro.go.txt)、[日志](evidence/continuation-error-repro.log)。当前断言观察到 `LoopAgent.Run` 返回 nil error、partial content。应保留续流失败/取消的 cause，并返回 typed partial outcome 或 error；用“次数耗尽”和“续流请求失败”两种不同终态，测试 UI 和调用方不会将 partial 当完成。

### RT-05. `TurnRunner.Cancel` 提前把仍在执行的 turn 标成终态

**严重度：中（P2）｜证据：代码直接确认；终态可提前观察。作用域：直接使用 `TurnRunner.Cancel` 的调用方。** `Cancel` 在 `turnrunner.go:222-225` 锁外读取当前 turn 和 cancel func，随后调用 cancel，再于 `:234-239` 立即把 `Status` 改成 `TurnCanceled`。`Turn.Done()` 仅按 status 判定，`turn.go:65-73` 因而在 RunTurn、工具及子 goroutine 尚未退出时就报告 Done。`RunTurn` 最终仍在工具函数返回之后才完成锁内收尾（`turnrunner.go:158-175`），所以 API 查询终态和实际执行生命周期不一致。建议加 `cancel_requested` 非终态；只有执行体收敛并 join 后由 RunTurn 单点提交 terminal。并为取消与完成竞态增加确定性屏障测试。

## 实现不完善 / API 契约缺口

### RT-06. 实际交互取消会被 TurnRunner 记为失败

**严重度：中（P2）｜证据：入口交叉验证。** REPL 为 turn 建立自己的 `context.WithCancel`（`internal/cli/repl_session.go:680-682`），快捷键路径直接调用 cancel，没有调用 `TurnRunner.Cancel`，也就没有设置 `Turn.Canceled`。`RunTurn` 按 `turn.Canceled`、`runErr` 决定 terminal（`turnrunner.go:158-169`），会将 `context.Canceled` 归为 `TurnFailed`；REPL 另一处却用 `turnCtx.Err()` 将结果显示为 interrupt（`repl_session.go:609,627-639`）。于是用户看到“已中断”，内部 turn 查询看到“失败”。让每个入口调用统一的取消协调器，并统一错误分类及终态映射。

### RT-07. `FailureSynthesisMiddleware` 会在用户取消后再试一次

**严重度：中（P2）｜证据：静态路径。** `DefaultFailureTurnSynthesizer.IsRecoverable` 将 `context.Canceled` 归入可恢复（`failure_synthesis.go:49-63`）；middleware 对可恢复 error 会用已取消的同一 `spanCtx` 再跑一次 Loop（`:221-266`）。Loop 会立即再次返回取消，但会多走一次包装路径、重复错误事件且可能干扰上层对一次取消/一次终态的判断。用户主动取消与可重试的模型瞬态错误应分类分开，不执行自动恢复重试。

### RT-08. SubAgent 的 follow-up 返回成功，但可能没有送达

**严重度：中（P2）｜证据：接口与实现不一致。** `SubAgent.Send` 不检查 state、不观察传入 context、对满 inbox 使用 `default` 丢弃，仍返回 nil（`subagent.go:287-305`）。调用方会认为指令送达，但实际只记录在 `Received`；已经结束或 buffer 满时不会进入 runner。`Interrupt` 对 idle/completed agent 也返回 nil，且会写入 `interrupted=true`（`:307-320`）；idle 时调用后再 Run，最终完成结果仍可能被标为 Interrupted。应让投递返回明确 ack/拒绝与容量状态，并只允许 Running 状态接收 send/interrupt。

### RT-09. Harness 的根 trace span 在异步执行开始前就已结束

**严重度：低（P3）｜证据：静态路径。** `Harness.Submit` 在 `harness.go:131-134` 创建根 span 并 `defer span.End()`，随后才 `go h.run(spanCtx, ...)`（`:157-161`）；真正执行和结果发送在 `:168-198`。因此 root span 的持续时间只覆盖启动 Submit，不覆盖 agent。真实 SubAgent runner 会走 Harness（`real_subagent_runner.go:111-138`），子调用仍有同 trace 但 root 时长和父子生命周期误导诊断。应把 span End 移到异步 run 收敛的 defer；同类核对 `turnrunner.go:92-95` 的 start span。

### RT-10. EventStream timeout/drop 只在发送方可见且 Loop 忽略发送错误

**严重度：中（P2，投影完整性）｜证据：静态路径。** EventStream 的 BlockUntilConsumed 默认有 30 秒 timeout（`eventstream.go:19-20, 81-107`），超时返回 `ErrSendTimeout`；但 Loop 本地 `sendEvent` 和增量 token send 都丢弃 `Send` 错误（`loop.go:431-435, 824-831`）。如果 UI/SSE 消费暂停或堵塞，关键 tool/result/error/done 事件可能缺失，而运行继续且没有 gap 状态。事件流是投影，不应把它变成执行事实；但消费端应拿到序号/gap 标记，运行侧应计数并暴露 drop/timeout，终态应从独立 turn/result 接口读取。

### RT-11. 缺省 SessionStore 的 Save 返回成功但不持久化

**严重度：中（P2）｜证据：源码已确认。作用域：直接使用 `NewRegistry()` 缺省组件的 SDK/测试调用方。** `NewRegistry()` 装入 `SessionStoreImpl{}`；其 `Save` 只写 slog 并返回 nil，`Load` 返回空 session（`stubs.go:30-46`）。对明确标为 stub 的过渡实现可以接受，但成功返回会让没有显式替换 store 的集成方误以为具备 durable commit。生产 Assembly 使用独立 Session 组件，不因此直接判 CLI 持久化失败。缺省 store 应返回 `ErrUnsupported`，或命名/构造方式明确表达 no-op。

## 合理设计与已覆盖保护

- `Assembly` 已经集中装配模型、工具、middleware、Agent、Harness 和 TurnRunner。CLI interactive/prompt 与 server 路径的消费适配不同，但可以继续复用同一 Runtime，不需要新 composition root。
- `LoopAgent` 默认顺序工具执行，按 ToolCallID 对齐并行结果（`loop.go:600-631`）；CLI 装配选择 parallel，可由配置切回 sequential（`assemble.go:1275-1291`）。该默认应和 R3 的取消语义一起评估。
- `EventStream.Close` 先通知等待者，再等持有 send read-lock 的发送者，之后关闭 channel（`eventstream.go:210-223`）；`MemoryEventBus` 在读写锁下移除和关闭订阅者，避免并发 send/close panic。队列满时事件丢弃与计数属于显式投影策略，不应误当持久化。
- Loop 已在本轮 cancellation event 上合成缺失 tool result，且按 call ID 避免重复；缺口在 Agent/session history 没有消费这个完整 transcript，而不是该合成机制本身。
- 主 CLI 在 Assembly 中为 Harness 和 TurnRunner 注入同一 `DefaultRunSlotGuard`（`assemble.go:1275, 359, 367`），所以直接 `AgentImpl.Run` 的并发问题不作为当前 CLI 的同等级生产 bug。零配置 `Harness`/`TurnRunner` 使用 noop guard 是文档化选择（`harness.go:57-61`, `run_slot.go:112-130`）；应在公共 API 上明确它不会保证 per-session single writer。
- `DefaultRegistry` 与缺省 `SessionStoreImpl` 等是明示的 stub；不过 stub 的 `Save` 返回 nil 而不持久化（`stubs.go:30-46`），集成方如果误把它当生产 store 会收到假成功。默认 registry 应标注不适用于 durable runtime，或 stub 返回 unsupported error。

## Runtime 迭代建议

1. **I0：先定契约并补回归门禁。** 将上述单写入者、唯一 quiescent terminal、tool accepted/done/failed/unknown、tool-call/result pairing、非权威 event projection、close/join 写成 Runtime contracts。把三份定向复现并入永久测试；再补并行工具不服从 context、RunTurn 与 Cancel 竞态、实际 REPL Ctrl-C terminal、失败恢复取消不重试、SubAgent inbox overflow 的测试。全套测试由主审计验证，不在本轮运行。
2. **I1：收口执行取消与工具副作用。** 在现有 Loop/TurnRunner 上加入明确取消请求和工具操作状态；并行 dispatcher 需要报告每个 worker 是否收敛或 unknown。RunSlot 保持单 writer，TurnRunner 是唯一 terminal 提交点；取消等待 join，或在界面明确标记 still-running/unknown 并禁止可能重复副作用的盲重试。
3. **I2：提交规范 turn transcript。** 由 Agent 从一次执行拿到完整 assistant/tool 记录，而非只提取最后 assistant event。验证每个 tool call ID 恰有一个结果；成功/失败/取消都走同一个 history append 与 compact 路径。再由 session 层在确定 durable commit 边界写 session，恢复时校验 transcript。内存成功不等于磁盘提交成功；磁盘失败须呈现给用户并可重试。
4. **I3：将政策、事件与关闭生命周期按 Assembly 隔离。** 替换 process-global interceptor 为实例作用域策略；hook 调用接收真实 context。事件携带 run/session ID 与单调序号，丢失时发 gap 供 UI/SSE 恢复，权威状态从 turn/result/session API 查询。Harness、SubAgent、dispatcher 和 stream Close 都应 join 本组件拥有的 goroutine，并让 root span 覆盖该异步生命周期。
5. **I4：稳定后打磨扩展契约。** 修复 SubAgent Send/Interrupt ack，默认 Stub 显式暴露 unsupported，给失败返回 typed error/partial result。保留现有 Extension、Hook、middleware 与 registry 接口分层，只在观察到具体调用痛点时合并接口。

## 测试证据与覆盖说明

没有运行全套测试。针对 R1、R2、R4 的一次性测试分别运行通过，其源码与日志保存在 `evidence/`；“PASS”表示复现当前缺陷的断言成立，不表示产品行为符合预期。重点核对的既有测试包括 `cancel_turn_cleanup_test.go`（仅 Loop events/transcript）、`condition_parallel_test.go`（协作式 context 取消）、`turnrunner_test.go`（取消后等待 RunTurn 完成才检查终态，但未在运行中查询 Done）、`loop_stoplength_test.go`/`e2e_stoplength_test.go`（正常续流和次数耗尽，没有续流错误）、`run_slot_test.go`（取消时 slot 已被占用）及 `subagent_tool_test.go`（adapter 正常路径，未测带部分结果的 error）。其余生产与测试文件按台账纳入覆盖；另有 CLI/session/tools/LLM 的深审由并行审计文档负责，本报告仅引用和核对实际跨边界调用。

## 未覆盖与限制

- 未运行全套、race、E2E/终端 UI 测试；本轮只运行上述三条定向复现。尚未用真实远端模型/工具验证 provider 对 orphan tool_call 的具体报错文案。
- 并行工具“不服从 context”后的实际副作用大小取决于具体 Tool 实现；审计确认 runtime 提前返回且不 join，影响范围是运行时合同层面的高风险，具体工具逐个行为由 tools 审计补充。
- `docs/audits/2026-09-24/01-runtime-files.txt` 提供 40 个 core 源文件及测试文件的清单。测试清单覆盖范围按文件和测试名核查；源码问题按本报告引用的实际执行路径逐行核对。
