# R7 扩展、技能与遥测生命周期设计

状态：设计预研，尚未实现、尚未运行本文件列出的验收。
依据：`docs/audits/2026-09-24/07-extensions-tracing.md`、`docs/iterations/2026-09-25-runtime/execution-tasks.json` 的 R7-DES-01 / R7-BLD-01 / R7-BLD-02 / R7-REV-01，以及当前默认 CLI 装配链源码。
范围：把 extension、skill、tracing 的声明能力与默认运行路径对齐；不重做 MCP transport，不在本轮设计里实现代码。

## 决策摘要

1. 一个 CLI invocation 只创建一个 Tracer 和一个 exporter owner。根命令创建并关闭它；Agent assembly 复用其 context/Tracer，不另建 trace identity，也不关闭父级 exporter。
2. Assembly 的每项资源在成功创建的同一位置登记一次关闭动作。关闭按依赖逆序执行、可重复调用、接受 deadline、返回汇总错误。关闭先关 admission 并复用 R1 run owner；不复制一套 run 生命周期。若可信 in-process `.so` callback 或 exporter 不理会 context，deadline 只能让 caller 收到 `cleanup_pending`/quarantine，不能承诺强杀或无泄漏退出；原 owner 与依赖 sinks 保持可达，直到资源确实返回/停止后才继续关闭。
3. Extension registry 的公开枚举保持注册顺序；每个 extension 的注册先进入私有 staging registry，Init 成功才原子提交。仅成功提交的 extension 执行一次 Shutdown。Load/Init/Shutdown 失败必须汇总并能由 assembly 调用方观察。
4. Hook 输入是只读快照。首期只承诺公开的 `HookActionPass` 与 `HookActionBlock`；未知 action、`terminate`、`replace` 返回显式 unsupported 错误，不再折叠成含糊的“halted”。
5. Skill 文件有明确的文件数、单文件字节数、根目录总字节数和取消边界；拒绝逃出根目录的符号链接。Project skill 必须消费 R6 已冻结并验证的 source bytes 与 directory membership；未有该 snapshot/trust handoff 时 fail closed，不按 path 重新打开。Skill 参数声明转换成有限 JSON Schema 子集，通过 R4 `PreparedInvocation` 冻结 schema/规范参数，并在 approval 前校验。当前 `tools` 声明只表示建议；它不能当作工具授权 allowlist。
6. 默认可用 exporter 只有本地 JSONL 与 stdout。`none` 和 `enabled=false` 都创建 noop tracing 且绝不输出；未知 exporter fail closed。当前名为 `otlp` 的实现不满足标准 OTLP 协议，当前 Kafka 实现是自定义 TCP 帧；在通过对应外部 conformance 前不向用户宣称支持。
7. async/event forwarding 明确采用有界 best-effort：调用方可区分已入队、丢弃、sink 失败、关闭后拒绝和截止时未排空；不宣称 at-least-once 或 lossless。CLI 默认通过 invocation owner 的单一 `ShutdownReport` 观察 exporter 计数、pending 与 close errors；close errors 影响 command 返回状态/exit code，不以 logger-only 代替。

R0 正在处理 trace/log 隐私修复。本设计不重写敏感字段识别或默认脱敏补丁；R7 必须消费 R0 的实现，并把 prompt/secret sentinel 测试作为整条 CLI + assembly 导出链的合入门槛。

## 当前默认装配和所有权

真实入口是 `cmd/cli/main.go:73-110` 的 `newTracing → runCLI → cli.Run`。普通命令可进入 `internal/cli/prompt.go` 的 `AssembleAgent`，交互会话经 `internal/cli/repl_session.go` 进入同一个 assembly builder。因此 assembly 生命周期不是测试专用路径。

当前 `AssembleAgent`（`internal/cli/assemble.go:314-460`）按此顺序执行：

```text
newAssembleState
  → assembleModel
  → assembleTracing
  → assembleTools（含 MCP、skill）
  → assembleExtensions（Load / Init / 注册 Hook 与 Middleware）
  → approval / resilience / output guard / subagent / extra tools
  → loop agent / middleware
  → compactor
  → session
  → appendFinalCleanup
  → restore history / Agent / Harness / TurnRunner
```

清理顺序来自 append 次序，而非显式依赖图。`runCleanup`（`assemble.go:628-632`）对 slice 倒序调用，无 once、无 context、无错误聚合。`assembleExtensions` 在较早位置登记 PluginManager Shutdown（约 `927-950`）；`appendFinalCleanup` 后登记，LIFO 会先执行终端清理：stop 热重载、关 EventBus/ACP、关 session/memory、最后关 tracing exporter（`637-661`），然后才 Shutdown extensions。Extension Shutdown 因而可能在其 sink 已关闭后写 span；session/event bus 也先于可能仍使用它们的 extension 关闭。`appendFinalCleanup` 只在 compactor 和 session 装配之后才调用；若 tracing 已创建，而 `assembleTools` 或 `assembleCompactor` 返回错误，早期 `runCleanup` 看不到 exporter 的 cleanup closure。现有几个资源 cleanup 也忽略错误并使用 `context.Background()`。

Tracing 有两个默认 owner：`cmd/cli/main.go:newTracing` 创建 CLI 根 tracer/exporter；`internal/cli/assemble.go:768-784` 的 `assembleTracing` 再按同一配置创建 assembly tracer/exporter。根 span 的 context 经过 `cli.Run` 传入 assembly，但 Agent Harness 显式持有 assembly tracer。`Tracer.Start`（`internal/tracing/span.go:112-137`）用本 tracer 的 traceID，而只从 parent context 取 parentSpanID；跨 tracer 创建的子 span 可能有父 span ID 却使用另一 trace ID。默认 JSONL 可能因此写入两个文件/两棵不相连的 trace。

启动错误可从当前 buildModel、tools、compactor 分支触发；其中 tracing 先于后两者创建。源码足以证明 cleanup closure 尚未注册，并非依靠动态测试推测。R7 需要在实际默认 assembly 上增加失败注入测试确认 exporter Shutdown 一次、没有遗留 goroutine/文件句柄。

### Runtime 生命周期目标

- 由最外层 invocation/runtime 持有 Tracer/exporter。`AssembleAgent` 接收同一 Tracer（或仅接收含它的 context），不重建 exporter；Assembly shutdown 只关闭自己创建且拥有的依赖。最外层先关闭 Assembly 和所有 span producer，再结束 CLI root span，最后 flush/shutdown exporter。
- Assembly 资源一旦 acquire 就立刻登记对应 disposer，避免“清理注册得太晚”。注册失败/后续装配失败统一执行当前已注册项。
- 增加可报告错误的 `Close(ctx) error` contract；旧 `Cleanup func()` 只可作为兼容 wrapper，prompt、REPL、serve 和 CLI root 的正常生产关闭必须调用有 context、能返回 close error 的入口。close owner 状态为 `open → closing → cleanup_pending/quarantined → closed`，只在中间状态实际出现时经过；首个调用在同一个 shared admission fence 上拒绝新 run，其余并发 close 等同一个结果。唯一 R1 seam 是给现有 guard 增加窄 `RunAdmissionCloser.CloseAdmission()`：同一 guard 的 `ClaimRun` 与该关闭操作在其自身同步下线性化；关闭前已取得的 claim 仍交给原 R1 owner，关闭后新 claim 返回 closing error。Assembly 必须把同一个支持该 fence 的 guard 传给 `Harness.Submit` 与 `TurnRunner.RunTurn`；若生产装配拿到的 guard 不可关闭 admission，fail closed，不另建第二个不一致 fence 或 Run owner/supervisor。
- `Harness.Submit` 与 `TurnRunner.RunTurn` 必须检查 assembly 共享的 admission fence；关门后新请求返回明确的 closing error。关闭等待在途 run 时只使用 R1 `RunClaim`/`RunStatusHandle` 和 owner 的 quiescence 信号。活跃 run 或资源进入 `cleanup_pending`/quarantined 时，保留原 R1 owner、claim、worker/resource handles 与其仍需使用的 sinks；不能因 caller deadline 到期就释放 slot 或标成 quiescent。
- 清理顺序按资源依赖：关闭 admission → 停 watcher 与 event producer → 通过 R1 owner 等待在途 run 及 extension callback → Shutdown 已启动 extensions 和 MCP/ACP 连接 → 关闭 event/session/memory 等写入目标 → 确认所有仍会产 span 的服务已停 → flush/shutdown exporter。依赖 sink 必须晚于所有 producer；某个阶段遇错或超时仍继续安全可继续的清理，但不得关闭仍有 producer 的 sink。
- Caller context/deadline 只限制 caller 等待和 cooperative callback；可信 in-process `.so`、Hook 或 exporter 可以忽略取消，Go 无法强制终止它们。到期返回明确 `cleanup_pending`/quarantine 错误并保持原 owner、跟踪句柄和依赖 sinks 可达；不声称强杀、不丢弃未跟踪 goroutine。若 callback 后来真实返回，原 owner 恰好一次续完剩余依赖清理并变为 closed/quiescent。
- 每个 close caller 都把 cleanup 失败体现在命令结果中：`prompt.go`、`repl_session.go`、`serve_cmd.go` 返回/join assembly close error；`cmd/cli/main.go` 将 root exporter shutdown error 转成非零 CLI exit，而不是只打印 slog。Assembly setup 同时遇到 build 错误和 cleanup 错误时用 `errors.Join` 保留二者，并保留 build error 上下文。
- 所有异步组件的 owner 明确且局部于一次 invocation/assembly；移除依赖 package-global 列表来代替 owner 的清理语义。关闭函数与 caller context/deadline 串通，但对不合作资源的实际停止只由 owner 收到真实完成证据后认定。

## Extension 生命周期与事件合同

### 顺序、并发、失败

当前 `DefaultExtensionRegistry` 按 map 保存工具/hooks/middleware，`All*` 遍历 map（`internal/extension/registry.go`）；相同配置下次序可能变化，而 `assembleExtensions` 用该次序组 `HookChain` 与 middleware onion。Middleware 顺序会改变调用内外层，Hook 顺序会改变哪个 hook 先 block，必须稳定。

目标顺序是：配置 PluginPaths 顺序 → loader 返回的 Extension 顺序 → extension Init 的注册顺序。不同插件同名扩展或 building block 按明确 duplicate policy 报错；不得静默覆盖。registry 在 mutex 内生成带顺序的快照，getter 返回的 slice 是副本。测试固定注册顺序，并验证 middleware 第一项为最外层的约定。

`extensionCoordinator.initExtension` 目前在锁外调用 Init、成功后才写 `managed[name]`（`internal/extension/manager.go:60-74`），无法阻止并发双 Init，也无法识别正在初始化；失败 extension 已直接向共享 registry 注册的内容不回滚。Shutdown 对任意加载项调用，忽略状态且没有 stopped 防重。`PluginManager.Init/Shutdown` 都丢弃聚合错误并返回 nil（`internal/extension/plugin_manager.go:60-91`）。

目标每个实例的状态为 `pending → initializing → running → stopping → stopped`，失败为可查询的 `failed`；在调用扩展代码前先原子预留实例状态。同实例并发 Init 只执行一次，其余等待并获得同一结果；Init 期间 Shutdown 等待完成或按 ctx deadline 返回。成功 Init 的 extension 逆序 Shutdown 严格一次；失败 Init 不调用正常 Shutdown（如插件 API 要支持部分 acquire，需显式提供 rollback hook，否则 Init 必须自行回滚）。Shutdown 失败仍记为 `stopped-with-error`，不得第二次无条件重跑副作用。

每个 extension Init 使用与父 registry 同接口的 staging registry。提交时在锁内检查全部名字冲突，再按注册顺序原子合并；Init 失败/冲突则丢弃 staging，避免半个 extension 对运行中的 agent 可见。Load 可继续尝试后续路径，但 `errors.Join` 必须保留路径错误；默认 assembly 可以保留已成功 extension 的部分可用策略，但要将加载/初始化错误和 failed extension 列入 `AssemblyDiagnostics`（及结构化日志），CLI/TUI 可以区分 ready/degraded，不可只记 warning 后宣称全部 ready。

### Hook 输入与 action

`HookEvent` 注释称 immutable，但 `Data any` 没有保护。`internal/cli/extension_bridge.go` 直接传 `core.Submission`，其中 slice/map 与调用方共享；AfterRun 再把 submission/result 放入 map。Hook 可能修改后续 agent 用的数据。

桥接层应复制允许暴露的 DTO 到只读语义快照：提交文本/附件 metadata、消息 slice、运行结果及其 map/slice 均递归复制至受支持的 JSON-like 值；无法安全复制的对象不给 hook，改成稳定字符串/结构化标识。`BeforeRun` 与 `AfterRun` 分别拿独立副本，hook 对副本的修改不可影响下一 hook 或 Agent；写清文本内容是否属于 R0 隐私字段，默认 trace 不记录它。

当前公开只导出 `HookActionPass`；block/terminate/replace 常量为包内私有，adapter 对任何非 pass 一律返回相同错误，也不使用 `Replacement`（`internal/extension/types.go`、`internal/cli/extension_bridge.go:27-72`）。R7 导出 `HookActionBlock` 并只支持 `pass` / `block`：pass 继续；block 在 before-run 变成带 extension/hook 标识但不泄露原始请求内容的明确拒绝错误；after-run block 不回滚已完成运行，只报告 hook observer failure。未知 action 返回 `ErrUnsupportedHookAction`。外部 package fixture 必须能编译并返回 `extension.HookActionBlock`；另测 terminate、replace 与任意未知值显式 unsupported。要支持 terminate/replace，必须另行设计取消边界、输入替换验证与审计语义，不能仅导出常量。

`.so` 插件是同进程 Go plugin，视为完全可信代码，不提供 sandbox。HTTP loader 当前取回的是描述符，`rpcExtension.Init/Shutdown` 只记录日志（`internal/extension/plugin.go:147-163`）；没有远程 Tool/Hook/Middleware 执行协议。`grpc://` 返回 `ErrUnsupportedRPC`。配置/API 应在选择 remote path 时清楚报 `descriptor-only / unsupported execution`，不能显示成可执行 remote extension。commands/providers 目前只有写接口和包内 getter，PluginManager/CLI 没有消费出口；`defaultConfigProvider.Load/Watch` 返回 `errDefaultConfig`（`internal/extension/default.go:14-29`）。这些要么补齐 caller 与端到端测试，要么在公开文档和能力报告标为 dormant/unavailable，不应假装已接入。

### EventBridge 的取消与观测

`EventBridge` 文档称 FIFO、completeness/no-loss（`internal/extension/bridge.go:36-80`），但转发 goroutine从 source 取走事件后，若 out buffer 满且 ctx 取消，会走取消分支退出，已消费事件没有被投递；`Wait` 只等生产 goroutine 退出/关闭 out，不代表消费者读空了 out。

R7 选择有界 best-effort 合同：保持单 reader/FIFO；转发成功计入 `forwarded`；source 关闭时正常关闭 output；取消时已读但无法入队的事件计入 `dropped_after_read`，不伪称 drain 完成；尚未从 source 取走的事件归 source owner 处理。`Wait` 只承诺 forward goroutine 退出，输出 channel 可能仍有缓冲数据。提供线程安全只读 Stats/错误结果，计数器不得包含 event payload。若未来选择取消时 drain，必须有独立短 deadline 与 buffer 上限，并报告 deadline 时遗弃量。验收填满 out buffer、确认 source 已读后再 cancel，断言 bounded return 且事件计数准确。

## Skill 文件、参数和能力边界

### 生产路径和资源限制

默认 assembly 在 `internal/cli/assemble.go:920,1942-1991` 直接创建 `YAMLSkillLoader`，遍历目录后把每个 skill 注册为 deferred tool；skill name/description/category 另注入 system prompt。`DefaultSkillRegistry.Match`、全局 loader/registry（`internal/skill/skill_registry.go`、`registry.go`）没有进入该默认路径，因此 progressive disclosure 当前是 library API，不是 CLI 能力。选择 R7 的最小切口：沿用当前 load-all-as-tools 行为并清楚报告数量/失败项，或者明确将 API 标记 dormant；若要接 Match，另做真实 agent 召回/选择流程和预算验收，不以创建 registry 实例充当接线完成。

Project-sourced skill bytes 必须来自 R6 已冻结的 project-source snapshot/manifest：用同一份 immutable bytes 计算摘要、解析 frontmatter、建 `SkillDefinition`、注册 deferred tool 并生成 prompt；R7 不重新发明一份 trust manifest，也不在 fingerprint 后按路径 `os.Open`。快照须保留 R6 authority 给出的 canonical path、相对 source identity、present/absent 状态、内容 digest 与精确 bytes；skill directory 的排序成员清单也要来自该次冻结 manifest，以检测新增/删除/重命名。显式 `rc.Skill.Dir` 不绕过 project trust；配置的 skill root 或文件若在 project trusted canonical root 外，不继承该 project 的信任，除非 R6 为该 source 提供独立可验证的 identity。R6 snapshot/trust seam 未接好、manifest 在消费前已变、快照缺失/不可信，或 loader 只能按路径重读时，该 project skill source 必须不可用并 fail closed。

loader 每行上限 4 MiB，但没有总文件、总目录、候选文件/skill 数量限制；LoadDir 只在 WalkDir 开始前检查 context，遍历/逐行扫描不持续检查取消。WalkDir 不跟随目录 symlink，但候选文件路径由 `os.Open` 跟随，可能读取根目录外内容（`internal/skill/skill_loader.go:80-170`）。设计建议明确初始上限并配置化前先以默认常量实施：单文件最多 1 MiB、每 root 最多 128 个候选文件/成功 skill、每 root 累计读取最多 16 MiB；遍历每个 entry 和读取固定大小 chunk 时检查 `ctx.Err()`，用 bounded reader 在超限时稳定报错。根目录 canonicalize；拒绝所有 symlink skill 文件（最简单可审计）或解析真实路径并确认其仍位于 canonical root 内，路径前缀比较需 `filepath.Rel` 防 sibling-prefix 绕过。

扫描顺序排序为相对路径字典序，使重复 skill 名和错误报告稳定。返回 `SkillLoadReport{Loaded, Skipped, Errors}`；坏文件可继续跳过，但每个失败按相对路径汇总给 CLI，不能只在内部 logger 留一行。标准 YAML v3 已在 go.mod 作为间接依赖；建议解析明确定义的 frontmatter mapping，拒绝未知类型、重复字段、非 mapping 根对象和超深/超大节点。若决定保留现有子集，则版本化记录 subset 并对不支持语法报清楚错误，不要把手写解析器称作完整 YAML。

### 输入参数与工具能力

`SkillAdapter.Execute` 目前完全忽略 `call.Args`，只返回 prompt 与 metadata；description 把 parameter key 当说明文字，没有 `tools.Parameterized` 实现（`internal/skill/skill_adapter.go`）。`SkillDefinition.Tools()` 也没有被执行授权层检查，底层全局 tool registry 依旧可用。因此声明工具名单是模型提示而非安全边界。

R7 建议用 `tools.Parameterized` 已有接口暴露 JSON Schema，并由 R4 `ToolCallPreparer.PrepareToolCall` 冻结同一份 schema/digest、规范化 arguments，再对该 normalized call 做 schema 校验；顺序必须是 schema validation → approval → durable dispatch admission/dispatch。无效参数在 approval callback 前失败且不派发，执行端不能另读可变 schema 或重走按名称 lookup。仅承诺所支持子集，其他关键字在加载时拒绝；参数大小/嵌套深度也有上限。兼容旧 `parameters: {key: scalar}` 时把它解释为默认值而非 schema；缺省值如何覆盖、未知字段是否拒绝必须写入前向兼容测试。`SkillDefinition.Tools()` 仍只是 description，不能改变 R4 为注册 executor 绑定的 trusted `EffectSet`；动态 skill/custom registration 默认 unknown，工具名单不是授权 allowlist。若 skill 需要真正 tool allowlist，应由独立 scope dispatcher 执行每次调用前授权；在该 dispatcher 未实现前，把字段改名/文档标为 `suggested_tools` 并明确 unsupported enforcement，不要通过 system prompt 暗示安全控制。

`DefaultSkillDefinition` 仅浅复制 map；`.Tools()` 与 `.Parameters()` 返回内部 slice/map。注册时将已校验字段深拷贝成不可变快照，getter 返回副本，避免注册后外部修改改变 prompt/schema。

`internal/core/system_prompt.go:268-281` 把可配置 name/category/description 拼成 XML 属性；`%q` 处理 Go 字符串引号，但不做 XML 属性编码。构造 XML 时对每个字段正确转义或用结构化 serializer，并用含 `"/><system...`、`&`、`<` 的 metadata 测试解析结果节点数与内容。该 XML 边界与 R0 trace 隐私责任不同，属于本 R7 skill 变更。

## Tracing exporter 支持矩阵

当前默认配置在 `internal/config/loader.go:281-292` 开启 tracing，exporter 为 `jsonl`；validator（`internal/config/validator.go:43,72`）只允许 `jsonl`、`stdout`、`none`。但 builder 有两个版本：main `newTracing`（`cmd/cli/main.go:127-165`）仅处理 stdout/jsonl，其他值退回 stdout；assembly `buildTraceExporter`（`internal/cli/assemble.go:2281-2303`）把未识别值一律当 JSONL，`none` 因而会落盘。下面矩阵按**真实默认 assembly 路径**描述，而非按类型名推断协议。

| 配置/实现 | 当前真实行为 | R7 对用户承诺 |
|---|---|---|
| `jsonl` | 默认可达；本地 JSONL exporter，assembly 同步写，main 根 tracer 包一层 async；目前会出现双 tracer/exporter | 支持单一 runtime owner 下的本地 JSONL；是否 durable 按文件写/flush 错误报告，不能从 enqueue 成功推断已持久化 |
| `stdout` | main 与 assembly 分别创建 sink；支持本地调试输出 | 支持；须保持统一 redaction policy，并由同一 owner flush/close |
| `none` | main 不建 tracer，但 assembly switch default 建 JSONL；当前配置可接受 | 修复后 no-op，零文件/零 stdout；与 `enabled=false` 一样，no exporter/no background worker |
| `otlp` | validator 默认拒绝；builder 有名为 OTLP 的 exporter，但 body 是 `{spans:[...]}` 的自定义 JSON，不是 OTLP ExportTraceServiceRequest；main 对未知值回落 stdout | 暂不支持；配置 validation 明确拒绝，或待改用官方 OTLP exporter 后以真实 collector conformance 打开 |
| `kafka` | 存在库 exporter，但 assembly 配置 builder 未选择它；wire 是长度前缀 JSON/TCP，非 Kafka protocol；压缩名称/实现也需核实 | CLI 不支持；如果保留仅可称内部自定义 TCP debug sink，不叫 Kafka，不能列入 tracing exporter 配置 |
| 未知字符串 | main 退回 stdout，assembly 退回 JSONL | 启动配置错误，fail closed，禁止静默切换 sink |

采用唯一 `BuildTracingRuntime(cfg)` 入口先归一化 enabled/exporter，再创建 exporter/tracer。Exporter 值先做严格 switch；disabled/none 直接 noop，不开文件、不起 goroutine；已启用但 sink 创建失败应由 assembly 返回可见错误或 `degraded` diagnostics，不能无声降级到 stdout，也不能关闭 tracing 后宣称有 trace。配置 validator 和 CLI builder 使用同一支持表，单测覆盖每个合法值和未知值。

OTLP 要开放前，至少以标准 OpenTelemetry Collector 或兼容官方接收端验证 request encoding、HTTP status、partial success、资源/scope/span mapping、shutdown flush、timeout；本仓自建只读懂自定义格式的 `httptest` 不算互通。Kafka 要开放前使用真实 Kafka client/broker 做 metadata、produce request、auth、compression、delivery ack 和 shutdown 验证；自定义 tcp mock 只验证自定义协议。当前设计没有执行这些外部测试，因此这些状态仍是 unavailable。

## Trace identity、状态和递送

单 tracer 下所有 span 从一个 root context 派生；`Start` 要从 parent span context 继承 traceID，并校验 parent 和 tracer 是否兼容。若跨 tracer context 则返回显式错误或建立新的 root，不能只复用 parentSpanID。Trace tree index 以 `(trace_id, span_id)` 为键，排序按 start time/spanID，不能靠 map iteration 输出不稳定树。

每个返回 error 的 span owner 在所有 return path 前写 `ERROR` status；取消使用明确的 cancelled/error 约定；成功路径才是 `OK`。当前 `localSpan.ToSpanData` 未显式设置状态时导出默认 OK（`internal/tracing/span.go:276-284`），这会掩盖失败；R7 加统一 defer/finalize helper 或保证所有函数显式设置。状态消息不放 raw prompt、secret、工具参数或大块 provider body；具体 redaction/filter 以 R0 决议为准。

当前 `AsyncExporter` 文档写“at-least-once”，但队列满会 warn 后返回 nil；关闭后仍可 enqueue 到无人消费的 channel；Shutdown 不理会 ctx 就 `wg.Wait()`；内层 exporter error 只写 log 并丢 span（`internal/tracing/async.go:59-126`）。`OTLPTraceExporter.flushBatch` 在发送前清空 batch，失败后丢整个 batch（`exporter_otlp.go:147-164`）。此外当前 main 包一层异步，assembly 同步写，语义取决于入口。

R7 选择“有界 best-effort + 可查询计数”的一个统一语义：

- `ExportSpan` 返回 nil 仅表示 accepted/enqueued；队列满返回可分类 `ErrQueueFull` 并使 `dropped_total` +1，不能返回 nil。
- 从 closing 状态起拒绝新 span（`ErrExporterClosed` + `rejected_after_close_total`）；状态变化与 send 使用同一同步协议，不允许 close 后入队。
- Sink 成功、失败分别累计 delivered/failed，失败记录只留稳定错误类别、sink 名、数量和 trace/span ID，不序列化 span 内容到诊断日志。
- Shutdown 先停止接收，限定 deadline 排空已入队事件。期限内完成则 drain；到期则记录 `abandoned_on_shutdown_total` 和返回 deadline/error。绝不宣称至少一次；不自动重试非幂等远端 sink。JSONL 同步写失败返回错误。
- 默认 CLI 只通过 `cmd/cli/main.go` 的最外层 invocation finalizer 返回一个 `ShutdownReport` 观察 exporter 递送情况；report 至少含 `ExporterStats`（accepted/delivered/queue-drop/sink-failure/rejected-after-close/abandoned-on-shutdown）、`cleanup_pending` 状态和是否可最终 close 的状态，join 后的 cleanup errors 作为该 finalizer 的 `error` 返回。不要同时提供另一个默认 `Stats()` polling/output 路径，也不要用 logger-only 记账。报告不含 span/event/prompt/secret payload；0 与 unavailable/pending 必须可区分。命令把 finalizer 的 aggregate error 映射成非零退出。非合作资源 deadline 后仍由同一 owner 持有，重复 close 等待/读取同一 closure 状态，不能遗失 owner。

Async wrapper 和底层 batch exporter 不能各自丢弃且都返回成功；只有一个层级管理 bounded queue/delivery result。若保留每 span sync interface，sink batch failure 应反馈给该层且完整记录失败批次数。测试模拟队列满、sink error、close race、cancel deadline，断言状态及 counter，而非仅确认 slog 调用。

R0 的 privacy 复测必须使用唯一 prompt/secret sentinel，通过 `cmd/cli` 真正组装、调用一个工具并触发 extension hook，读取最终 JSONL 与 slog 文件，断言 sentinel 不存在；测试不代表真实日志/UI全路径时不能写成全路径通过。

## Telemetry key encoding 与有界 label domain

当前 `internal/production/telemetry.go:109-133` 以逗号和等号拼接 metric name/label map；逗号、等号或括号出现在 key/value 中会让不同 tuple 形成同一 `Snapshot` key。`metricKey` 必须改为版本化、结构化的 canonical tuple 编码：metric family、label 数量、按 label key 字节序排序后的每个 key/value 都以 UTF-8 byte length 前缀编码（或等价的无歧义二进制 framing），不通过分隔符猜边界。此编码对规范化后的 tuple 是 injective；不声称源值被刻意 fold 到同一个有限 bucket 后还能彼此区分。

内建 producer 在写入前定义有限 metric family、固定 label key 和有限 value domain。当前 CLI 默认 producer 只使用 `tool.call.count`（`tool_class`、`cache_hit`）、`tool.call.duration_ms`（`tool_class`）、`llm.tokens.input` 与 `llm.tokens.output`（无 labels）；`tool_class` 的有限值为 `read|mutation|process|web|mcp|skill|extension|other`，由 instrumentation producer 分类，未知/自定义工具统一 fold 到保留 `other` 值，`cache_hit` 仅 `true|false`。未经认可的 label key 对该 family 返回可分类错误；超出有限 domain 的值只 fold 该 label key 的值到 `other` 并增加固定 `unknown_label_value_total` 计数，不把整个 tuple 或其它 label 维度折成一个桶。原始 tool/extension 名称、路径、prompt、model credential 均不能成为内建 metric label。调用者自定义的其它 metric 仍受同一个总 series cap 约束，不能获得无限 cardinality。

`DefaultTelemetry` 的 regular series 总上限固定为 `DefaultMaxSeries = 1024`。达到上限后，已有 tuple 继续聚合；新 tuple 被拒绝/丢弃并累计无标签 `series_limit_dropped_total`，不得把所有超限 tuples 静默并进同一个普通 `other` series。`unknown_label_value_total` 与 `series_limit_dropped_total` 是固定、不可由 caller 伪造的 reserved snapshot counters，不计入普通 series cap，不通过再次调用 `Record` 递归上报。测试需从 `DefaultTelemetry.Snapshot` 读取它们；这是 telemetry 的数据 snapshot，不是 exporter stats 的第二观测出口。

## R7 owner close callers 与跨层验收

Resource close 由 assembly 记录 disposer 和调用 R1 owner；调用入口的文件责任分开：`internal/cli/assemble.go` 的 extension/skill acquire、其 close fence 与资源登记由 R7-BLD-01 所有；`cmd/cli/main.go` 的 root tracing/exporter finalizer 及 `internal/cli/prompt.go`、`repl_session.go`、`serve_cmd.go` 的 close-error 返回接线由 R7-BLD-02 所有。两个 builder 不并行覆盖 `assemble.go`；若 trace injection 需要修改同文件，先由 BLD-01 整合其 assembly 生命周期改动，再由 BLD-02 提交独立 hunk/cherry-pick。R7-BLD-01 在已有 `internal/core/run_slot.go` 仅增加 `RunAdmissionCloser.CloseAdmission()` 与同 guard `ClaimRun` 的原子 admission fence；`Harness`、`TurnRunner` 继续注入同一个已有 guard，不修改 R1 owner。R1 `RunClaim`、worker tracking、status handles 的语义和文件 authority 保持归 R1，不重写、不复制。

端到端 close acceptance 走默认 assembly/真实调用方：`TestAssemblyCloseClosesSharedAdmissionAndRetainsR1Owner` 竞争 `CloseAdmission` 与 `Harness.Submit`/`TurnRunner.RunTurn`，证明 fence 之后无新 claim、已有 `RunClaim` 仍由 R1 owner 收尾；若阻塞的 trusted extension shutdown 或 exporter sink 忽略 ctx，deadline 前后返回 `cleanup_pending`，原 owner/sink 仍被保留、report 不声称 closed/drained/quiescent；释放阻塞后同一 owner 只做一次剩余 shutdown，完成后再关闭依赖 exporter。注入 assembly setup error + cleanup error，断言二者都能从错误链找到。分别从 prompt、interactive REPL、serve 与 root CLI 触发 close failure，断言错误返回到命令调用方且 CLI exit 非零；root exporter failure 不能只打印 slog 后成功退出。

## API 能力状态

| 表面 | 当前状态 | R7 处置 |
|---|---|---|
| 本地 Go `.so` Extension | 可加载并在进程内 Init；没有隔离 | 支持但标为 trusted in-process；Init/Shutdown、注册失败、生命周期并发有真实 assembly 验收 |
| HTTP Extension endpoint | HTTP descriptor loader，rpcExtension 生命周期为 no-op，没有远程执行 | 明确标 unavailable/descriptor-only；远端执行未实现前不计入 ready extension |
| gRPC Extension | 返回 `ErrUnsupportedRPC` | 明确 unsupported，不做隐式回退 |
| Extension commands/providers | 注册 API 存在但没有公开消费出口 | dormant/unavailable，或单列实现+端到端消费任务；不可列为已接入 |
| Hook pass/block | pass 可继续，任何非 pass 当前都 generic halt | 仅 pass/block 按上文正式支持；terminate/replace 明确 unsupported |
| Skill load-all deferred tool | CLI 默认接线 | 限额、参数、prompt escaping 验收后支持；工具名单当前不是授权边界 |
| Skill Match/progressive disclosure | Registry API 可用但默认 assembly 未调用 | dormant/library-only，或另立有真实 Agent selection 的迭代 |
| Default config provider | placeholder，Load 报未实现 | 标 unavailable 或移除 API；不能作为可用配置扩展能力宣传 |
| JSONL/stdout tracing | 可由默认配置 builder 创建 | 支持，统一 builder/runtime owner 后验证 |
| OTLP/Kafka 名称对应协议 | 当前 payload/frame 不是对应标准协议 | unavailable，完成外部 conformance 再启用 |

## 文件范围与迭代拆分

**R7-BLD-01（extension 与 skill 行为）**：

- `internal/extension/manager.go`、`plugin_manager.go`：per-instance 生命周期状态、并发 reserve/wait、防重复 Shutdown、错误聚合和 staging commit。
- `internal/extension/registry.go`：registration-order snapshots、副本、同名冲突/提交；`types.go` / `bridge.go` / `internal/cli/extension_bridge.go`：action error、observer snapshot、EventBridge drop/drain 统计。
- `internal/cli/assemble.go`：真实 load/init/diagnostics/cleanup 接线，extension degraded 仍可见；`extension/plugin.go`：remote descriptor only 状态清晰。
- 在已有 `internal/core/run_slot.go` 增加与 `ClaimRun` 线性化的窄 close-admission fence；复用共享 guard，不改 `RunClaim`、RunOwner、status 或 worker 收尾语义。
- `internal/skill/skill_loader.go`、`skill.go`、`skill_adapter.go`、`skill_registry.go`、`internal/core/system_prompt.go`：边界和参数合同；只消费 R6 frozen project-source bytes/manifest；通过 R4 prepared schema-before-approval seam；若暂不接 registry Match，应将其支持状态作为文档/API decision，而非半接线。
- `internal/extension/types.go`：导出 `HookActionBlock`。测试必须放外部 `extension_test` package 以验证真实公开 API。
- 在当前测试基础上新增 execution-tasks 指定测试名及小型真实插件从 load→Init→工具或 hook→assembly close 的完整回归。避免只对 manager 单元 stub 测试。

**R7-BLD-02（trace runtime + 可见递送）**：

- `cmd/cli/main.go`、`internal/cli/prompt.go`、`repl_session.go`、`serve_cmd.go`：root invocation 单 exporter/report 收口；将每个默认 caller 的 assembly/trace close errors 返回到命令结果，非零 exit 可观察。`internal/cli/assemble.go` 的 tracing hunk 只能在 BLD-01 先提交 assembly 生命周期后串行集成，不能并行覆盖该文件。
- `internal/cli/assemble.go`、Assembly options/types：注入同一 tracer/exporter owner，删掉第二 builder/关闭者，清理资源失败可见。
- `internal/tracing/span.go`、`async.go`、`exporter.go`、`exporter_otlp.go`、`exporter_kafka.go`、`load.go`：parent identity/status/delivery contracts；未达 conformance 的 exporter 不进入默认配置。
- `internal/config/loader.go` / `validator.go`：enabled/exporter 单一支持表，none 真关闭，unknown 拒绝。
- R0 对敏感字段序列化负责；R7 负责在已修复字段经过 unified runtime 的端到端导出链时仍有 redaction sentinel 回归。APR audit choice、MEM stats 等若与 R6/R0 同时改动，先复用其 canonical semantics；只修 R7 当前 owner 的真实 gap，不复制另一套状态源。

## 验收清单与证据界限

实施后按 `execution-tasks.json` 中原测试名运行，并补以下生产装配验收：

1. `TestExtensionOrderStable`、`TestExtensionInitFailurePropagates`、`TestExtensionConcurrentInitAndShutdownSafe`、`TestHookEventDataIsImmutable`、`TestHookActionBlockExternalPackage`：验证注册顺序、init 错误可查询、同实例 Init/Shutdown 并发单次、failed init staging 不残留、hook 改副本不改 Agent 原值；外部 package fixture 编译并返回公开 `HookActionBlock`，before-run 真阻止执行，after-run 不回滚既成结果。
2. `TestSkillLoaderHonorsLimitsAndCancel`、`TestSkillMetadataCannotInjectXML`、`TestSkillArgumentsValidated`、`TestSkillFrozenSnapshotBytesUsedForLoadAndRegistration`、`TestChangedProjectSkillMembershipRequiresRetrust`、`TestSkillArgsFailBeforeApprovalAndDispatch`：包含单文件/总 root/file count 超限，扫描中 cancel，文件 symlink escape、复杂/非法 YAML、required/type/enum/unknown key 和 prompt 属性 XML payload；改变冻结之后的 skill bytes/目录成员时，解析与注册仍只使用 R6 snapshot 或 fail closed；无效参数必须在 approval callback 前拒绝且无 dispatch。
3. `TestUnsupportedRemoteExtensionRejected`：HTTP descriptor/gRPC 在实际 config assembly 上不能错误显示 executable/ready。
4. `TestEventBridgeCancelAfterReadReportsOrDrainsEvent`：填满 64 项 out，再安排 source 已读的下一条、取消 ctx，检查有界退出和唯一计数；分别测试正常关闭时 FIFO。
5. `TestTraceUsesSingleParentChain`、`TestTraceNoneWritesNothing`：走真实 `cmd/cli` → `AssembleAgent`，断言一条 TraceID/parent chain；none 和 disabled 生成零 sink 文件、零 stdout trace、无 async worker；未知 exporter 是启动错误。
6. `TestTraceErrorSpanStatus`、`TestTraceShutdownDoesNotAcceptOrphanedSpan`、`TestTraceFailedBatchReported`、`TestUncooperativeExtensionShutdownReturnsCleanupPending`、`TestUncooperativeExporterCloseRetainsSinkUntilProducerStops`、`TestDefaultCloseCallersPropagateErrors`：失败 span ERROR；关闭后拒绝；注入 exporter fail 和 ctx deadline 后，通过唯一 `ShutdownReport` 确认计数/错误可观察。被测 extension/exporter 忽略 context 时 caller 有界返回但 owner/sink 保留；释放阻塞后同一 owner 完成一次 cleanup。prompt、REPL、serve 和 root CLI 的 close failure 均影响返回错误/exit status。
7. R0 sentinel 测试作为 R7 合入前置；验证 trace + slog 两个 sink 的最终字节，不要把只检查 `SpanData.Attributes` 的单测称作端到端隐私验证。
8. `TestTelemetryLabelsAreInjective` 必须把旧分隔符冲突输入 `{"a":"x,b=y"}` 与 `{"a":"x","b":"y"}` 记录进 `DefaultTelemetry` 并从 `Snapshot()` 证明 key distinct 且数值没有合并；另测 `TestTelemetrySeriesCapAndUnknownToolLabelFold` 覆盖固定 producer label domains、未知 tool → 该维度 `other`、`DefaultMaxSeries` 总上限及无标签 drop counters。`TestAuditHashAdvancesOnlyAfterAppend`、`TestApprovalAuditRecordsFinalChoice`、`TestUnavailableStatsNotReportedAsZero` 按 R7/R6 当前责任边界执行；磁盘 append 失败和实际最终用户 approval 选择都需从主写入路径注入验证。
9. 标准 OTLP collector 与 Kafka broker 的互通验收未执行前，support table 继续标 unavailable；本仓 fake HTTP/TCP server 测试只可标“本地 framing 单测”。PTY/真实交互验收也必须实际运行后才能标 pass。

当前这份设计是源码核对与既有审计结论，不是执行证据。R7-DES-01 还依赖 execution ledger 中的 R1-REV-01、R4-REV-01、R6-REV-01；合并实现前需由 reviewer 对齐这些已批准的隐私、session/runtime 和 audit/status 契约。`R7-REV-01` 应以默认 CLI assembly、错误注入、并发/race 测试、可读取导出样本和真实支持矩阵完成评审；不能仅凭 API 存在或单测通过宣称可交付。
