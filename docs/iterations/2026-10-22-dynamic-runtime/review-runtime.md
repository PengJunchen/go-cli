# Runtime 与渐进能力独立复审

日期：2026-10-22。源码：独立 worktree `dynamic-capability-runtime/go-cli`。本复审按用户顺序在生产功能实现完成后运行测试；未修改保护文件、原 infra 任务或检查阈值。

## 已复核的真实接线

- `cmd/cli/main.go` 将入口原 `loadConfig` closure 放入 runtime context，`/reload` 真实重新读取配置；库调用保留显式 loader seam。
- Harness 与 TurnRunner 均在 run-slot admission 后 acquire generation，并将实际 prepared registry、metadata、hook chain、pre-tool interceptor 传入 context。runOwner 等待 callback、worker、resource、迟到 durable completion 和 commit lease 后释放 generation。
- LoopAgent 的 schema、prompt tools 与 prepared dispatch 使用相同 pinned registry。请求间 Version 变化才重新披露；同一响应夹带尚未披露的新 tool 在任何 dispatch 前拒绝。
- 新 MCP source 追加与该 source 首次 materialization 只改变 exposure Version，不改变已有 executor 的 authority fence；替换/禁用/重授予仍使旧 prepared decision 失效。
- project skills 使用 verified captured bytes；runtime completion guard 绑定实际参与计划的 project trust。工具 admission、完成结果、流输出、Agent 内存历史和 durable terminal checkpoint 均复核当前 fence。历史 digest 只用于审计，不恢复权限。
- stdio MCP 的成功连接 owner 不再继承 setup/errgroup context；HTTP 与 SDK RPC 也有独立 owner 生命周期。关闭请求 deadline 只结束等待，不假称强杀忽略 context 的 Go callback。

## 复审发现与修复

1. 全局 hook JSON 经 YAML-subset 路径解析成零个 hook，初始 interceptor 为 nil。作者修为按实际格式解析，并将全局 hook declarations 纳入 generation 摘要。
2. lazy MCP 的 `exec.CommandContext(setupCtx)` 会在 errgroup 完成或首 run 结束后杀掉已发布进程。作者改为 owner 持有进程，握手/请求取消由现有 process owner 处理，并补跨两次调用的真实 stdio 合同。
3. 原 deferred registry 没有传递 Version，导致本 run 下一次模型请求仍使用旧 schema。已合并 overlay revision 与 backing Version，并补真实 next-request/同响应攻击回归。
4. RPC descriptor materialization 丢失 `.rpc.json` 双后缀，错误转入 native loader。作者保留协议后缀。
5. RPC hash 后执行原 pathname 有内容替换窗口。现在执行同流校验后捕获的入口文件；临时目录由进程 owner 清理。额外 interpreter/script/assets 仍是单独受信依赖。
6. RPC FIFO/device artifact 可能使 candidate builder 卡在 Open。现在拒绝非普通文件，Unix 使用 nonblocking/no-follow open 后再次检查类型，复制循环检查取消。
7. 原最终 run fence 在 Agent 内存历史提交之后才运行。现在 Agent 历史、compaction 返回、完整模型响应与 event 发布也检查 fence，拒绝迟到私有结果。
8. 远端已 dispatch 后的非法结果形状与 callback panic 原来可能被记为确定失败。现在 transport/result decode 失败以及协议 `callback_panicked`/`outcome_unknown` 均保留 unknown 标记，普通 callback_failed 仍按声明的失败处理。
9. Load/握手已成功、Init 前依赖图被拒绝时，原 coordinator 未登记这些资源。PluginManager 现在仅将自己拥有的 exact instance 纳入同一 once-only shutdown 状态机；外部 unknown instance 保留原 noop 合同。
10. named MCP refresh 原来先发布 registry，随后构建 runtime plan 仍可能失败。现在完整候选 descriptor、prompt/schema 预算和 Snapshot 先准备，`ReloadWithCommit` 最后重新检查取消/revision/epoch，再提交 `PublishMaterialized` 与 owner；提交成功后不再运行可能失败的 loader 或网络操作。提交拒绝 Abort，保留当前 source 和 runtime generation。

11. 公开 SDK 的 command/provider 追加真实 CLI 入口验收时，命令已成功，但 provider 的实际结果被成本记录器拒绝：原 validator 只接受两类 HTTP endpoint 来源。最小修复允许 `OriginExtensionProcess` 与 `ProtocolExtensionRPC` 的组合，保留未知来源和协议不匹配拒绝；没有跳过 guarded candidate、operation authority 或成本记录。

MCP 发布的锁序为 progressive controller → DTR transaction → manager → MCP owner → dispatch fence → backing registry。AcquireRun 在 manager 解锁后才读取 registry；lease Acquire/Release 不取得 DTR、owner 或 manager 锁。MCPClientGeneration.Retire 只在私有短锁内改变状态、检查引用和关闭 drained channel；client Disconnect 在后续 cleanup goroutine。host commit 只允许有界内存发布，禁止用户 callback、I/O、等待、manager 重入以及返回错误前部分提交。

## 验证证据

| 验证面 | 命令/证据 | 当前结果 |
|---|---|---|
| snapshot/DAG/scope/manager、独立 invocation、revoke、prepared append | `go test -race -count=5 -run 'Test(Generation\|RevokeFences\|ScopePanic\|ManagerClose\|SnapshotDetaches\|CallResultFenced\|RunPinsGeneration\|PinnedRegistry\|ProgressiveDiscovery\|MCPAddition)' ./internal/capability ./internal/core ./internal/tools` | 三包通过；原始日志 `runtime/lifecycle-race.log` |
| capability / public SDK / extension / core 普通全包 | `go test -count=1` 对应四包 | 通过；extension 覆盖清空宿主 env、oversize/disconnect/timeout、worker exit、handshake/checksum；原始日志 `runtime/targeted-initial.log` |
| 标准 Skill 与 stdio setup lifetime | targeted `-race -count=3` | skill/mcp 两包通过；原始日志 `runtime/review-spec-race.log` |
| CLI 新功能独立复审 | 非法 hook candidate 保旧、未验证 project capture 拒绝 adopt、真实 Harness 两次 run 复用同一 MCP server | 独立回归与后续 CLI 合同已通过完整 gate；原作者最终定向 race 2.821s，见 `spec/logs/final-cli-contract-race.log` |
| CLI 原作者 targeted | `spec/logs/cli-targeted-3.log` | 作者报告修复后的 targeted 通过；独立复测须另记录 |
| 最终 MCP 原子候选普通定向 | oversized candidate 在首次激活和 named refresh 前拒绝、下一请求 schema 更新、project grant 变化拒绝 reconnect | 原作者执行 exit 0，0.954s；已读取原始 `spec/logs/mcp-atomicity-final.log`。最终完整 race gate 另行记录 |
| Snapshot permutation | 2 个 source 顺序 × 6 个 node 排列 × dependency/declaration 顺序反转，比较完整、能力和 prompt digest 与 canonical 投影 | 默认和 headless/no_plugin 的新增定向 race 均通过 |
| 公开 SDK 真实 CLI entry | 临时外部 module 仅依赖 public SDK+stdlib 编译 worker；临时 FileTrustStore 显式项目 grant，经原 loader 和 `runWithTrustManager` 执行 extension-hello 与 entrysdk/entrymodel prompt | 默认 race：capability 1.250s、production 1.608s、cmd/cli 2.661s，exit 0；`runtime/final-entry-default-race-3.log` |
| 相同 SDK entry 的轻量构建 | headless/no_plugin 执行相同 command、provider、握手、原始 argv、无宿主 key/endpoint、Wait 后捕获入口已删除断言 | race：capability 1.474s、production 1.547s、cmd/cli 2.573s，exit 0；`runtime/final-entry-headless-race.log` |
| 追加验收目标 lint | capability、production、cmd/cli，默认和 headless/no_plugin 分别执行 | 两次 exit 0，均 0 issues；`runtime/final-entry-default-lint-2.log`、`runtime/final-entry-headless-lint.log` |
| 完整 tools 普通包 | `runtime/targeted-initial.log` | 既有 grep escaped descendant helper 的 1 秒 PID 等待失败；不是整包通过 |

最后一轮 owned race 中 capability/public SDK/core 通过；extension 的 pre-Init cleanup 回归发现 Load-only owner 未纳入原 coordinator，已修为仅 PluginManager 自己拥有的 exact instance 进入同一关闭状态机，unknown 外部实例维持原 noop 合同。修复后 extension 全包 race 通过（`runtime/final-extension-race.log`，5.710s），session 全包 race 通过（`runtime/final-owned-fixes-race.log`，6.498s）。

后续新增的 host commit 拒绝、builder 取消、stale revision 不执行 commit，以及 frozen MCP descriptor 单事务 materialization 回归均已通过追加 SDK entry 前的完整全量 gate。取消检查在 manager 最终锁内再次读取；提交与取消真正并发时以发布线性化为界，不承诺回滚已提交的外部效果。

负载下 RPC env 测试曾在 2 秒 worker 启动/握手 fixture deadline 失败；安静原预算 race 重复 3 次通过。测试启动预算调整为 10 秒、外层 20 秒，专门测挂起 callback 的 100ms deadline 与全部 env/unknown/cleanup 断言保持。

既有 grep helper 在繁忙时失败；安静后相同 Go/环境/默认 tags/count=1，current 与 baseline-src 串行均 exit 0（`runtime/quiet-grep-current.log`、`runtime/quiet-grep-base.log`）。这支持负载敏感性的解释，不构成无竞态的证明，也没有修改旧测试超时。

本轮追加入口前的完整 `make verify` exit 0：8,959 个测试函数，8,953 通过、6 条件跳过、0 失败；E2E exit 0：4,877 个测试函数，4,871 通过、6 条件跳过、0 失败。追加的 RPC 来源成本校验修复已由上述默认/轻量定向 race 覆盖，主验收会按最终源码再运行完整 gate，不能用这里的旧完整 gate 替代。

原始运行日志已产生于本轮 `/tmp/go-cli-iteration-20261022.to3oRg/`，收尾时按用户要求清理；不会用 shell 最后一个 tail 命令的成功状态代替前面测试的 FAIL。完整 `make verify`、headless/no_plugin、平台构建与最终性能报告由主验收记录统一汇总。

历史中间失败未删除：`final-owned-race.log` 暴露上述 pre-Init owner 与 session fixture 缺少 RecordVersion 的问题；两者修复后的全包结果已列出。`final-cli-review-race.log` 的 CLI 编译失败发生在 named refresh 接线编辑期间，已修复，不能作为冻结源码的最终结论；同次 skill/mcp race 两包通过。本文不以中间失败替代最终 gate，也不以源码冻结替代测试通过。

追加 entry 验收的第一轮实际失败暴露成本 validator 来源枚举遗漏（`final-entry-default-race.log`）；第二轮 CLI 已通过但新 production 测试构造器漏传 nil tiers 导致编译失败，修复后的第三轮三包均通过。初次 lint 因当前 shell PATH 未含工具目录返回 127，使用已安装工具的绝对路径复跑；不把启动错误当作源码 lint 失败，也不隐去它。

## 范围与限制

CLI 的热更新是 idle run seam 的数据计划更新；shell hooks、prompt、Skill metadata 真实影响后续 run。代码/native plugin/provider/command 或不能双开的进程级配置变化返回 restart-required，不承诺 Go `.so` 热卸载。generic Manager 独立实现双代/候选失败保旧/lease drain，但不能将这一模型写成 CLI 已能无缝热替换任意资源。

SDK RPC v1 协商 tools、现有 hooks、commands、complete-response providers 与 prompts；provider Stream 只发单一 final chunk，不声称 token streaming。标准文件 Skill 单独提供渐进发现；RPC Skill DTO 尚未协商。进程是故障隔离，不提供文件/网络/CPU/RSS 沙箱；Unix 清理进程组，其他平台当前清理直接进程。入口 hash 不认证发布者，也不自动固定解释器或参数文件的供应链。

审计分为 configGeneration、catalogRevision 与 materializedArtifactDigest。用户目录 Skill 的 metadata 身份与按需正文属于分开的加载阶段；skill_load 的正文 digest 经白名单字段进入实际 receipt 和 durable SessionEntry，未读取的正文不被 metadata digest 冒充固定。项目完整 captured manifest 可固定项目正文。Manifest requires/provides 已由真实 SDK adapter、PluginManager 拓扑 Init/逆序 Shutdown 和 CLI candidate DAG 消费，缺失/循环/冲突 fail-closed。

Go/Rust 全运行负载等价性能比例没有通过本复审建立，不从 Acquire 微基准推断整机表现。
