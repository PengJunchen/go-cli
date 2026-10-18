# OpenCode v2 源码研究：可移植的生命周期与扩展契约

研究日期：2026-10-18。源码基线固定为官方仓库 `anomalyco/opencode` 的 `v2` 分支提交 `4617210822bdbb31e5749751afbfb4584ccedac8`。本报告的源码链接均固定到该 SHA；源码克隆位于 `/tmp/go-cli-architecture-20261018.QZFu4f/opencode`，不属于 go-cli 仓库。

## 结论先行

OpenCode v2 值得 go-cli 学的是若干清晰的生命周期边界，而不是把 Effect 服务图整体翻译成 Go：

1. **按资源寿命分图并缓存。** `LayerNode` 把服务实现、依赖和 `global/location` 生命周期标签显式建模；每个 location 只构建一次服务图，图及其中的 watcher、MCP 连接等由对应 Scope 关闭。这能控制重复初始化和资源所有权，但无限 TTL 也意味着访问过的 location 会持续占用缓存，go-cli 应增加显式容量/关闭策略。
2. **扩展注册由可关闭的代际作用域管理。** 插件注册、hook 和转换器随插件 Scope 一起释放；源文件加载失败会保留旧版本。新插件 setup 失败时则是在旧 Scope 关闭后重新激活旧定义，属于有限恢复，不是全局原子事务。源码没有提供通用的在途工具租约、generation drain 或不可中断清理保证，Go 设计必须明确补齐。
3. **模型看到的工具定义与实际执行器来自同一请求快照。** 这是防止“模型调用了本次请求未公布的工具”的有价值契约。快照并不固定插件 Scope，也没有公开的持久 catalog revision，因此不能将它误读为热更新期间的执行租约。
4. **权限在执行边界再次评估。** 列表过滤主要用于可见性；敏感工具仍由叶子执行器调用 `Permission.assert`。这比只靠 prompt 或工具列表可靠。不过授权与实际副作用之间仍可能存在撤权 TOCTOU；go-cli 应规定调用授权时点、长任务撤销语义和迟到结果的提交栅栏。
5. **会话读模型和事件日志是两种持久化机制。** SQLite 中的 session/message/inbox 投影持续存在，原始事件表可配置；内存 PubSub 不可重放。执行 claim 与恢复预算提供崩溃恢复线索，不构成分布式锁或 exactly-once。

总体判断：可借鉴“显式 owner、不可变请求快照、候选状态与生效状态区分、事件/投影分工、权限落在副作用边界”的设计。不要照搬 Effect 的类型级 DI、Layer 编译器、Fiber/Context/Service 全家桶，也不要承诺 Go goroutine 可被强制终止。

## 版本身份、证据范围

截至 2026-10-18，直接从官方 Git remote 读取到：`refs/heads/v2` 指向本报告固定提交 `461721...`；`refs/heads/dev` 指向不同的 `60a5a6ca7ce176b04cd2321ffd1819b1ea8e6006`；官方 tag `v2.0.32` 指向 `843a6c514e606b436a7fe713422ace0a93ad3d61`。GitHub 的固定提交页显示 `4617210` 为刷新 bundled models.dev snapshot 的提交；固定提交的 `packages/cli/package.json` 声明版本 `2.0.26`。因此，用户钉住的 SHA 是当时官方 `v2` 分支头，但不是当时最新发行 tag。这里的 `v2` 指上游分支/发行线，不能和 SDK 路径、协议版本或 `dev` 混称。[固定提交](https://github.com/anomalyco/opencode/commit/4617210822bdbb31e5749751afbfb4584ccedac8) · [固定提交 CLI manifest](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/cli/package.json#L3-L4) · [v2.0.32 release](https://github.com/anomalyco/opencode/releases/tag/v2.0.32)

本次浏览了官方 GitHub 的固定提交、其源码文件和发行页，并在官方仓库 clone 中检查实现及测试定义。**没有安装 Bun/Node 依赖，没有运行上游测试、类型检查、构建或性能基准。**以下“测试覆盖”仅描述仓库中存在的测试用例；不代表用例本轮通过。源码结论仅对 pinned SHA 有效，未将 `dev` 实现外推到 v2。

## 运行时图：LayerNode、Location 与 Scope

`packages/util/src/effect/layer-node.ts` 将节点定义为 Layer 实现、依赖节点、服务名/身份及生命周期 tag。`group` 保持有序合成，只向外暴露根节点服务；替换在最终 acquire 前先解析。TypeScript 类型层检查 Layer 所需依赖是否由声明依赖提供、替换是否覆盖输出和错误、替换 node 是否属于兼容 tag、替换 Layer 是否闭合。运行时 `compile` 另检查可达环、未绑定节点、跨 tag 替换及同一 Layer 实例被不同依赖线接入等错误。不能把它概括成“所有图或 Scope 错误都有运行时校验”。[LayerNode 构造/类型约束](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/util/src/effect/layer-node.ts#L69-L169) · [运行时解析、去重与编译](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/util/src/effect/layer-node.ts#L218-L329)

`app-node.ts` 仅声明两档：location 可依赖 global，global 不依赖 location。`Instance.graph` 把 Core 服务按依赖组成图；`Instance.layer(ref)` 的替换顺序是默认实现、调用者替换、最后绑定当前 Location 和插件选项。`LayerNode.compile` 对 global tag 子图复用父 memo map，对其余部分使用每次 build 新建的局部 memo map。它是按作用域共享构建结果，不是运行时 service locator，也不是自动把任意对象提升到全局单例。[tag 关系](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/util/src/effect/app-node.ts#L1-L14) · [Core 图与替换优先级](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/instance.ts#L59-L180)

`LocationServiceMap` 以 canonicalized directory/workspace ref 为 key，按需 build location graph，并对健康图设置无限 TTL。build 失败时先移除失败映射，以允许下一次重新构建。显式 invalidate 先从 map 脱离，随后 shutdown 生命周期；若 build 在脱离期间完成，也会关闭新图。注释允许已借出旧图的 session step 运行至 handoff boundary。全量 reload 会 invalidate 已知 refs 并重建，但这不同于普通配置 reload；后者触发受影响服务自行更新。无限 TTL 的代价是打开过的 locations 长期留在内存，产品侧需定义 location 关闭或上限策略。[LocationServiceMap 构建与关闭](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/location-services.ts#L12-L76) · [显式全量 reload](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/location-service-map.ts#L18-L43)

### 对 Go 的适配

可用普通构造函数加显式 `Dependencies`/`Owner` 表达图，不必复刻 Effect Layer。全局依赖和 location 依赖可由两个 builder/接口层区分；图启动前做 DAG、缺失依赖、替换目标和作用域可达性检查。Scope 可实现为 owner 状态机：注册 finalizer、幂等 Close、逆序清理、错误聚合、父子取消传播、partial setup 回滚。所有后台 goroutine 必须有 context、退出条件及可测试 join 路径。若清理期限结束，记录“仍在运行/状态未知”，不能伪报已关闭。

## 插件加载与热更新语义

supervisor 的处理顺序是：把内建插件、宿主 SDK 插件、实例插件和已解析的用户插件按优先级组合；监听配置、模块变化和更新状态；用 `Queue.sliding(1)` 合并事件，约 100ms debounce 后由单一消费者串行激活。hold/awaitActivation 先把待处理状态告知调用者，减少冷启动读取到半初始化插件注册表的概率。supervisor 中的数字 `generation` 只用于忽略过期的异步 update-check 结果；真正的 `Plugin.Generation` 是插件定义、source/revision 和独立 child Scope，二者不可混为一谈。[supervisor 触发与序列化](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/plugin/supervisor.ts#L124-L268) · [Plugin generation 接口](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/plugin/service.ts)

Registry 保留未变化的共同前缀。变化时按逆序关闭旧后缀 Scope，再构建新后缀。失败语义需要区分：

| 失败点 | pinned v2 行为 | 重要边界 |
|---|---|---|
| 本地 source 导入/解析失败 | 新 generation 尚未形成，保留现有运行版本并记录错误。 | source manager 会缓存相同 fingerprint 的失败 Promise，避免无关 watcher 事件重复触发模块顶层副作用；内容变化后才重试。 |
| 新定义 revision 未变化但 setup 曾失败 | 缓存失败结果，不在无关刷新时重复 setup。 | 必须出现新 revision 才重新尝试。 |
| 新定义已导入，setup 失败 | 失败代记录错误；若旧插件原先健康，则用新 Scope 重新激活旧 definition 做 fallback。 | 旧 Scope 已经关闭；旧 setup 副作用只能由其 cleanup/finalizer 撤销，无法由宿主推导任意副作用的事务回滚。恢复 setup 也可能失败，届时停用该插件。 |
| 已激活插件 callback 后续抛错 | 关联 State group 的 registrations 被撤销，失败记录上报，关闭插件 Scope；清理可异步进行。 | setup、状态刷新和清理之间有额外调度；awaitActivation 不等同于所有异步 finalizer 已完成。 |
| 配置移除/策略禁用 | 插件从候选序列移除，旧 Scope 按序关闭。 | 源码没有声明“所有由该插件启动的任意 goroutine 都已结束”的通用强保证。 |

上表由 supervisor、Plugin registry、State group/failure 处理及相关测试共同支持。[Registry activation/fallback](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/plugin/service.ts) · [插件失败清理](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/state.ts) · [reload 测试](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/test/plugin/supervisor-reload.test.ts) · [registry 测试](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/test/plugin.test.ts) · [failure 测试](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/test/plugin-failure.test.ts)

**核心缺口/Go 决策：**没有看到 generation lease/borrower counter 来线性化“新 lease acquire”与“旧代 publish/retire”的竞争，也没有插件 scope 被 Tool snapshot 延长存活期的契约。这不是证明每个正在执行的旧工具必然崩溃；它只说明源码和测试没有给出通用保活、取消并 join 的保证。Go 中 goroutine 不能被宿主强杀。进程内插件必须合作式遵循 Context；若要强制回收不合作插件，边界应是可 kill 的子进程。

Go 代际发布建议采用明确状态机：构造候选 → 校验/预连 → 原子发布 `(revision, snapshot)` → 禁止旧代新租约 → 等待旧租约归零或将取消信号传播至 deadline → 逆依赖清理。Acquire 与 publish 必须有共同的线性化点（例如 mutex 下 `active` 指针与 refcount 操作，或一个 generation owner 锁）；只做 `atomic.Pointer` swap 而不协调引用计数会漏掉“读取旧指针后尚未增 ref”的 race。若新旧资源不能并存，应标注为 stop-then-start：这类资源无法同时满足无停机、旧代持续服务、setup 失败回滚三个目标，须给出明确降级策略。

## Tool 快照与“单一能力目录”边界

OpenCode 的公共 Promise/Effect 插件契约适配到同一个 Core 运行时；`@opencode/schema` 和 `@opencode/protocol` 共享 API DTO/schema，并组合 client/server 请求面。这是“跨包共享协议结构”的一手证据，不是“所有扩展能力共用一个带 revision 的目录”的证据。[插件包 exports](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/plugin/package.json) · [Schema 包](https://github.com/anomalyco/opencode/tree/4617210822bdbb31e5749751afbfb4584ccedac8/packages/schema/src) · [Protocol API](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/protocol/src/api.ts#L200-L246)

Core `Tool` 是 location 服务，维护工具定义和 namespace。构造 snapshot 时按当前 permission rules 过滤隐藏/禁用能力，分成直接 tools 和 CodeMode tools，生成 model-visible definitions 与 executor closure。executor 只接受当前请求实际公布的 definition，拒绝调用未公布工具。CodeMode 的大型 namespace catalog 可复用，但缓存 key 是内部 State 的 data identity 和可见工具名集合；`State` 有内部 revision，Tool 公共接口没有通用 revision token。每个请求的 definitions/executors 是新建的，catalog 可复用；这属于内部性能缓存，不应称“公共 ToolCatalogRevision”。[Tool snapshot](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/tool.ts#L41-L65) · [快照与 catalog 缓存](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/tool.ts#L225-L270) · [registry snapshot test](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/test/tool-registry.test.ts)

这给 go-cli 的功能投影审计一个直接启发：注册、可查询、出现在 SDK/schema、进入默认 CLI、被 prompt/model 揭示、执行时仍可解析，是六个不同可观察状态。每个扩展都要有 consumer 路径和测试；“注册表接受了定义”不能单独算成端到端可用。推荐把公开能力 schema 和运行时 catalog 建立稳定 ID + revision + provenance；一个请求持有同一不可变快照里的 schema、权限过滤结果和 executor selector。若向外暴露注册/发现 API，再测试默认 CLI 与 SDK 的实际投影，不借用 OpenCode 的包结构来推断 go-cli 的行为。

## Hooks、Skills、MCP、配置和权限

**Hooks。** `PluginHooks` 按 aisdk/session/permission/shell/tool 分域，回调注册绑定 Scope，按注册次序触发，provider-specific hook 可按 provider ID 筛选。只有 tool execute.before 明确有工具失败通道；其他 hook 类型未声明等价失败合同。它们是同进程受信任扩展点，不构成沙箱。Go 侧要为每个 hook 写清同步/异步、超时、panic/error、顺序和撤销语义，避免把所有 hook 放进无界 fan-out。[hook domains 与 scope registration](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/plugin/hooks.ts)

**Skills。** Skill registry 负责可发现项，默认不把全部正文拼入系统提示；仅在有描述且允许自动调用时生成精简指引，实际按需通过 Skill 工具取正文和文件。读取/列表会应用 skill deny 过滤。远程 discovery 对路径分段、URL/origin 做校验，限制并发，使用 staging + 备份 rename 更新目录，以避免部分下载覆盖可用版本。可迁移的是渐进暴露、内容/元数据分离、路径安全及“先暂存再替换”；不要每次 turn 无条件扫描并全文载入全部 skill。[Skill API 与 permission filtering](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/skill.ts) · [远程 skill discovery](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/skill/discovery.ts)

**MCP。** 每个 server entry 自己有状态、startup latch、client 与子 Scope；启动连接、发现 tools/prompts 后才标记 connected。单 server 的 reconcile/生命周期有锁；远端连接路径会做额外串行控制。失败重试、过期 session retry、断开时关闭 scope 与刷新注册表都有专门处理。异步启动不阻塞 location graph 完成，调用者等待 server readiness 后得到明确 unavailable 错误。MCP wrapper 执行时做 permission assert。MCP instructions 只有在当前 agent/session 有实际可执行 MCP tool 时才加入 prompt。[MCP server lifecycle](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/mcp/index.ts) · [MCP tool adapter](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/tool/mcp.ts) · [MCP prompt instructions](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/mcp/instructions.ts)

**Config / prompt。** config discovery 按 well-known、global、explicit、direct project、project supplementary、content 顺序产出 entries；JSON/JSONC 解析后做变量替换、规范化和 schema decode。不同业务域自行定义覆盖/合并，不是一个万能 deep merge。watcher 事件滑动合并并 debounce；reload 串行，成功发现新 entries 后发 config.updated，由 provider/MCP/skill 等插件各自 reconcile。坏 well-known refresh 保留最近成功值；普通 JSON/JSONC 文档解析失败会被拒绝并记录诊断，不能据此推断每种配置源都有 last-known-good 保留。读取有效配置不自动重建所有 location services。Instructions 收集来源，按 canonical JSON/hash 做差异；会话将运行中的增量作为事件记录，因此跨重启读取时不依赖旧 location source 仍可访问。[Config load/reload](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/config.ts#L89-L330) · [Instruction sources](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/instruction-discovery.ts) · [Instruction state](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/session/instruction-state.ts)

**权限。** 规则集来自 Agent、Session 以及 saved allow；evaluate 按资源匹配，最终匹配项决定 allow/deny/ask，默认 ask。配置中显式 denied 先于可保存的 allow。permission hook 能再调整 ask/allow/deny。ask 发事件并等待用户 reply；拒绝作为特定错误穿过叶子工具通用错误归一化；带反馈拒绝则变成可供模型继续处理的 corrected error。关闭 permission service 会拒绝 pending prompts。重要的是工具目录过滤不是授权替代品：shell、文件修改、MCP 等敏感叶子必须在执行时 `assert`。撤权 TOCTOU 仍需由宿主策略定义：快速副作用可在调用开始时授权；长时/分阶段副作用可在阶段边界重验；写回 session 的结果应验证 invocation 与当前 generation fence。[Permission evaluation/assert/reply](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/permission.ts#L140-L340) · [managed policy plugin](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/config/plugin/policy.ts)

## Session、事件与持久化

Bus 区分 durable / ephemeral typed event。durable publish 预留 aggregate sequence，在事务里运行 projector/commit hook；只有 server `events.persist` 开启时，原始事件行才写入 event 表。session、message、pending/inbox 等 read model 则由 SQLite/Drizzle 表维护，不随 raw event persistence 开关消失。内存 PubSub 是易失流，不支持断线后可靠续读；需要 replay/follow 的消费者走 durable log 和 sequence watermark。[Bus publish/persistence](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/bus.ts#L139-L180) · [event transaction / persist paths](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/bus.ts#L330-L430) · [event schema](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/schema/src/event.ts)

SessionStore 的 claim 在 durable execution-start 事件的事务路径更新；terminal event 释放 claim，进程 shutdown 中断会留下待恢复状态，启动时按次数预算恢复。SessionRunCoordinator 按 session key 串行、不同 session 并行，wake 是合并通知而非保存多个执行命令。inbox enqueue 与 delivery 先后写入 durable event/projection。这个组合支持崩溃后恢复工作意图，但若进程在外部副作用已发生而完成事件未提交时崩溃，重试可能重复副作用；不是 exactly-once，也不是分布式多宿主锁。[SessionStore claim](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/session/store.ts#L53-L88) · [claim transaction](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/session/store.ts#L190-L256) · [run coordinator](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/session/run-coordinator.ts) · [execution recovery tests](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/test/session-execution.test.ts)

对 Go 的实用边界是：SQLite 写事务保证 session claim / message/event projection 的原子可见性；调用外部系统的副作用需要幂等 key、intent/receipt、outbox 或业务 reconciliation。不要把进程内 mutex、in-memory bus 和 DB transaction 混称为同一耐久保证。

## 故障矩阵与 Go 侧最小可验证契约

| 故障/竞态 | 源码证据得出的边界 | Go 验收契约 |
|---|---|---|
| Layer build 失败 | location 失败项驱逐，可由后续请求重建；已成功图长时间缓存。 | 部分构建发生的资源必须由 candidate owner 逆序清理；失败不会污染正在服务的旧图。 |
| config 格式错误 | 规范化/decode 阶段拒绝错误配置；热刷新保留此前成功状态。 | parse/validate 再发布；bad candidate 保留 last-known-good，并向用户报告来源、字段和时间。 |
| plugin import 错误 | 保留已有运行版本；相同内容错误不会被无关事件反复执行。 | 错误状态关联 fingerprint/revision；内容变化可重试，完全相同失败按策略冷却。 |
| plugin setup error | 先关旧变更后缀，再尝试新；旧 definition fallback 可能恢复，但不是 rollback。 | 可回滚候选只能在 side effects 被 owner 管理时成立；otherwise 明示 stop/start gap；setup 中途注册必须自动随 candidate scope 释放。 |
| lease acquire 与 publish 同时发生 | OpenCode 没有找到通用 tool lease contract。 | 写并发测试覆盖读 active → 尚未 ref++ → swap/retire 的三步竞态；不能只测 atomic swap。 |
| 长任务不听取消 | Scope close 的结果依赖 Effect fiber/插件 cleanup；Go goroutine 不可强制 kill。 | Context 传播、关闭 deadline、关闭结果 unknown 状态；若需强回收走子进程。 |
| exclusive resource 新旧代冲突 | OpenCode 的后缀先关再装；回退可能重建，但存在服务空窗。 | 测量/规定 outage 和回滚边界；独占 listener/db lock 不允许假设 zero-downtime overlap。 |
| catalog 变化后旧快照执行 | snapshot 与 executor 可独立存活；没看到自动 Scope 引用。 | 同 revision 的 schema/selector/executor 成对 pin；撤代前拒绝新 lease，旧调用 drain 或显式取消。 |
| 权限在广告后撤销 | 叶子会在执行时重新 assert，但已通过 assert 后的长操作可能继续。 | 定义授权线性化点；撤权 epoch 可选用于阶段重验/提交前检查，迟到结果不能写过新的 fence。 |
| durable event 与外部副作用 | event/session projection 可事务化；外部服务不随本地事务原子提交。 | 幂等/reconcile；恢复测试覆盖副作用已完成、receipt 未落盘的窗口。 |

建议先实现并测试最小接口/行为，而不是先引入完整 runtime：

- `Owner`：`Go(ctx)`/`Close(ctx)` 或等价接口；Close 幂等；finalizer 逆序；构造失败自动回收；关闭超时能表达未退出 goroutine。
- `Generation[T]`：不可变 revision、active/retiring/closed 状态、引用租约；Acquire 和 Publish/Retire 线性化；新代失败保留旧代；旧代归零后清理；setup 资源无法双开时显式进入停服策略。
- `CatalogSnapshot`：revision + 按权限过滤的 public schema + 同 revision executor selector；请求始终使用同一快照；注册表更新不偷偷改变正在运行请求的定义。
- `Policy`：每个敏感 side effect 检查当前规则；run/turn 的身份和 generation fence 随结果向上带回；对“撤权不影响已开始的非中断调用”或“阶段间撤权”作出明确选定。
- `ConfigCandidate`：来源与 digest、解析诊断、有效值；candidate 完整校验后再发布；watch 事件可合并，失败不让旧值消失；业务域按明示规则 reconcile。
- `SessionCommit`：事件序列、projector 与 session 状态在同一 DB transaction；外部 action 用 intent/receipt/idempotency 解决事务外窗口；in-memory fanout 只作通知。

最低测试组：构造失败清理；替换/缺依赖/循环；location key 隔离和关闭；config bad reload 保留旧值；plugin import/setup 两类失败与 fallback；lease acquire/swap race；取消不合作调用后的 bounded close；独占资源不能双开的失败情形；工具 advertise 后撤权；pending ask 收到 close；工具 result generation fence；session 崩溃恢复与外部副作用重复窗口。性能测试另测，不拿这些单测推导吞吐。

## 学习优先级与不应复制的部分

1. **高优先：资源 owner 与 Scope 逆序关闭。** 先把当前依赖图构造顺序和清理责任显式化，减少泄漏/重复初始化；按 global / location / session / run 分寿命，避免拿“全局缓存”代替 owner。
2. **高优先：不可变请求工具快照与一致 schema。** 确保模型定义和 executor 对同一 revision；权限检查落在叶子。补齐当前产品中 registry 到 SDK/API/默认 CLI 的消费路径清单。
3. **高优先：candidate reload 与 last-known-good。** 分清解析/导入失败、setup 失败、发布成功后清理失败；每类失败决定保旧、停用或降级。不要把刷新全部能力图作为普通 config reload 的副作用。
4. **中优先：bounded eventing 和 session durability。** 明确 durable log 与可丢失通知的区分；对慢消费者设队列策略/背压；恢复预算与外部 action idempotency 一起设计。
5. **中优先：渐进式 skills/MCP discovery。** 复用路径/URL 校验、staging 更新、startup latch、server 独立 owner、按需注入 prompt；把远端连接成本限制在具体 MCP entry，不阻塞整体服务启动。
6. **按需：丰富插件市场、自动更新和多 hook 域。** 这些带来攻击面、下载信任、缓存增长和清理难度；先确定扩展 API、审计/权限与进程边界，再开放广泛动态代码加载。

不建议复制：Effect Layer 的大型类型级泛型/错误通道、上下文 service locator、Effect runtime/Fiber 及 TS 特有动态 ESM 热加载方式。Go 可以保留相同的显式依赖、owner、错误链、上下文取消和代际状态机，由普通接口、`context.Context`、`errors.Is/As`、显式锁/原子状态和可测的 owner 实现。对不可信第三方代码用进程协议或经过明确 sandbox 评估的运行时；MCP 本身只规范工具交互，不负责代码隔离和生命周期。

## 校验与证据索引

本轮已检查的上游测试定义包括 LayerNode graph/type tests、plugin reload/failure tests、tool registry snapshot tests、permission tests、session execution/recovery tests；它们被链接在相应章节。**没有执行这些测试或任何 benchmark。**无性能数字、资源占用排名或“热更新无停顿”结论。上述 Go 契约是根据源码事实和 Go 生命周期边界推导的设计建议；尤其 generation lease、撤权 TOCTOU、独占资源 overlap 与 durable side effect 窗口必须由 go-cli 自己的并发测试验证。

官方 pinned source 入口：[`LayerNode`](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/util/src/effect/layer-node.ts) · [`Instance`](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/instance.ts) · [`LocationServiceMap`](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/location-services.ts) · [`Plugin supervisor`](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/plugin/supervisor.ts) · [`Plugin registry`](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/plugin/service.ts) · [`Tool`](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/tool.ts) · [`Permission`](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/permission.ts) · [`Config`](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/config.ts) · [`MCP`](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/mcp/index.ts) · [`Bus`](https://github.com/anomalyco/opencode/blob/4617210822bdbb31e5749751afbfb4584ccedac8/packages/core/src/bus.ts)
