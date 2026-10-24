# 动态能力运行时：深模块设计

日期：2026-10-22。输入为 10/18 architecture、iteration-plan、已复审 PoC 与当前源码。按用户本轮指令先完成全功能生产实现，之后统一测试；不修改原 infra 任务/里程碑、Makefile、lint 或 CI。

## 选定 seam 与两种 Interface

方案 A 是按资源类型暴露 register/stage/publish/retire、hook manager、skill index、MCP connection 的控制方法。调用者必须协调锁、revision、关闭顺序和授权，Interface 近似 Implementation，Depth 较低，拒绝。

方案 B 在 `internal/capability` 放置单一深 Module：`Manager[T].Reload/Acquire/Close`。Reload 的 builder 取得候选 Scope，返回不可变 Snapshot 和组合根私有的冻结 payload；Acquire 返回 run lease。Module 隐藏候选单飞、版本检查、失败保旧、最多两代、invocation ref、撤权取消与完成 fence、逆序关闭。选用 B。Snapshot accessor 只返回深拷贝；payload 是组合根内部 seam，发布后不可修改，不能把它作为外部 SDK DTO 公开。

依赖分层：DAG/digest/状态机属于 in-process；本地文件 loader、MCP transport/owner、进程 Runner 是内部 adapters；真正外部模型/MCP 使用已有 mock adapters 验证。`capability` 只依赖标准库；core 依赖消费方最小端口；公开 SDK 不依赖 internal。无需额外 runtimecontract 包。

## 冻结合同

```go
type RunGenerationProvider interface {
    AcquireRun(context.Context) (context.Context, RunGenerationLease, error)
}
type RunGenerationLease interface { Release() }

// package capability (generic payload belongs to the composition root)
func NewManager[T any]() *Manager[T]
func (*Manager[T]) Reload(context.Context,
    func(context.Context, *Scope) (Snapshot, T, error)) error
func (*Manager[T]) Acquire(context.Context) (*Lease[T], error)
func (*Manager[T]) Close(context.Context) error
func (*Manager[T]) Revoke() uint64
```

`Lease.Snapshot/Value/Release/BeginCall`、`Invocation.Context/Complete/Release` 提供 run 与子调用独立 ownership。parent release 禁止新 invocation；已有 invocation 继续保活，Complete 重读当前撤权 epoch。撤权后的外部副作用结果为 unknown，不谎称回滚。`Scope.Defer(id, finalizer)` 登记资源，Close 逆序关闭、捕获 panic、聚合错误；重入返回 CloseInProgress，WaitClosed 单独等待。

Snapshot 输入包含 source 的捕获字节/digest、stable node ID/owner/requires/provides/permissions、phase/priority、prompt、policy/trust epoch。对 source/node/依赖排序后编码并 SHA-256；缺失 provider、冲突、环在 builder setup 前验证。mtime 与 size 不作为内容标识。版本化摘要不记录秘密；配置秘密使用非公开 handle/version。

## 生产接线

- `AssembleAgent` 保留现有 pipeline；extension loader 使用已有 `assembleConfig.pluginLoader` seam。extension bootstrap 必须在 `assembleModel` 前建立 provider，command bootstrap 必须在 CLI route resolution 前运行；不能仅增加 manager getter。
- `internal/cli/dynamic_capabilities.go` 维护私有冻结 runtimePlan，并实现 AcquireRun。该 adapter 把选中的 Snapshot 和 payload 放入返回 context。Harness 使用 `WithRunGenerationProvider`，TurnRunner 使用 setter，两个入口相同准入。
- `Harness.Submit`/`EinoTurnRunner.RunTurn` 在 RunSlotGuard 已准入、callback 副作用前 acquire。runOwner 持有 lease；`supervise` 在 worker、resource、callback、durable late completion 都 join 后关闭 commit lease，再 release generation，再释放 slot。调用者超时不释放代。
- Skill catalog 来自原 loader/report、默认/用户与 verified project bytes；List/Match 实际进入 CLI prompt/tool projection。标准 SKILL.md 先发布 metadata，完整正文按调用加载，不能重读未验证项目文件。
- MCP 继续使用 `mcp_generation.go`、`assemblyMCPGeneration`、`MCPClientGeneration` 和 deferred registry；catalog/延迟 dial 复用现有 source trust、prepared dispatch 与 lease，不建立第二种 transport。新代不得把旧 run 的 registry 动态变成另一代。
- SDK adapter 将公开 DTO 转为现有 extension registry；JSONL RPC 是受界限的等价 versioned wire transport，不引入 gRPC 依赖。启动清空默认环境、校验 artifact digest、handshake 与协商能力、限制 frame/inflight/deadline；scope 拥有进程关闭。进程是故障隔离，OS 文件/网络/RSS sandbox 不在本轮承诺。

## 状态与资源不变量

稳定状态 current；构建状态 current+candidate；发布后 current+retiring。retiring 有活引用或 cleanup 未完成时后续 reload 只更新最新 revision 并等待，不启动第三代。builder 不持 manager 锁；publish 重查 desired revision 与 revoke epoch；stale/失败 candidate 先 cleanup 再退回旧代。Close 禁止新 builder/publish、取消合作 builder，等待其真实退出和 Scope cleanup；deadline 只结束等待，不能声称强杀 Go callback。

长寿 SessionStore/MemoryStore 属于 Assembly owner，不随代关闭。独占端口/文件锁等候选不能双开时返回 restart-required。权限继续以现有 prepared policy/trust fence 为事实来源，generation epoch 只补充撤权 fence，不取代现有审批。

## 文件所有权与验收

runtime builder：`internal/capability/**`、`pkg/extensionapi/v1/**`、`internal/core/run_generation.go` 与 Harness/TurnRunner/runOwner 接线、extension SDK/RPC 新文件。assembly builder：assemble/skill/MCP/config/CLI command 生产投影。profile builder：TUI/headless/profile、bench fixtures、plugin_stub parity。共享文件修改前协调，不修改其他工作树。

验收面使用上述 Interface：旧 run pin、候选失败保旧、stale revision、双代上限、invocation 独立引用、不可变 Snapshot、panic/reentrant cleanup、Close 等候 builder、撤权后迟到结果 fence。另有真实 CLI Skill/MCP/extension command/provider consumers、外部 SDK 独立编译、RPC frame/deadline/env/退出，以及完整 make verify。按用户顺序，生产功能完成后再写/跑相应测试与基准；任何未完成项如实记录。

## 实现中的 generation 与 request exposure

生产接线区分两种版本。generation 固定配置、捕获来源、权限/信任身份和资源 owner；runOwner 持有其 lease。`catalogRevision` 仅表示同一 generation 中逐步披露的工具集合。首次激活从未发布过的 MCP source，且所有工具名没有全局冲突时，`BeginMCPGenerationAddition` 在 dispatch fence 与 registry 锁下原子追加 descriptor、reservation 和后续首次 materialization；它只提升 exposure Version，保持已有注册的 executor/schema/authority 不变。替换、移除、撤权、重授予或同一 source 再次发布仍提升原 Registry authority fence；这个例外不扩展到任意 Register 操作。

LoopAgent 在两个模型请求之间检查 exposure Version，从同一 pinned registry 读取一份 definitions，并据此更新下一请求的 schema 和 prompt。当前请求实际发出的工具名集合是 dispatch membership；响应中出现没有披露的名字，会在任何工具执行前拒绝。因此 `mcp_discover` 激活后可在本 run 的下一次模型请求使用新工具，而不能在同一个模型响应里夹带调用尚未披露的新工具。已有 prepared sibling calls 不因纯追加而失效。

`/reload` 在 idle run seam 构建新的数据计划：标准 Skill catalog、经验证 shell hooks、prompt 配置与相关来源 identity。它真实更新后续 run 的 hook chain、pre-tool interceptor 与 prompt options；失败保留旧计划。当前 CLI 对代码/native plugin/provider/command 或不能双开的进程级配置变化返回 `restart-required`。这些资源仍归原 Assembly owner，普通 session/store 不被移入 generation Scope。通用 Manager 的独立 candidate/current/retiring 生命周期已实现，但不能把 CLI 这轮的数据 cutover 写成任意代码热卸载。

named MCP refresh 先构建连接与 frozen candidate descriptor，以只读 `PreviewDefinition` 做最终 schema/prompt 预算和 Snapshot 验证，再持 DTR transaction 进入内部 host seam `Manager.ReloadWithCommit`。manager 在最终锁内重读取消、revision、epoch 后调用有界内存 commit，`PublishMaterialized` 同时发布 schema、executor、dispatch lease 和 owner；失败 Abort 并保留当前 source。提交成功后没有 deferred Load 或其他可失败步骤。commit 不进入公开 SDK，禁止用户代码、I/O、等待、manager 重入以及返回错误前部分提交。锁序固定为 DTR → manager → owner → dispatch fence → registry，Retire 仅改变 lease 状态，实际 Disconnect 在 cleanup goroutine 等待旧 lease 后执行。

MCP connect 请求与进程存活分离：连接/握手受请求 deadline 约束，成功后的 stdio/HTTP owner 不继承 errgroup 或一次 run 的取消。SDK RPC 同样由独立进程 owner 持有；调用返回前解除 deadline cancellation callback，Shutdown/transport failure/调用超时才停止进程。SDK v1 provider 仅协商 complete-response，Stream adapter 发出单一 final chunk，未声称具备 token streaming。RPC executable 先流式捕获并校验 SHA-256，执行该捕获文件，进程退出后清理 owner 临时目录；上限 128 MiB。

## 三层审计与扩展依赖的实际消费

本轮审计区分 `configGeneration`、`catalogRevision` 和 `materializedArtifactDigest`。第一层记录已捕获配置/来源/资源计划，第二层记录同代 request 可见工具的追加披露，第三层记录按需读取的指令正文。用户目录 Skill 尚未读取的正文不被 metadata digest 冒充固定：`skill_load` 完整正文首次 materialize 后缓存，并返回 `skill_content_digest`；core 只识别这一工具的 64 位十六进制白名单字段，传到实际 AgentMessage/tool receipt 与 durable SessionEntry。此字段是指令制品的审计事实，不提升来源信任或执行权限，不转发到模型协议中，也不保存任意工具 metadata。

公开 Manifest 的 `requires/provides` 由实际 SDK adapter 提供；PluginManager 在 Load 后暴露 detached Manifest，CLI 在 Init 前校验缺失、冲突和循环，并将同一声明纳入 generation source/node identity。PluginManager 依据拓扑顺序初始化声明依赖的扩展，前序初始化失败时不再初始化依赖者，关闭逆序运行。legacy native extension 未声明 Manifest 时保留相互原顺序。此依赖只表示能力可用性与初始化顺序，不承诺任意跨扩展 host service invocation。
