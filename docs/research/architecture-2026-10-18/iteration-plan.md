# 动态能力架构迭代计划

日期：2026-10-18。状态：Designer 方案，供用户故事审核与后续 TDD 拆解。本文不修改 `go-cli-infra/infra/tasks.json`、milestone、规则、Makefile 或 CI；这些文件按仓库要求受保护。生产实现由后续 Builder 按阶段执行，Reviewer 对每项可执行验收逐条确认。

计划按现有 `AssembleAgent`、MCP generation lease、extension owner、skill load report、prepared dispatch fence、RunSlotGuard 和 durable commit 增量演进；不复制已经存在的能力，不照搬 9/30 旧审计的过时结论。当前源码状态详见 [current-state.md](current-state.md)，设计目标和信任边界详见 [architecture.md](architecture.md)，一手项目证据详见 [reference-projects.md](reference-projects.md) 与 [opencode-v2.md](opencode-v2.md)。P1–P7 的 `commands[]` 是测试实现后的目标验收命令，当前不可执行；P0 的复现命令、冻结日志与基准数据见最终 [feasibility.md](feasibility.md)。

## 推进顺序与启用门

| 阶段 | 结果 | 默认启用状态 | 依赖 |
|---|---|---|---|
| P0 | 临时 PoC 先红后绿，验证快照/租约/回退契约和基准方法 | 不接生产 | 无 |
| P1 | 为组合根增加窄的 typed factory seam，行为不变 | 可默认启用 | P0 通过 |
| P2 | 外部公开 SDK、skill/hook/prompt/command/provider 都有真实 consumer | 默认旧行为兼容；新增能力逐项显式启用 | P1 |
| P3 | 规范化内容摘要、依赖 DAG、确定性能力计划 | 仅构建与校验，不做 live reload | P2 |
| P4 | 完整 candidate generation、owner scope、发布/排空状态机 | manager 可构建，live adopt 关闭 | P3 |
| P5 | 每 run pin、权限即时撤销、恢复重授权、迟到写 fence | 完成安全验收后按 turn 边界启用 | P4 |
| P6 | 版本化 subprocess RPC 扩展 runtime 与进程管理 | opt-in；无 OS 沙箱的平台需明确降级 | P2、P4、P5 |
| P7 | 资源/性能回归门和 WASM adoption ADR | WASM 不属于必选依赖 | P0、P4、P5；RPC 基准依赖 P6 |

任一阶段没有满足回退、撤权、排空或资源验收时，保留上一阶段行为并关闭相关动态入口。不可把“注册成功”作为“CLI 已能消费”的验收，也不可把新 generation publish 与所有外部副作用宣称为通用事务。

## P0：隔离 PoC 与基准合同

- `id`: `P0-01`
- `role`: designer → builder → reviewer
- `title`: 验证 generation lease 与候选回退原型
- `status`: 基础机制验证完成；完整生产合同尚未验收。
- `user_story`: 作为运行时架构的实现者，我需要在不碰生产代码的环境里验证 generation acquire/release、失败候选回退和资源排空，以便在修改 Assembly 前暴露并发与所有权错误。
- `goal`: 在 `/tmp/go-cli-architecture-20261018.QZFu4f/` 下的独立临时 Go module 中，先写会失败的合同测试，再实现最小状态机；记录源码、命令、race 结果、allocs 和边界。不把 PoC 的结果当作生产实现证明。
- `deliverable`: 先有红测，再有绿实现；PoC 包含 generation snapshot、lease/drain、内容 hash 变更检测、DAG 校验、失败候选保旧、确定性 hook 顺序/取消、当前 policy 撤销、owner cleanup、背压及最小 benchmark。可复现结果归档到本次研究目录的 feasibility 报告；临时 module、下载与构建产物留在指定 `/tmp` 路径，不进生产仓库。
- `verification[]`:
  - `AC-P0-1`: 启动一个 run 持有旧 generation lease；发布新 generation 后旧资源保持打开，拒绝新 acquire；旧 run 结束并 release 后旧 owner 才关闭。
  - `AC-P0-2`: 候选任一 Init/compile/DAG 检查失败时，current generation ID 不变；候选已登记 owner 恰好逆序关闭一次。
  - `AC-P0-3`: PoC 内相同规范化输入重复构建得到相同 prompt/digest；不同 prompt bytes 改变 digest。真实文件系统中保留 mtime/size 修改内容的校验留到 P3。
  - `AC-P0-4`: DAG 拒绝缺依赖、重复 provider、循环；不同 map / 注册遍历顺序产生相同拓扑及 hook 执行顺序。
  - `AC-P0-5`: revoke epoch 发布是线性化点；此后新 invocation 失败关闭。已有 invocation token 的 callback 只保证合作式取消，完成时 fence 拒绝迟到结果；任意 callback 的外部副作用仍可能是 `unknown/partial`，不宣称通用原子停止。
  - `AC-P0-6`: 有界并发 limiter 到达上限时，等待可被 context 取消；不创建无界调用 goroutine。wire byte accounting 与 framing overhead 不在 PoC 验收范围，留到 P6。
  - `AC-P0-7`: 先运行不通过的合同测试，再实现；Builder 冻结快照的 16 个顶层测试（6 个基础测试、10 个回归测试）普通与 race 均通过；Reviewer 独立冻结副本经 4 个源码文件 SHA 匹配后，普通与 race 均通过，26 个顶层测试各运行 25 轮通过。该结果验证隔离原型机制，不代表本节 11 项 AC 全部完成，也不代表生产合同已验收。
  - `AC-P0-8`: benchmarks 报告 `ns/op`、`B/op`、`allocs/op`；最终有效数据与 Go/OS/CPU、命令只记录在 [feasibility.md](feasibility.md) 结果表，作为本研究唯一的 PoC 数字来源。Reviewer 修复改变代码后，初轮 benchmark 不能代表最终结果。
  - `AC-P0-9`: 以下 8 个原始独立复审交错均由 barrier 测试覆盖，并在最终 Reviewer 冻结副本的普通、race 与重复复测中通过：stale candidate 不覆盖新意图；独立 invocation ref 保活；snapshot accessor 不泄漏可变对象；撤权后非合作 callback 的完成结果被 fence；并发 reload 不并行构造候选；finalizer panic 不跳过后续 cleanup；reentrant `Scope.Close` 不自锁；`Manager.Close` 等待候选 builder 并对 shutdown 状态给出真实结果。
  - `AC-P0-10`: 若 A generation 因 lease/in-flight owner 尚未排空而 retiring、B 已成为 current，此时发起 C reload 必须只合并到 pending，不可启动 C builder；A drain 后才可构造候选。current+retiring+candidate 三代同时占用必须被 barrier 对抗测试拒绝。Builder 与 Reviewer 冻结副本均通过 `TestNoCandidateBuildWhileRetiringGenerationIsLeased`。
  - `AC-P0-11`: 等待 retiring generation 排空时，取消等待的 context 应及时返回，且不得破坏 current 或旧 generation 的 drain；Builder 与 Reviewer 冻结副本均通过 `TestRetiringGenerationWaitHonorsCancellation`。
- `review_result`: Builder 冻结快照有 16 个顶层测试（6 个基础测试、10 个回归测试），普通与 race 均通过。Reviewer 独立冻结副本与 Builder 快照的 4 个源码文件 SHA 匹配；普通与 race 均通过，`go test -count=25 -timeout=120s -run '^Test' ./...` 的 26 个顶层测试（Builder 16 + Reviewer 10）各运行 25 轮通过，用时 8.210 秒。A retiring、B current 时 C reload 等待 drain、取消等待不扰动排空的两个资源门回归均通过。基线 `make verify` 退出码 0、全部 gate 通过。详见最终 [审查记录](review.md) 与 [PoC 复现和基准报告](feasibility.md)。上述结果只验证隔离原型机制，不表示本节列出的 11 项 AC 全部完成，也不表示生产集成合同已验收。
- `implemented_tests[]`（临时 PoC 基础测试）: `TestBuildOrderIsStableAndRejectsInvalidGraphs`, `TestHookAndPromptPlansAreDeterministic`, `TestLeasePinsOldGenerationAndCandidateFailurePreservesCurrent`, `TestRevokeDeniesOldLeaseAndCancelsInFlightCall`, `TestScopeClosesLIFOAndLimiterHonorsCancellation`, `TestAcquireReloadRaceKeepsSnapshotFieldsCoherent`。
- `review_regression_tests[]`（Builder 与 Reviewer 冻结快照均通过）： `TestLatestConcurrentReloadIntentWins`, `TestCallPinsGenerationUntilCallbackReturns`, `TestSnapshotReturnsDetachedValue`, `TestRevokedNonCooperativeCallCannotReturnSuccess`, `TestReloadBuildersAreSerialized`, `TestNoCandidateBuildWhileRetiringGenerationIsLeased`, `TestRetiringGenerationWaitHonorsCancellation`, `TestCloseWaitsForInFlightCandidateCleanup`, `TestScopePanicAndReentrantCloseDoNotStopLIFOCleanup`, `TestScopeCloseInProgressIsExplicitAndWaitable`。
- `commands[]`: Builder 快照普通/race、Reviewer 冻结副本普通/race 与全 26 个顶层测试 `-count=25` 的复跑结果见最终 [review.md](review.md)；P0 最终命令、日志、复现源码与基准见 [feasibility.md](feasibility.md#复现步骤)。
- `not_covered[]`: 真实文件保留 mtime 与 size 但内容变化的 digest 校验（P3）；wire frame + serialization bytes accounting（P6）；生产 Snapshot 实际嵌套 slice/map 的只读封装与边界（P4）；生产 Assembly/CLI/skill/MCP/session 的实际 run 与 commit 集成（P1+）；candidate overlap 的实际 heap/RSS、exclusive-resource cutover、子进程 OS 资源限制及真实负载 Go/Rust 对照（P4/P6/P7）。这些都不是本次 toy PoC 的通过项；A-retiring/B-current/C-candidate 的等待 drain 及等待取消行为由 Builder 与 Reviewer 冻结副本验证通过。
- `rule_mapping[]`: `verify-rules.md §5.1 contract-first`; `VT-016` 只用于测试先于实现；`VT-019` 只在 Mock-vs-Real adapter 对照存在时适用，当前独立 PoC 没有真实生产 adapter；`§3.1 goroutine/race` 与 `§3.2 context cancellation`; `go-validation-rules.md concurrency/error wrapping/interface satisfaction`。不把 CLI 专项 VG-001/VG-002 映射到独立 PoC。
- `depends_on[]`: `[]`
- `rollback`: PoC 失败时只保留失败用例与结论，删除临时可执行产物；不触碰生产代码。修正契约或状态机后重新执行红/绿/race，不通过则阻止 P1。
- `resource_acceptance`: 目标仍为 current+candidate / current+retiring 不叠加第三代；Builder 与 Reviewer 冻结副本通过 `TestNoCandidateBuildWhileRetiringGenerationIsLeased` 与 `TestRetiringGenerationWaitHonorsCancellation`。资源门要求 A drain 前没有 C builder、A drain 后才构建最新 pending revision；测试仅证明 toy 状态机行为，不代表生产 RSS/OS 资源或真实 Assembly 已测。PoC 分配数只按 [feasibility.md](feasibility.md#poc-结果) 报告；lease/hook 降分配由真实 profile 驱动，不要求全 turn 0 alloc/op。

## P1：组合根接线缝，不改默认行为

- `id`: `P1-01`
- `role`: designer → builder → reviewer
- `title`: 为 extension loader 增加一个最小 Assembly 构造 seam
- `status`: pending
- `user_story`: 作为 CLI 维护者，我需要在不重写 `assemble.go` 的前提下替换受测构造点，以便后续引入候选计划和动态能力时继续复用现有 pipeline。
- `goal`: 只在现有 `assembleExtensions` 的 plugin loader 创建处增加 package-private 使用方 factory seam；默认实现仍调用当前 loader/manager。首个 PR 不抽象模型、tool、MCP、session 或整个 `assembleState`，不拆 `assemble.go`，保持构造顺序、默认值、错误和关闭行为。后续 seam 只在某个 consumer 的合同测试确实需要时单独加。
- `deliverable`: 一个小接口/函数依赖、当前实现的 adapter 与故障注入测试；Assembly 默认构建仍沿当前函数路径。不得把 `AgentAssembly.Registry` 的组装后替换误当作前置 factory。
- `verification[]`:
  - `AC-P1-1`: 新旧默认 CLI fixture 的命令、prompt、tool schema、错误类别与关闭顺序相同。
  - `AC-P1-2`: 注入 plugin loader factory；setup 失败时已登记 extension owner 逆序清理且不部分投影；成功路径保持当前 tools/hooks/middleware 投影顺序。
  - `AC-P1-3`: 对依赖 seam 执行 `go test -race -count=1`，并通过 `go vet`；编译期实现满足 interface。
  - `AC-P1-4`: 默认 Assembly fixture 保持现有 trace parent-child 链和结构化错误类别；日志不含密钥或 prompt 全文。
- `planned_tests[]`（尚未实现）: `TestAssembleExtensionsUsesDefaultPluginLoader`, `TestAssembleExtensionsInjectedLoaderFailureCleansOwner`, `TestAssembleExtensionsKeepsDefaultProjectionOrder`。
- `commands[]`: `go test -race -count=1 -run 'TestAssembleExtensions(UsesDefaultPluginLoader|InjectedLoaderFailureCleansOwner|KeepsDefaultProjectionOrder)' ./internal/cli ./internal/extension`; 随后 `go vet ./internal/cli ./internal/extension`。
- `rule_mapping[]`: `go-validation-rules.md 接口/包结构/错误链`; `verify-rules.md §5.1 contract-first / VT-016 / VT-019`。此最小 seam 不创建 goroutine，不映射 CLI goroutine-leak ID。
- `depends_on[]`: `["P0-01"]`
- `rollback`: 保留现有 default constructor 调用，撤销新 factory wiring；不改变配置和依赖版本。无需迁移用户数据。
- `resource_acceptance`: P1 默认路径不得启动额外 watcher、后台 worker 或 extension 子进程；对同一真实 Assembly fixture 在当前实现与 seam 实现之间做 before/after 对照，记录 CPU、`B/op`、`allocs/op`、startup p50/p95，并解释超出基线噪声或预先约定回归阈值的差异。PoC 的 toy fixture 不作为生产资源基线。

## P2：让能力有稳定 API 且被真实消费

- `P2-01` — 公开 SDK / wire schema
  - `id`: `P2-01`
  - `title`: 稳定外部 SDK 与 wire schema
  - `role`: designer → builder → reviewer；`status`: pending
  - `user_story`: 作为外部扩展作者，我需要稳定且不依赖 `internal/` 的 API，以便更新 go-cli 时不必同步追随内部包。
  - `goal`: 定义 `pkg/extensionapi/v1` 或独立可版本化模块，明确 host API range、manifest、capability、tool schema/call、hook event/patch、command、provider request/stream、skill/prompt contribution 与诊断 DTO。
  - `deliverable`: API compatibility policy、wire golden fixtures、仅依赖公开包的最小外部示例。此任务只定公共合同，不宣称每种贡献都已有 consumer。
  - `verification[]`: `AC-P2-01-1` 外部示例 build/test 成功且 `go list -deps` 无 `internal/`；`AC-P2-01-2` golden fixtures 跨版本 decode/encode；`AC-P2-01-3` 未知 capability/API range 失败关闭并返回可定位错误。
  - `planned_tests[]`（尚未实现）: `TestPublicExtensionAPINoInternalImports`, `TestExtensionAPIGoldenCompatibility`, `TestExtensionAPIRangeRejectsUnsupportedHost`。
  - `commands[]`: `go test ./pkg/extensionapi/v1 ./tests/external-extension`; `go list -deps ./tests/external-extension/...` 并检查结果无 `internal/`。
  - `rule_mapping[]`: `go-validation-rules.md internal 边界/使用方定义最小接口/导出文档注释/错误链`; `verify-rules.md §5.1 / VT-016 / VT-019`。
  - `depends_on[]`: `["P1-01"]`
  - `rollback`: API additive；不移除现有 `.so`；若 wire shape 有歧义，先冻结 schema fixture 并推迟 runtime，不通过 `internal` 类型做临时逃生口。
  - `resource_acceptance`: SDK 只是定义/适配层，不启动 worker、不扫描内容；wire 序列化有最大 frame/bytes 合同，具体阈值由 P6 fixtures 验证。

- `P2-02` — 默认 skill registry consumer
  - `id`: `P2-02`
  - `title`: 把默认 YAML skills 接入真实 registry consumer
  - `role`: designer → builder → reviewer；`status`: pending
  - `user_story`: 作为使用 skills 的用户，我需要默认 Assembly 通过 `SkillRegistry.List/Match` 发现 YAML skills，以便 skill 能力与报告、prompt/tool 投影使用同一索引。
  - `goal`: 把现有 loader/report/data adapter 接入 Registry，不重复解析项目文件、不绕开 captured verified bytes；先保持当前 skill precedence 与工具行为。
  - `deliverable`: Assembly 中的实际 registry consumer、明确冲突/来源优先级、现有 skill load report 兼容；不是新造 loader。
  - `verification[]`: `AC-P2-02-1` 默认 YAML skills 可由 List/Match 命中并进入 prompt/tool projection；`AC-P2-02-2` 修改未验证项目文件不改变捕获的索引；`AC-P2-02-3` skipped/error report 类别与当前行为一致。
  - `planned_tests[]`（尚未实现）: `TestDefaultAssemblyUsesSkillRegistryList`, `TestSkillMatchUsesCapturedVerifiedBytes`, `TestSkillLoadReportCompatibility`。
  - `commands[]`: `go test -race -count=1 -run 'Test(DefaultAssemblyUsesSkillRegistryList|SkillMatchUsesCapturedVerifiedBytes|SkillLoadReportCompatibility)' ./internal/cli ./internal/skill`。
  - `rule_mapping[]`: `verify-rules.md §5.1 / VT-016 / VT-019`; `go-validation-rules.md internal package boundary / deterministic config / error wrapping`。
  - `depends_on[]`: `["P1-01"]`
  - `rollback`: 若匹配/工具行为改变，关 registry projection adapter 并回到当前 YAML deferred-tool adapter；保留报告与已验证 source 读取路径。
  - `resource_acceptance`: parse/index 仅在 assembly/candidate 阶段；dispatch 不扫描磁盘。分别量默认、用户、project skill 数量下 build 的时间/heap/alloc，不从候选 tool 数推断实际 turn 成本。

- `P2-03` — extension command consumer
  - `id`: `P2-03`
  - `title`: 在 CLI route resolution 前消费 extension commands
  - `role`: designer → builder → reviewer；`status`: pending
  - `user_story`: 作为 CLI 用户，我需要受信任 extension 注册的 command 能在路由解析前被发现并执行，以便 `RegisterCommand` 真正成为 CLI 能力。
  - `goal`: 在 config/project trust 建立后、subcommand route resolution 前构建 command index；保持 builtin route 优先级、help 与 duplicate/error 语义可预测。P6 RPC command 仍未启用。
  - `deliverable`: pre-route bootstrap consumer、duplicate/unknown/API/trust 诊断和受信任来源链；未签名的 hash 不当来源认证。
  - `verification[]`: `AC-P2-03-1` fake command 从真实 CLI 参数路径可路由；`AC-P2-03-2` duplicate/untrusted command 在执行 handler 前 fail-closed；`AC-P2-03-3` builtin help/unknown route 回归一致。
  - `planned_tests[]`（尚未实现）: `TestCLICommandProjectionPrecedesRouteResolution`, `TestUntrustedCommandRejectedBeforeExecution`, `TestDuplicateExtensionCommandFailsClosed`。
  - `commands[]`: `go test -race -count=1 -run 'Test(CLICommandProjectionPrecedesRouteResolution|UntrustedCommandRejectedBeforeExecution|DuplicateExtensionCommandFailsClosed)' ./internal/cli ./internal/extension ./cmd/cli`。
  - `rule_mapping[]`: `verify-rules.md VQ-001 / VQ-003 / VQ-004`; `verify-rules.md SCAN-010 command route`; `verify-rules.md §5.1 / VT-019`。
  - `depends_on[]`: `["P2-01"]`
  - `rollback`: feature gate 关闭 extension command projection，保留 builtin command registry；未验证 command 绝不回退成直接执行。
  - `resource_acceptance`: command discovery 只在 CLI bootstrap 执行一次；每次 route lookup 不启动 loader、扫描目录或保留无限 handler goroutine。

- `P2-04` — extension provider consumer
  - `id`: `P2-04`
  - `title`: 在 model resolve 前消费 extension providers
  - `role`: designer → builder → reviewer；`status`: pending
  - `user_story`: 作为 provider 扩展用户，我需要受信任的 provider 在模型 resolver 运行前注册，以便配置选择能够得到真实 model implementation。
  - `goal`: 将 provider projection 放在 resolver/`assembleModel` 前，明确 name/model/API conflict 与失败回滚；本任务只接静态/现有 extension source，不实现 subprocess runtime。
  - `deliverable`: provider registry consumer、model list/resolution 接线和失败诊断；模型 request/stream 使用公开 SDK DTO adapter。
  - `verification[]`: `AC-P2-04-1` fake provider 在 resolver 选择前可发现/调用；`AC-P2-04-2` 冲突、不兼容或 init 失败时无部分 model/provider 暴露；`AC-P2-04-3` provider stream 取消能终止合作 callback。
  - `planned_tests[]`（尚未实现）: `TestExtensionProviderProjectionPrecedesModelResolve`, `TestProviderProjectionRollsBackOnInitFailure`, `TestProviderStreamCancelPropagates`。
  - `commands[]`: `go test -race -count=1 -run 'Test(ExtensionProviderProjectionPrecedesModelResolve|ProviderProjectionRollsBackOnInitFailure|ProviderStreamCancelPropagates)' ./internal/cli ./internal/llm ./internal/extension`。
  - `rule_mapping[]`: `verify-rules.md §3.2 context cancel / §5.1 / VT-019`; `go-validation-rules.md minimal interface / error chain`。
  - `depends_on[]`: `["P2-01"]`
  - `rollback`: feature gate 关闭 provider projection，保留现有 builtin model resolver；失败 candidate 不留半个 provider，也不改变已有 API key 配置。
  - `resource_acceptance`: provider setup 只在 assembly/candidate 阶段；每次 model token/event 调用不重新解析 manifest。并发/stream bytes 由之后的 runtime owner/背压合同约束。

## P3：确定性候选计划与内容寻址

- `id`: `P3-01`
- `role`: designer → builder → reviewer
- `title`: 编译不可变能力计划并验证依赖图
- `status`: pending
- `user_story`: 作为运行中的 CLI，我需要对相同受信任内容生成相同 skill、hook、tool、prompt 与 dependency plan，以便诊断、回滚和审计可以精确复现。
- `goal`: 从 captured trusted sources 与配置产生规范化输入，生成 content digest、拓扑顺序、HookPlan、SkillIndex、PromptPlan、tool/provider projection。首版可全图重建，只有 profile 显示问题再拆增量；阶段完成仍不启用 live reload。
- `deliverable`: 版本化 canonical encoding 和 SHA-256/Merkle 输入清单；DAG 校验；确定性排序；完整候选诊断；prompt/capability digest。文件 mtime/size 只能作为发现变化的优化，不能作为 identity。artifact digest 只表示内容匹配，不验证 signer 或来源。
- `verification[]`:
  - `AC-P3-1`: 固定 fixture 下 map 次序、并发发现次序或重复读取不会改变 generation digest、dependency order、HookPlan 或 prompt digest。
  - `AC-P3-2`: 修改真实文件内容、保持字节长度并恢复原 mtime 后仍会改变 digest；未验证的项目路径不会进入 plan。
  - `AC-P3-3`: DAG 对 cycle、missing provider、重复能力、版本冲突、未授权 capability 返回可定位的诊断；构建任何资源前完成纯校验。
  - `AC-P3-4`: PromptPlan 对 system/custom replace、append、tools、verified context、selected skills、memories 等现有输入有明确排序；生成字节与旧默认路径对照确认无意外语义差异。
  - `AC-P3-5`: hook plan 仅包含当前受支持事件，排序稳定、patch 边界只读/受限；取消和分类失败策略均覆盖。
  - `AC-P3-6`: 工具定义、prompt 可见能力与实际 prepared dispatch 来自同一候选 plan；无部分注册外泄。
- `planned_tests[]`（尚未实现）: `TestContentDigestChangesWithSameSizeAndRestoredMtime`, `TestDependencyGraphRejectsCycle`, `TestDependencyOrderIsStable`, `TestPromptPlanHasStableDigest`, `TestHookPlanHasStableOrder`, `TestCandidateDoesNotExposePartialProjection`。
- `commands[]`: `go test -race -count=1 -run 'Test(ContentDigest|Dependency|PromptPlan|HookPlan|Candidate)' ./internal/cli ./internal/extension ./internal/skill ./internal/config`。
- `rule_mapping[]`: `verify-rules.md §5.1 contract-first / VT-016 / VT-019` 仅适用于 mock/real contract 与先测试后实现；`verify-rules.md §3.2 context cancellation` 仅适用于 hook/call cancel；`verify-rules.md SCAN-009` 仅适用于 prompt/default literal 扫描；`go-validation-rules.md deterministic config/error chain`。
- `depends_on[]`: `["P2-01", "P2-02", "P2-03", "P2-04"]`
- `rollback`: plan compiler 只用于 compare/report，若默认 prompt 或 skill 行为变化则回到现有 builder/loader；不要发布不完整 digest；不要求清理或迁移已有 session。
- `resource_acceptance`: 重载控制面可读取/哈希输入；turn/tool 热路径只消费冻结 index，不碰文件系统。记录全图 build 的耗时、峰值 heap、临时 `B/op`、skill/tool 节点数；计划本身不保留重复完整 prompt 文本副本。

## P4：generation 状态机、scope 与现有 MCP 复用

- `id`: `P4-01`
- `role`: designer → builder → reviewer
- `title`: 发布不可变 runtime generation 并排空旧 owner
- `status`: pending
- `user_story`: 作为正在执行的 run，我需要完整使用一个已验证 generation 并在更新后继续持有它所需的资源，以便新旧配置不会在一次工具调用中混用。
- `goal`: 实现 current/candidate/retiring/pending 状态机、Acquire/Release/retire/drain、candidate 失败回退和有限 owner scope。将现有 MCP source generation 作为下层 owner/ref 来组合，不复制 MCP transport；共享连接只通过专门 ref-counted owner，最后一个引用释放才 close。
- `deliverable`: 不可变 `GenerationSnapshot`、窄 lease、候选 builder、原子 publish 协议、依赖逆序 cleanup、有限 drain/诊断。候选构建外部 callback 不持 manager/global lock；panic 被边界捕获并按合同清理。session store / DB schema / process-wide exclusive resources 不属于普通 generation owner。
- `verification[]`:
  - `AC-P4-1`: 高并发 acquire/publish/retire 只取得完整 old/new snapshot；无可能 acquire 到已关闭 owner；`go test -race` 压测重复通过。
  - `AC-P4-2`: 任何候选失败、revision/trust epoch 变化或权限 epoch 不匹配都不 publish，旧代保持可服务；候选 owner 恰好关闭一次。
  - `AC-P4-3`: generation lease 在工具并行、子任务、stream/event producer 及异步 commit 期间保持；调用者提前返回不引起早释放。
  - `AC-P4-4`: MCP unchanged source 的 generation 复用不会重复 dial 或 double close；source 变更时旧连接 drain 后关；对照现有 MCP lifecycle tests。
  - `AC-P4-5`: 资源状态限制为 stable current=1；candidate build 时 current+candidate≤2；publish 后 current+retiring≤2。retiring 未完成时再次 reload 合并最新 revision，不再启动全 scope。
  - `AC-P4-6`: exclusive port/store lock candidate dry validation 可标记 `restart-required` 或受控 cutover；DB/session schema 变更不能走普通 owner hot swap。
  - `AC-P4-7`: 较新 revision 在慢 candidate 前先被请求时，旧 candidate 在 publish 线性化点被拒绝并清理，不能覆盖新请求的 revision；用 channel barrier 确定交错，不靠 sleep 猜竞态。
  - `AC-P4-8`: published snapshot 对外没有可变指针；改写 accessor 返回的 slices/maps/nested schema 不改变 generation 的其它 reader。`BeginCall` 在 callback 开始时取得独立 invocation ref，parent RunLease release 也不能提前 close owner，调用结束并释放 invocation ref 后才 drain。
  - `AC-P4-9`: reload coordinator 单飞；candidate 构建期间的新请求仅更新/合并 pending revision，不额外构建 candidate；retiring 未排空时不开始下一 candidate，始终满足两个代的峰值上限。
  - `AC-P4-10`: finalizer panic 不阻断后续逆序清理并返回聚合诊断；同 scope finalizer 重入 Close 不等待自身；Manager.Close 跟踪候选 builder，等待其退出或在 context deadline 后返回明确的未退出诊断。
- `planned_tests[]`（尚未实现）: `TestGenerationPublishRejectsStaleRevision`, `TestSnapshotAccessorsCannotMutatePublishedState`, `TestInvocationPinSurvivesParentLeaseRelease`, `TestGenerationAcquirePublishRetireLinearizable`, `TestMCPResourceRefCountsAcrossGenerations`, `TestExclusiveResourceRequiresCutover`, `TestConcurrentReloadsCoalesceWithoutParallelBuilders`, `TestScopeContinuesAfterFinalizerPanic`, `TestScopeReentrantCloseDoesNotDeadlock`, `TestManagerCloseWaitsForCandidateBuilder`。
- `commands[]`: `go test -race -count=25 -run 'Test(Generation|SnapshotAccessors|InvocationPin|MCP.*Generation|ExclusiveResource|ConcurrentReloadsCoalesce|ScopeContinuesAfterFinalizerPanic|ScopeReentrantClose|ManagerCloseWaitsForCandidateBuilder)' ./internal/cli ./internal/mcp ./internal/extension`。
- `rule_mapping[]`: `verify-rules.md §3.1 goroutine/race/channel`, `§3.2 context cancellation`, `§3.4 channel close safety`; add `§5.1 / VT-016 / VT-019` only for mock/real lifecycle contract tests; `go-validation-rules.md error chain/interface`。CLI-only `VG-001/VG-002` and trace-only `VT-010..VT-014` are not blanket requirements for package-level manager tests。
- `depends_on[]`: `["P3-01"]`
- `rollback`: live reload 仍禁用；若 scope/refcount 接口失败，继续通过已有 MCP generation owner 管理 MCP，并保留 Assembly close owner；不要在没有 lease 的情况下接通 generation 更新。
- `resource_acceptance`: 限定 generation 数为上述阶段上限；并发调用按 owner semaphore 和字节预算背压，不能创建无界 goroutine/channel。Close 必须 deadline、有诊断；忽略 context 的 in-process callback 可报告卡住但不得伪报强杀。

## P5：每次 run pin、立即撤权与迟到提交 fence

- `id`: `P5-01`
- `role`: designer → builder → reviewer
- `title`: 按 run 准入固定快照并保护提交边界
- `status`: pending
- `user_story`: 作为交互式 session 用户，我需要当前 run 固定一套 prompt、skills、tools、hooks 与 MCP 资源，同时允许后续 run 采用更新，并使撤权立即生效，以便动态更新不破坏在途执行或授权边界。
- `goal`: P2 期间保持现有交互行为；本阶段完成验收后，才通过显式 reload/配置更新在 turn 边界启用新代。Session 不持有 generation resource lease。把 policy/trust epoch 放在 generation 生命周期外，每次敏感副作用及结果提交再授权。用现有 RunSlotGuard、prepared dispatch、Mutation fence 和 RunCommitSink 增量实现 late-write fence。
- `deliverable`: RunAdmission 绑定 `runID + selected generation ID + policy/trust epoch`；runOwner 持 lease 到执行、子任务、stream 与 durable commit quiescent；持久记录 generation/capability/prompt digest；resume 用当前配置重新可见性过滤并按当前权限授权。撤销线性化后拒绝新 invocation；取消在途 cooperative work；host completion fence 拒绝迟到结果。只有串行化 admission/commit 的具体 mutation adapter 才能承诺更强的撤销后写入保证。
- `verification[]`:
  - `AC-P5-1`: generation 发布只影响之后准入的 run；一次 run 的 prompt、tool list、prepared dispatch、hook 和 MCP 连接全来自 pinned generation。
  - `AC-P5-2`: Session 本身没有永久 generation lease；长 session 经过多代更新后旧 owner 及时释放。一个仍有子任务/stream/commit 的 run 保持 lease。
  - `AC-P5-3`: revoke 线性化后新 invocation 被拒；已获 token 的 callback 只保证合作式取消与结果 fence，不保证任意插件内部副作用原子停止。prepared mutation adapter 如需更强保证，必须在 adapter commit 边界串行复核当前 policy。
  - `AC-P5-4`: 历史 turn 的 digest 可审计；resume 时按当前 tool/skill/project trust 和当前 policy 重建，不复活旧权限。
  - `AC-P5-5`: old generation run 在 drain 中可提交自己的合法结果；提交时验证 session 当前 run owner、该 run 入场 pinned generation、invocation epoch 和当前 policy，而不要求 old generation 等于 current generation。
  - `AC-P5-6`: 迟到 run 的 commit/memory/todo/result 不得覆盖新 session owner；外部副作用发生但结果未知时状态明确为 partial/unknown。
  - `AC-P5-7`: 取消、shutdown、同 session 新 run 接管/分支、并行工具、durable append 场景通过 race、multi-turn 和 goroutine-leak 测试；测试日志包含可审计 run/generation ID，不记录秘密。
  - `AC-P5-8`: revoke 后忽略 context 的 callback 即使最终返回 nil，host completion fence 必须重读当前 policy/trust epoch，拒绝对外结果、event 和 commit；对于其可能已经完成的外部副作用返回 unknown/partial，不谎报取消成功。
- `planned_tests[]`（尚未实现）: `TestRunPinsGenerationUntilAsyncCommit`, `TestCallResultFencedAfterRevoke`, `TestResumeReauthorizesWithCurrentPolicy`, `TestOldRunCommitAllowedWhileDraining`, `TestLateRunCannotOverwriteCurrentSessionOwner`, `TestRevokeLinearizesAgainstNewInvocation`, `TestPreparedMutationRejectsAtCommitAfterRevoke`, `TestNonCooperativeExternalSideEffectReportsUnknown`。
- `commands[]`: `go test -race -count=25 -run 'Test(RunPinsGeneration|CallResultFenced|ResumeReauthorizes|OldRunCommit|LateRunCannotOverwrite|RevokeLinearizes|PreparedMutationRejects|NonCooperativeExternalSideEffect)' ./internal/core ./internal/cli ./internal/session ./internal/approval ./internal/mcp`。
- `rule_mapping[]`: `verify-rules.md §3.1..3.4` for run goroutine, cancellation, error/channel contracts; CLI end-to-end cancellation/leak tests map to `VG-001/VG-002`; use `VT-011/VT-013` only if tool span/parent chain is touched, and `VT-015/VT-017/VT-018` only for the explicit long/multi-turn fixtures; `VT-019` applies where mock and real adapters are compared. `go-validation-rules.md error wrapping/context/goroutine lifecycle`。
- `depends_on[]`: `["P4-01"]`
- `rollback`: turn-boundary adopt 保持 feature-gated；撤销后仍使用当前 Assembly 生命周期固定能力的既有行为。若当前策略校验无法穿透 prepared dispatch，则禁止启用 live reload；不允许降级成 prompt-only 隐藏能力。
- `resource_acceptance`: 每 run 最多一个 generation lease；Session 长时间存在不占用旧代资源。审批/permission lookup 使用单调 policy epoch，不持 generation 内永久许可缓存。warm `Acquire`/空 hook 微基准记录分配，是否减少分配由 production profile 决定；完整 Agent run 单独报告，不能从前者推断全系统零分配或 Rust 相当。

## P6：公开 SDK 的 subprocess RPC runtime

- `id`: `P6-01`
- `role`: designer → builder → reviewer
- `title`: 将不受信任或跨语言扩展放到有界子进程
- `status`: pending
- `user_story`: 作为扩展用户，我需要跨进程版本化地注册和调用第三方能力，以便扩展崩溃不会直接变成宿主进程 panic，并能按 owner 生命周期启动、撤销和回收。
- `goal`: 依据稳定 `extensionapi/v1` 建立握手、协议版本、能力协商、启动/健康/退出、调用并发、stream 背压和结果不确定状态。RPC 进程是故障边界，不自动成为安全沙箱；按平台单独执行环境清理、工作目录、用户/容器/namespace、文件/网络/CPU/RSS 策略。
- `deliverable`: go-plugin 或等价 RPC adapter；manifest 校验、artifact digest、可选签名验证的分离合同；自定义 Runner/ProcessOwner 接口；限定可用的传输和本机网络威胁模型。MCP wire protocol 不替代这一生命周期协议。Go `.so` 仅保留兼容受信任宿主路径，明确无 unload。
- `verification[]`:
  - `AC-P6-1`: API handshake mismatch、启动超时、子进程退出、无效 frame、checksum 不符时，候选不发布并清理 process owner，旧 generation 仍可服务。
  - `AC-P6-2`: transport 测试覆盖请求 ID、取消、重复响应、超大 frame、流背压、队列 overload 和调用 deadline；断连后把执行状态标记为 failed 或 unknown，不能盲目重放有副作用请求。
  - `AC-P6-3`: 默认启动不泄漏宿主 env/secret；允许的 env、cwd、stdin/stdout/stderr、network endpoint 都按策略列明。`SkipHostEnv` 或等价能力有测试。
  - `AC-P6-4`: artifact hash 仅验证字节完整性；没有签名/可信来源时不得显示为 authenticated。若接入签名，测试信任根/发布者撤销。
  - `AC-P6-5`: 每插件可配置 max inflight、queued bytes、timeout、process count；host shutdown 和撤权能 terminate 子进程并等待有界时间。
  - `AC-P6-6`: 平台测试分别记录 macOS/Linux/Windows sandbox 限制；无 OS 级策略时用户可见为“仅进程故障隔离、无资源/文件/网络沙箱”。
- `planned_tests[]`（尚未实现）: `TestExtensionHandshakeRejectsVersionMismatch`, `TestExtensionProcessOwnerCleansFailedCandidate`, `TestRPCStreamBackpressureBounded`, `TestRPCUnknownResultAfterDisconnect`, `TestExtensionProcessDoesNotInheritHostSecrets`, `TestProcessPermissionRevokeTerminatesOwner`。
- `commands[]`: `go test -race -count=1 -run 'Test(ExtensionHandshake|ExtensionProcessOwner|RPCStreamBackpressure|RPCUnknownResult|ExtensionProcessDoesNotInherit|ProcessPermissionRevoke)' ./pkg/extensionapi/v1 ./internal/extension ./internal/cli`；各 OS sandbox command/platform limitations 由 CI matrix 或专项环境另行记录。
- `rule_mapping[]`: `verify-rules.md §3.1..3.4` for process lifecycle, cancel/error/channel tests; CLI end-to-end process cancellation/leak coverage maps to `VG-001/VG-002`; `VT-016/VT-019` apply only to test-first and mock/real protocol contract tests. `go-validation-rules.md env/config/error handling`. Secret values and endpoint literals are handled by the existing config/source policy; `SCAN-003/SCAN-004` are not blanket RPC runtime tests.
- `depends_on[]`: `["P2-01", "P4-01", "P5-01"]`
- `rollback`: 子进程 runtime 默认 opt-in；handshake/稳定性问题时关闭 remote adapter，回到 builtin typed API 与现有受信任 `.so`。进程协议不可热升级时采用兼容 API range 或要求 host restart；hash/signature 验证失败不得 fallback 到未验证制品。
- `resource_acceptance`: 每插件并发和队列均有限；发送/接收 bytes 按 payload + framing/serialization overhead 记账；启动、warm call、stream、drain 分开测 CPU、RSS、尾延迟。操作系统不支持硬限制时必须标为缺少硬预算，不可用应用层 semaphore 宣称 RSS 隔离。

## P7：性能/资源回归门及可选 WASM ADR

- `id`: `P7-01`
- `role`: designer → builder → reviewer
- `title`: 用固定工作量设立性能门并决定 WASM 是否值得试用
- `status`: pending
- `user_story`: 作为框架维护者，我需要在相同语义和工作量下测量 Go 实现的分配、吞吐、尾延迟和资源使用，并单独判断跨语言 sandbox 需求，以便优化依据数据而非语言排名或技术偏好。
- `goal`: 建立 reproducible benchmark matrix，并在有真实需求时以独立 ADR 比较直接 wazero、Extism SDK、subprocess RPC 和 typed in-process；Extism 当前不作为计划内必选依赖。
- `deliverable`: benchmark fixtures 和 baseline 报告；Go/Rust 对照协议及原始数据；资源预算提案 review；WASM adoption gate（真实 use case、ABI/API、host function allowlist、实例线程安全、取消保证、memory/RSS、并发池、升级/撤权、供应链验证）。
- `verification[]`:
  - `AC-P7-1`: 固定 CPU/OS、Go/Rust toolchain、构建优化、功能语义、输入字节、并发、网络 stub，对比 `ns/op`、`B/op`、`allocs/op`、吞吐、p50/p95/p99、CPU、RSS、GC、启动、更新、drain。
  - `AC-P7-2`: 分开冷/暖路径、hook 0/1/N、prompt 固定字节与规模分桶、MCP tool JSON、candidate full/single-node reload、慢 tool drain、失败回退、撤权。
  - `AC-P7-3`: 报告暖态 generation acquire、空/简单 hook 的 `allocs/op`，但不预设它们必须为 0；只有真实 profile 证明分配处于热点且优化收益值得复杂度时，才另定优化目标。完整 run/stream/prompt 必须独立报告，不能改口声称 Go 接近 Rust。
  - `AC-P7-4`: WASM 如进入 PoC，逐一测试 host function 允许面、禁用 filesystem/network 默认值、runtime cancel 检查、非线程安全 instance 的池化、编译 cache 和 instance close；memory pages 不作为进程 RSS 总额度。
  - `AC-P7-5`: 做 WASM ADR 的明确结论：无真实跨语言或隔离需求时不接依赖；满足 adoption gate 后仍以 independent PoC/bench 为先，随后另起 implementation plan。
- `AC-P7-6`: Go/Rust 对照分别给出 `CPU_Go/CPU_Rust`、`throughput_Rust/throughput_Go`、`p95_Go/p95_Rust`、`RSS_Go/RSS_Rust`。先测 go-cli 真实工作负载基线，再由维护者在同一轮对照前记录各 ratio 的目标区间；当前不预设数字。目标未通过时如实判定瓶颈与优化优先级，不用单一微基准宣称“接近 Rust”。
- `planned_tests[]`（尚未实现）: `BenchmarkGenerationAcquire`, `BenchmarkHookDispatch`, `BenchmarkPromptPlanBuild`, `BenchmarkMCPToolProjection`, `BenchmarkGenerationReload`, `TestBenchmarkFixturesUseIdenticalPayloads`, `TestGoRustBenchmarkSemanticsMatch`。
- `commands[]`: `go test -run '^$' -bench 'Benchmark(Generation|Hook|Prompt|MCP)' -benchmem ./internal/core ./internal/cli ./internal/mcp ./internal/extension`; Go/Rust 对照需使用相同机器与 fixture，记录可复现命令和原始结果。
- `rule_mapping[]`: `verify-rules.md §3.1 goroutine/race` applies to benchmark fixture lifecycle; `§5.1 / VT-016 / VT-019` only where mock/real contracts are compared. `VT-018` is required only if a 100+ turn stability test is included; CLI-only `VG-002` does not apply to isolated benchmarks. `go-validation-rules.md` race/test/goroutine lifecycle.
- `depends_on[]`: `["P0-01", "P4-01", "P5-01"]`；若做 RPC 对照另依赖 `["P6-01"]`。
- `rollback`: WASM 是独立可选 runtime；adoption gate 未通过时不加依赖。benchmark fixture 不介入生产运行；出现性能回归时回滚被测优化，不通过关闭 race/verify gate 解决。
- `resource_acceptance`: 目标按前序阶段的真实 fixture 和实测输入分布设定，不从 toy benchmark 设生产 SLO，也不预先断言 generation acquire 必须 0 alloc/op。稳态运行与 candidate overlap 都报告峰值 heap/RSS；generation scope、RPC queue、WASM instance pool 各有显式上限。性能通过以可重复数据为依据，不能以一次 `atomic.Pointer` 微基准代表全 Agent。

## 对应关系与全局禁区

| 目标合同 | 首次验收阶段 | 关键覆盖 |
|---|---|---|
| generation snapshot + lease/drain | P0、P4、P5 | P0 验证模型；P4 生产状态机；P5 接入真实 runOwner 生命周期 |
| 失败 candidate 保留旧代 | P0、P3、P4 | 纯校验失败和部分资源 setup 失败都不可泄漏 |
| content hash invalidation / DAG | P0、P3 | hash 覆盖 canonical content/API/dependency/prompt/policy revision；DAG 必须在资源初始化前验证 |
| hooks 顺序、取消、prompt 确定性 | P0、P3 | 明确受支持事件；map 顺序/文件遍历不得影响结果 |
| 权限撤销与恢复重授权 | P0、P5、P6 | policy epoch 独立于 generation；resume 不复活旧授权 |
| owner cleanup、refcount、迟到写入 | P0、P4、P5 | 幂等逆序关闭；同一 run generation fence；旧代合法 drain commit 可通过 |
| 背压与资源预算 | P0、P4、P6、P7 | bounded inflight/bytes/owner；各 runtime 说清 RSS 控制能力 |
| public SDK / 外部隔离 | P2、P6 | 公开合同不 import internal；RPC 进程不被误说为 OS 沙箱 |
| Go 与 Rust 性能比较 | P0、P7 | 同语义、同输入、同宿主、报告 allocs/CPU/尾延迟/RSS；不据语言名声下结论 |

以下事项不会在本计划中被当成完成：给 `.so` 加 generation 指针便声称可卸载；session 持有永久 generation lease；强制要求旧 run 只能写 current generation；hash 等同签名；RPC 或 WASM 默认提供完整 OS/RSS 隔离；对不响应 context 的 Go callback 宣称强制取消；无签名来源验证的下载插件自动可信；将 MCP tool schema 当作通用插件生命周期协议；对 SessionStore/schema/端口锁等独占资源承诺总能 candidate 双开；仅凭一次 atomic load 或零分配微基准声称整机接近 Rust。
