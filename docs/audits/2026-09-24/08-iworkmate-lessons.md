# I_WORK_MATE Runtime 稳定化经验与 go-cli 可迁移项

审计日期：2026-09-24。本文只做历史源码、提交和测试代码核读；没有修改或运行 I_WORK_MATE，也没有在本轮重跑其中任何测试。提交标题和变更记录里的“通过”是历史自述，下面把它们与当前可静态确认的代码、测试分别标明。

## 历史快照与作者边界

核读时 I_WORK_MATE 位于 `codex/tokenhub-integration`，HEAD 为 `116fef5e85bc9d1715e9a8a9d71b45441b0a0f39`。工作树已有修改，审计只记录路径、不读取或披露改动内容：

- `runtime/local-agent/e2e/support/artifactTrajectories.ts`
- `runtime/local-agent/src/runtime/appContext.test.ts`
- `runtime/local-agent/src/runtime/turnCompletionReview.test.ts`
- `runtime/local-agent/src/runtime/turnContextAssembly.test.ts`
- `runtime/local-agent/src/runtime/turnContextAssembly.ts`
- `runtime/local-agent/src/runtime/turnToolInvoker.test.ts`
- `runtime/local-agent/src/runtime/unverifiedCallout.prefix.test.ts`
- `runtime/packages/knowledge/src/kb-manager.test.ts`
- `runtime/packages/knowledge/src/kb-manager.ts`
- `runtime/packages/mcp-host/src/connectorManager.test.ts`

Git 历史中作者字段为 `zhuyichen` 的记录与用户所称“朱一辰”拼音相符；仓库也出现作者显示名“朱毅臣”，并与其中一个 `zhuyichen` 显示名共用 Git 邮件身份。提交元数据不能证明这两个中文姓名是同一人的法定/首选姓名，故本文把“朱一辰”归属写作基于拼音的合理对应，不据此把其他作者的 Runtime 工作记到他名下。特别是 8 月 23–27 日的 Durable Run、Pi worker、checkpoint、租约与跨重启 replay 主干，Git 作者主要为 `fengweibo`；朱一辰的可确认贡献集中在 8 月 20–21 日的 Runtime/前端稳定修复与 8 月 29–30 日的 Pi/Harness 接缝、契约、测试和评审修正。

## 8 月稳定化做对了什么

### 8 月 20 日：从“开 6 个会话就卡死”追到连接所有权

`1f6aaf9bc`（2026-08-20，作者字段 `zhuyichen`）把症状追到 SSE reader 生命周期：多个 UI feature 各持长连接，消费方取消并不等于关闭底层 reader；服务器每 15 秒发的 SSE 注释心跳不产生 yield，因而取消后生成器的清理代码也可能一直等不到。随后每次页面重挂载都留下连接，达到 Chromium 对同源 HTTP/1.1 的 6 条连接上限时，普通请求一并排队超时。提交与变更记录把先前“只合并为单条 firehose”列为未触及资源释放根因的方案。

修复沿资源创建/释放闭环落在三个接缝：

- 客户端 reader 循环用 `finally` 执行 `reader.cancel()`，同时覆盖解析失败重连和消费方在收到事件后 `break` 的路径。可对照该提交中的 `frontend/packages/api-client/src/transport/sse.ts:61-87`；对应测试断言 break 与损坏帧重连都会 cancel reader：`.../sse.test.ts:107-150`。
- 服务端 gap 事件统一成正式 envelope，gap 时同时补发缓冲区，客户端拿到一个新的 cursor 后继续消费；负数 cursor 表示首连并全量补发。可对照 `runtime/local-agent/src/events/eventBus.ts:29-83` 及其测试；这样修的是协议双方不一致引起的重连循环，而不是单独改 UI 重试间隔。
- 同批还补了进程监督、进程退出时停止子进程、崩溃日志、fire-and-forget Promise 捕获、Run 终态守卫和工具安装路径边界；TurnManager catch 先回读 Run 状态，避免终态后清理失败把 completed 翻成 failed：`runtime/local-agent/src/runtime/turnManager.ts:74-97`。

变更记录 `[20260820-six-session-freeze-fix-batch.md](/Users/pengjunchen/Documents/projects/I_WORK_MATE/.project/changes/2026-08/20260820-six-session-freeze-fix-batch.md)` §“为什么改”“验证结果”“未验证项与剩余风险”提供复现背景、用例范围与当时的验收边界。记录称 12 项修复都先跑红再跑绿，并报告 Runtime 与前端测试通过；该记录也明确承认没有 GUI 真机回归，且当消费者取消卡在 pending `reader.read()`、只有心跳没有真实事件时，单靠 `finally` 不能立即打断读取。这是已知残余，不应因普通 break 用例通过就当作取消完整闭环。

### 8 月 21 日：继续评审邻接边界

`c604ea564`（作者字段 `zhuyichen`）称以 `git diff main...HEAD` 对 frontend 与 Runtime 做 Standards/Spec 双轴审查。除大量其他问题外，它把 SSE 的 401 无限静默重连改为不可重试错误，并补了 EventStream 监听器抛错不应终止订阅的修复。这个后续不是 8 月 20 日连接泄漏回归，而是同一稳定性工作继续沿传输错误与消费回调边界扩展。提交说明报告了测试和构建结果；本审计没有复跑。

### 8 月 23–30 日：耐久 Runtime 是多人分工，不应错记作者

之后团队把稳定性从进程内回合扩展到崩溃恢复。`8aa8f548` 建立 durable harness；`41b9051` 引入 Pi 长任务耐久执行；`ac3bea331` 增加 Pi checkpoint 幂等提交；`183be161` 把终态提交线性化；紧随其后的 `e81f3f54` 又加固 checkpoint reconciliation 与相关测试；`43089c8a` 补权威 SSE gap recovery；`144ff81a` 添加 Runtime 跨重启 SSE replay 测试。这些主干提交作者主要为 `fengweibo`。

朱一辰在这段的可确认贡献主要是 Pi/Harness 边界：

- `7edde6ff2`（H1a）先按锁定 Pi 的原生 hook 返回类型裁定 `session_before_compact` 语义，再冻结 `HarnessEvent@1`/`HarnessHandlerResult@1`、契约 fixture 与 worker 扩展装配点。提交说明称新增 105 个 golden fixture，并报告了 TL1/TL4 等测试；这些是提交说明记录，不是本次审计重跑结果。
- `66fbb11b4`（H1b）在宿主工具链外包一层观察事件，明确不重排工具执行链，并加 bus/TurnManager 行为测试。
- 同日后续 `762128982`、`9d53b6342`、`1df72c0cb` 继续修正观察回调、异步异常与诊断回调自身失败的隔离。后来的复盘明确写出：只捕获同步 throw 不足以隔离异步 reject，也必须隔离错误报告器自己失败。见 [扩展权限过宽与观察回调干扰业务.md](/Users/pengjunchen/Documents/projects/I_WORK_MATE/docs/postmortem/扩展权限过宽与观察回调干扰业务.md:42)。
- `6542b224f` 给工具完成事件暴露稳定 `receiptId`，把耐久 action 身份贯穿回执；`174001fa0` 随后补三个返回点、五字段重放属性和唯一 completed 断言。
- `18d1092df` 把 Runtime 测试迁到 Hosted 生产包装器并删除 398 行进程内测试 Runtime。迁移显出了 Hosted 路径的 abort 缓冲 flush、stopped 冻结和终态文案等缺口。教训是用生产装配形态验证核心路径，测试替身不能冒充集成证据。
- `931e47bdb` 添加 `SIGKILL → Runtime 重启 → 租约过期 → 恢复` 的无头旅程，同时修正恢复时能力快照来源，避免同步失败后的 stale 本机策略在重启时被误判为策略变化。测试代码在 `runtime/local-agent/e2e/recovery.e2e.ts:123-155` 明确断言只一条 assistant 消息、唯一终态、cursor 单调、无 gap 和恢复检查点；`158-176` 验证 stop 抢先于恢复时不再发模型请求。静态检查可确认这些断言存在，不能替代本审计对用例的执行。

团队结构规范进一步把耐久运行规则写实：单一 owner lease + generation fence、对外部副作用以幂等键和 verifier 判定能否重放、事件与终态在同一事务提交后再分发、未知写入进人工对账。可对照 `docs/desktop/本地Runtime结构与开发规范.md:650-660`。这些是 Runtime 团队后续形成的规则，不全是朱一辰个人交付。

## 失败链与仍要吸取的教训

| 证据链 | 审计判定 | 可迁移结论 |
| --- | --- | --- |
| 8/20 `1f6aaf9bc` reader 释放与 gap envelope → 8/20 用例覆盖 break/损坏帧重连 → 变更记录仍列 pending `reader.read()` 取消为残余 | 已修确定可触发的泄漏路径；没有证明所有 idle cancel 情况都可立即结束。 | 资源释放测试要把消费者退出时机覆盖到阻塞读、注释心跳、错误帧和进程关停；清理函数存在不等于它能在每个等待状态被调用。 |
| 8/30 `7edde6ff2`/`66fbb11b4` 建事件合同与可选 observer → 同日三轮失败隔离修复 → 当前结构规范记载 H1b `onHarnessEvent` 无生产注入、工具事件自产自销并退役 | 生命周期合同和观察能力本身没有闭环；测试新 bus 不证明有生产消费者，也不证明它有产品价值。 | Runtime 只为已确认的消费者实现事件/Hook 接缝；约定所有者、事件消费者、失败语义和退场门，缺消费者时应删掉无价值旁路。当前文字在 `docs/desktop/本地Runtime结构与开发规范.md:571` 与 Runtime/Harness 变更书中有明示。 |
| 8/20 gap 修复统一已知 envelope → 9/22 `36ae04500` 再修坏 JSON/坏信封：旧客户端对坏帧断线重连，带着旧 cursor 请求，服务器反复补回同一坏帧，退避封顶但 cursor 永不前进；新处理逐帧记录、跳过，能读出非负整数 seq 时推进 cursor。 | 9 月问题是同一 SSE 契约面上的另一种损坏帧循环，不证明 8/20 的 gap 根因重新出现。当前实现约在 `frontend/packages/api-client/src/transport/sse.ts:202-221`，历史实现与测试用下面列出的 `git show` 固定 SHA 复核。 | 验证重连不止网络断线和显式 gap，还要模拟损坏帧、部分 seq、重复/乱序和游标停滞，并断言下一次请求 cursor 与帧消费结果。 |
| 9/21 `bf061bffc` 审查发现首字节 idle 超时没有实际守卫；删除后已有 87 条用例仍全绿，而行为从 60 秒终止变成永远不结束。另一个“与 SSE 同口径”的用例只和字面量比较，改常量仍全绿，后来改成两个实际常量对账。 | 测试通过数字和测试数量无法证明判据有效；这些断言先前并不观察目标行为。 | 每个回归用例要给出能让旧行为失败的反例，并断言实际请求、cursor、终态或耗时；用例名称、注释和固定常量不算行为证据。历史细节见 `20260921-w2844-bytes-renderer-idle` 变更记录和 `测试假绿与门禁证据失真.md:50-66`。 |

跨层经验与源代码审计要保持边界：8 月变更记录声称全套 unit/build 通过，但同时标注真实 GUI 回归未做；9 月 postmortem 也明确多项历史测试仅静态核读。因此本轮只能确认提交、实现、测试代码与报告的验证范围，不把 commit message 或旧报告升级成当前可复现的测试结论。宿主配置、关闭屏障、父子进程寿命和空闲 SSE 的复盘见 [执行入口旁路与宿主生命周期失控.md](/Users/pengjunchen/Documents/projects/I_WORK_MATE/docs/postmortem/执行入口旁路与宿主生命周期失控.md:49)。

## 映射到 go-cli Runtime

go-cli 已有可复用基础：`RunTurn` 使用 `context.WithCancel`，并对配置的 RunSlot 执行单回合 claim；EventStream 有限缓冲、背压和显式 close；JSONL SessionStore 提供会话记录落盘。[turnrunner.go](/Users/pengjunchen/Documents/github/go-cli/internal/core/turnrunner.go:77)、[run_slot.go](/Users/pengjunchen/Documents/github/go-cli/internal/core/run_slot.go:22)、[eventstream.go](/Users/pengjunchen/Documents/github/go-cli/internal/core/eventstream.go:23)、[jsonl.go](/Users/pengjunchen/Documents/github/go-cli/internal/session/jsonl.go:63)

若 Runtime 目标包括跨进程恢复，现有机制需要明确各自承诺：默认 [TurnSnapshot store](/Users/pengjunchen/Documents/github/go-cli/internal/session/snapshot.go:20) 是进程内 map；[IdempotentCache](/Users/pengjunchen/Documents/github/go-cli/internal/production/idempotent.go:11) 是会淘汰旧键的内存缓存；[RunSlot](/Users/pengjunchen/Documents/github/go-cli/internal/core/run_slot.go:112) 的零配置实现是 no-op；[EventStream](/Users/pengjunchen/Documents/github/go-cli/internal/core/eventstream.go:39) 是内存队列。但生产组合根 [AssembleAgent](/Users/pengjunchen/Documents/github/go-cli/internal/cli/assemble.go:1275) 创建一个共享的 `DefaultRunSlotGuard`，并将它同时注入 Harness 和 TurnRunner（`:359`、`:367`）；因此这里指出的是直接构造零配置时的 fallback 语义，并不表示生产主入口缺少进程内互斥。这些进程内机制能支持同一进程内的回合协作或去重，不能单独证明崩溃后会话、运行中工具动作、事件 cursor 和终态可恢复。若 CLI 产品决定不承诺运行中断点续跑，则应清楚呈现终止和重新提交语义；若承诺恢复，则要增加持久执行状态与证据链，不能把保存了对话 JSONL 等同于恢复了未完成的副作用。

建议把 Runtime 迭代拆成以下验证门：

1. **资源与终态。** 为每个 goroutine、stream、LLM 请求、工具进程登记创建方、取消信号、关闭方与 join/drain 等待。验证消费者 break、context cancel、阻塞 provider read、工具进程退出和 terminal 后无迟到事件。观察/trace sink 的同步或异步失败都不能改写工具结果，也不能在错误处理器失败时留下 goroutine 泄漏。
2. **Run 与事件合同。** 统一 Run/turn ID、事件身份和终态；定义缓冲满、流断开、首连、gap、坏帧、重复和重排的行为。跨重启游标应来自耐久序列或 generation+sequence；不能只用墙钟时间来规避进程重启碰撞。`EventBus` listener 抛错不能阻断其他订阅者或反向改变 agent 终态。
3. **副作用和恢复。** 若确实需要续跑，先持久化 Run owner/generation、工具 action intent、稳定 action ID 与 checkpoint；动作结果回执落盘后才能发终态事件。只读动作可自动重做；写文件、shell、MCP 等需要幂等键和执行后 verifier 才能安全自动对账。无法判断外部副作用是否已发生时，应停在明确的人工对账状态，不盲目重跑。
4. **真实 Runtime 崩溃旅程。** 用真实 CLI 子进程和确定性假模型/工具故障注入，验证写 action 前后、回执前后、checkpoint 提交前后、发送终态前后的硬退出；重启后断言无重复副作用、最多一个有效 owner、一个终态、稳定事件顺序与 cursor 恢复。另覆盖 pending read 取消、坏事件恢复和观察窗口。保留单元测试验证纯函数，进程级旅程验证协议与持久化接缝；两种证据不可互换。
5. **做减法与证据收尾。** 在每个 slice 先定问题和不可变条件，再实现最小修复、针对旧行为的回归断言、独立复核与适用的集成验证；删掉没有消费者的私有帧、in-process 测试假 Runtime 和无引用导出。把测试代码已增加、命令实际通过、进程级旅程通过、交互/真机验收分别记状态。

这些经验适合迁移为 Runtime 的所有权、幂等、恢复、测试和评审要求；I_WORK_MATE 的前端 SSE/SQLite/Pi worker 组合本身不应直接搬进 CLI。go-cli 是否引入持久 Run journal、跨进程 owner lease、Pi 或另一个 Harness 引擎，须由总审计中的产品目标与既有循环架构决定。

## 复核入口

以下命令可在当前 I_WORK_MATE 检出目录重现关键历史对象；路径与行号引用 8 月内容时，以提交 SHA 的树为准，当前文件链接仅用于快速定位：

```bash
git -C /Users/pengjunchen/Documents/projects/I_WORK_MATE show 1f6aaf9bc:frontend/packages/api-client/src/transport/sse.ts
git -C /Users/pengjunchen/Documents/projects/I_WORK_MATE show 1f6aaf9bc:frontend/packages/api-client/src/transport/sse.test.ts
git -C /Users/pengjunchen/Documents/projects/I_WORK_MATE show 1f6aaf9bc:runtime/local-agent/src/events/eventBus.ts
git -C /Users/pengjunchen/Documents/projects/I_WORK_MATE show c604ea564 --stat
git -C /Users/pengjunchen/Documents/projects/I_WORK_MATE show 7edde6ff2 --stat
git -C /Users/pengjunchen/Documents/projects/I_WORK_MATE show 66fbb11b4 --stat
git -C /Users/pengjunchen/Documents/projects/I_WORK_MATE show 18d1092df --stat
git -C /Users/pengjunchen/Documents/projects/I_WORK_MATE show 931e47bdb:runtime/local-agent/e2e/recovery.e2e.ts
git -C /Users/pengjunchen/Documents/projects/I_WORK_MATE show 36ae04500 -- frontend/packages/api-client/src/transport/sse.ts frontend/packages/api-client/src/transport/sse.test.ts
git -C /Users/pengjunchen/Documents/projects/I_WORK_MATE show bf061bffc -- frontend/packages/api-client/src/transport/http.ts frontend/packages/api-client/src/transport/http.test.ts
```
