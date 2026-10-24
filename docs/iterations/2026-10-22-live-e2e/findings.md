# 真实交互验收中的集成发现

> Retention note (2026-10-24): raw evidence JSON and screenshots were removed after the findings and report retained the observed failures, repairs, results, and limits.


日期：2026-10-22。最终状态：原始 `make verify` 与 `make test-e2e` 均已通过。前者 9036 started / 9030 passed / 0 failed / 6 skipped，31 包，536.385s；后者 4907 started / 4901 passed / 0 failed / 6 skipped，8 包，178.945s。1245 个源码文件的 manifest 未变（SHA `4f04c8f51ab0000a7ee9b2302835d671220a21d70bfe80b0c157297c5906ecfc`），最后的测试隔离修正未改变实际生产 binary `1e74`。真实旅程按各 binary 分别记录，不能拼接成全部属于最终 SHA 的证据。

全部 22 个 CLI action 与前端已关闭；已精确移除两个自有 trust key、四个 authority 文件和六个自有 ignored 日志。最终 scoped 扫描覆盖 2141 个目录，匹配的自有测试 authority 残留为 0，trust 自有前缀残留为 0。主代理已实际执行严格的连续尾部清理：history 删除 8 行/64B 后为 3872B，audit 删除末尾 20 行/7260B 后为 124841B；较早六条不确定 audit 记录共 2118B 保留。两者保留前缀均经校验未变，没有恢复时间戳；不公开个人摘要或 inode。主代理已在全部自有进程退出后精确删除原始任务临时根目录，源码、报告与24张截图保留，现有Magpie未停止。

以下正文保留各历史阶段当时的失败、中止与待验状态；当前结论以上述最终验收为准。前期单元测试、mock 模型和非 durable Skill 旅程未覆盖正式 interactive 的完整 durable Skill 路径，这一遗漏由后续真实交互与对应回归补齐，不能用上轮 gate 代替其证据。

## 已实现并通过定向验证的修复

- 项目授权撤销后，`VerifiedProjectSources.Valid()` 仍可为真：它检查 manager 所铸证明的形状和 seal，不读取最新 trust store。动态 run admission、工具 admission/completion、Skill 搜索/正文加载以及显式终端加载原先误把它当作当前授权。现在复用 `CommitIfCurrent(ctx, no-op)` 读取现有持久 authority；新 run 拒绝时返还 lease/gate，已发生调用的迟到结果被完成 fence 拒绝，新 grant 也不复活旧 plan。
- pinned prompt 在授权失效时返回固定的安全拒绝文本，不再构建该项目的私有指令。resolver provider 的每次候选授权在 operation mutex 之前检查 run authority，覆盖初始、重试、failover 与 continuation；保持 one-shot permit、原预算和取消分支。授权是 admission 线性化边界，不声称并发稍后的撤销可以撤回已发出的外部动作。
- `CheckRunGeneration(ctx)` 优先使用可选 `CheckContext(ctx)`，旧 `Check()` 实现兼容保留，避免当前调用的取消信号在进入持久文件锁等待前丢失。新增回归专门覆盖独立 manager 持有同一文件锁时的取消；不声称可以抢占底层文件 I/O。
- `skill_search`/`skill_load` 原先通过普通 trusted control 注册，没有 durable 执行要求的本地完成证明。新增明确的宿主 `RegisterTrustedInlineControl` 声明并经原 deferred/middleware wrapper 转发；只供已核对同步完成、返回后无未跟踪工作的宿主控制器使用。两个 Skill 控制器完成读取/解析后同步返回，脚本仍须独立正常工具调用。原 `Register`、原 `RegisterTrustedControl`、SDK 和按工具名决定权限的行为都没有放宽。

新增测试使用真正的临时 FileTrustStore 和两个独立 manager，覆盖撤销后新 run/Skill load 拒绝、旧 grant 不复活、模型完成结果不外泄、候选授权锁外检查及取消。正向回归通过真实 Assembly、localhost guarded provider、受保护 shell hook、Skill 控制器和 durable sink 连续运行两轮，检查物理会话身份、HTTP 调用和 hook side effect；测试只创建并清理自己的唯一 workspace authority ID。统一验收的 default、headless 两组均运行 21 个测试函数、包含子例共 57 项，race 全部通过；两组四包 lint 均为 0 issues。日志为本轮临时 `logs/targeted-live-default-race-4.log`、`targeted-live-headless-race.log` 及对应 lint 日志，保留早期测试字段与私有目录权限 fixture 错误的原始失败记录，归档后由主代理精确清理。此结果不替代真实 Magpie/UI 旅程。

静态复核未发现 trust-store 重入：prepared 的 `CommitIfCurrent` 回调只做 fence/commit 标记，返回并释放 store 锁后才调用 executor；工具完成 fence 和 session terminal fence 在该事务之外。provider 的 store 检查也在 authority mutex 之外。正向 durable 回归用于防止这条合法路径挂起，不能以静态判断替代其运行结果。

## 首次真实 UI probe 的显示发现

主代理在真实 L01 probe 中成功运行 ls、grep、read、find。只读核对自有 `ask-interactive-e4089f9b.pty` 和 physical session `s_693627913ad2bf79621a3b34430c55e2`：三条 assistant proposal 正文为空但分别带 grep/read/find 的 ToolCalls；canonical 和 live 正文投影无条件产生标题，形成空 `[assistant]`/`[assistant · live]` 行。这是实际投影行为，不能归因于旧 resize。窄修复只隐藏已确认的 tool-only 正文行，保留原 canonical occurrence、ToolCalls、执行状态和 receipt；未确认的空 streaming 占位、真实空结果、错误和已有流式正文仍可显示。过滤正文后，工具 metadata 通过 durable EntryID 精确找到投影行，避免原 source 数组下标错位。三个新语义回归覆盖单双栏、live→确认、执行/unknown receipt 与刷新关联，已纳入后续统一定向验收；原 probe 不因此被重新标记为修后证据。

最终正文缺失 closing backtick 已存在于该自有 session 的第 22 行 canonical Content，PTY 如实显示；provider 请求返回 200、finish reason 为 stop，未见截断证据。本轮保留为模型输出现象，不更改正文或核心累积逻辑。143 列双栏的原始输出行宽保持 143 cells，英文单词跨硬换行属于现有显示策略，未发现中文越栏的证据。原 probe 和修后新版旅程须分别保存。

## 正式拒绝流程暴露的 durable 阻断

新版真实 ask 进程的 L01 完成后，L02 write 弹出完整审批。主代理选择 n，未产生文件副作用，但 REPL 报 `durable run finished without positive transcript publication` 并退出。自有 physical session `s_d7d09140e9621a78d41c4f2c67405c26` 中，失败 run `6a99abefeb3694f3` 的第 24–26 行分别为 user、assistant proposal、`not_dispatched/error` tool receipt，没有 run_terminal；真实审批审计为 callback_deny。等待人工选择约 72 秒，但 callback 未记录 deadline/callback_error，不能把等待时间直接归因于超时。精确 workspace authority 中八个 dispatch marker 已被八个 resolution 配平，L02 未产生 dispatch marker；本轮保留旧失败行，不手工追加 terminal、删除 marker 或伪称完成恢复。

确定的源码冲突是 `publishPreparedRevocation` 为一次拒绝提升了 `DispatchFence.Approval`，动态 guard 又固定 run 开始的 Approval/Plan，因此连已确认未派发的 receipt 事件、下一模型请求和 terminal publication 都被当作整轮策略撤权拒绝。现新增 `DispatchFenceDecision` serial：一次明确拒绝仍使所有旧 prepared snapshots 失效，并清理缓存；它不冒充真正的 permission policy 变化。mode、可复用授权 store 撤销与 Plan 变化继续提升原 Approval/Plan 域，仍拒绝旧 run 和迟到结果。完整 prepared tuple 比较保留 Decision。`RunGenerationMetadata.PolicyEpoch` 属配置 snapshot，审批内部 `policyEpoch` 属缓存 key 版本，两者均不能替代 mutable prepared serial。

取消后的既有 durable bookkeeping 使用 owner 原有、有界的 CleanupCommitContext，再检查当前 persisted trust；不使用这个 context 派发模型/工具。取消在下一次模型 admission 前被正常识别，明确未派发的审批取消可持久化 interrupted receipt，不能由已取消的 store 检查自动误记成 unknown。Pre-tool hook 已知拒绝补发 terminal tool_result。未知远端副作用、未 join 的本地资源、真实 trust/policy 撤销仍保留原 fail-closed 边界。

另一个明确的取消窗口是 preparation 已完成、sealed invocation 尚未跨 final commit 时取消。只有 Dispatch 已返回、`DispatchCommitted=false` 且 capability completion error **恰等于**当前标准 `ctx.Err()`，才保留 known interrupted 结果；不使用 errors.Is 接受带有其他 authority 原因的组合错误。真实 executor 零次进入的 barrier 回归与 commit 后取消、组合 authority 错误的反例已在下述统一窗口执行。不能把这个前置边界推广成已派发外部动作的取消成功保证。

新增 actual Assembly + dynamic guard + 临时 FileTrustStore + durable sink 连续两 run 回归，分别覆盖快速 ask deny、auto、deny policy、受保护 shell hook deny、ask cancel 后迟到 allow、实际 deadline 后迟到 allow；核对正向 publication、精确 not-dispatched receipt、无拒绝副作用和相同物理会话后续 run。另保留真实 Approval/Plan 的 in-flight fence，以及明确拒绝作废旧 prepared sibling 的对抗断言。统一窗口 default/headless 每组 84 个测试函数、含子例 152 项 race 全部通过，两个 tag 六包 lint 均为 0 issues；三个提交前取消 barrier 子例实际通过。该 source 对应 race binary SHA `43d47fb20169ff317377b6d7982090df65a698aea20395b6ed575f3fcdd25b8d`，不覆盖后续 D02 parser 修改。前期成功工具路径、直接 adapter 审批测试没有覆盖这一整条真实 durable 拒绝链，这是本轮真实交互才暴露的集成缺口。

## D02：Skill-only 刷新被未变的 JSON hook 拒绝

真实动态进程 PID 94071 的 D01 已完成 skill_search、skill_load、read。随后 helper 的 skill-v2 只改变 SKILL.md；未 grant 前 `/reload` 正确 needs-trust。正常审阅并 grant 新 manifest 后，原进程 `/reload` 却报 `cli: hook configuration: yaml: invalid mapping line "{"`。hook 文件 SHA 始终为 `7b162276d1816152aec9f7fa2a43c649e1287aff1c7dfdc02c291cfbc1e12dec`，config 字节亦未变，不能修改 fixture 格式掩盖失败。

精确原因是启动的 `loadShellHooks` 按 trim 后前导 `{` 选择 JSON，而刷新 `validateRuntimeHooks` 强制 YAML。现在两个 consumer 共用 `parseShellHooksConfig`，只统一既有格式识别，不扩展 YAML 语法，不改变启动原有 warn/skip 行为；刷新依然验证 captured verified bytes、hook event、post_tool_use 不支持以及 timeout 必须为正且不超过一分钟。

回归先写源码再实现：真实 Assembly 从已 grant JSON hooks.yaml 启动并执行 hook，只有 Skill 文件改变并重新正常 grant，同一 Assembly 刷新后再次执行相同 hook；检查 config/hook 字节完全不变、generation 更新及两次真实本地 hook side effect。第二组断言 JSON/YAML 等价、非法 JSON/event/post hook/timeout 仍拒绝。测试只使用临时 FileTrustStore，HOME 不重设，保留完整 `LoadTrustedWithManager` 签发的私有来源证明，仅把公开执行配置限定为 verified 临时项目设置。首轮 fixture 返回脱离原配置的 overlay，导致 MCP provenance 检查拒绝，原失败日志保留；修正 fixture 后 default/headless 两组各 6 个测试函数 race 全通过（2.242s/1.910s），两组 CLI lint 均为 0 issues，没有放宽生产 guard。此结果不替代真实模型/UI 复验；D02 原始失败与后续 stale run 保留，不记为成功。

后续 `1e74d58…` 的真实动态复验已在同一 PID 96955、同一 physical SID 完成 Skill V1→V2、Hook-only V2、非法候选拒绝及恢复、V3 阻止 read，并在真实 revoke 后于 provider admission 前拒绝新调度。完成阶段共 20 个实际 HTTP 请求；invalid/revoke 没有新增请求。这里只按该版本更新实测事实，不将旧 `43d47…` 的失败会话改记成功，完整阶段证据见 [真实 E2E 报告](report.md)。

## 完整 gate 暴露的旧测试与存储隔离问题

原始 p1 `make verify` 保留了两类失败：15 分钟 timeout，以及 `TestToolInterceptor_CancelsBeforeExecution` 仍要求取消后没有 `tool_result`。后者与本轮有意补齐已知未执行的终态 receipt 冲突。仅收紧旧测试：继续要求 executor 零次进入和唯一取消通知，另要求唯一、精确关联 `c1` 的 error/interrupted 结果、固定取消正文、没有派发 `tool_call`，以及 canonical receipt 的 `danger`/`c1` 身份和 outcome 一致；没有回滚生产终态事件。该 case 的 default/headless race 均通过（1.774s/1.637s），原失败与 timeout 不被删除或改记为通过。

完整验收期间还观察到个人 history 增加 64B、动态旅程 audit 增加 9378B。尾部归因与源码核对将 history 的输入序列关联到 `TestPendingInput_REPLIntegration`、`TestInteractiveCmd_RunConversation` 的 inline config；两者没有 `History.Path`。动态 fixture 未配置 audit 路径；actual edge entry fixture 同样继承 audit 默认启用却未设置路径，其三次工具调用与三轮 gate 中的九条 `main` audit 记录形成来源和执行顺序上的强关联，不作为独立时钟证明。没有为归因读取无关个人正文，也没有由本次修复截断、重写或清除个人文件。

最小隔离改动仅在测试：三个共享 helper 显式设置临时 `Memory.StorePath`、`History.Path`、`Production.Audit.Path`；原 `Audit.Enabled`、Git、sandbox 和权限语义保留。E2E helper 以显式路径替换 HOME 覆盖；已确认的两个 inline case 和 ExitCommand/EmptyInput 复用隔离配置。两个 actual cmd entry fixture 同样只补三个回退路径，原空 `Session.StorePath` 保持空，不引入 durable 路径或额外 authority。内部 CLI focused 验收 default 37 项、headless 36 项，以及两个 actual cmd entry case 的两 tag race 均通过；对应个人三个 store 的测试前后 metadata 保持不变。该定向集合仍漏掉 MaxTokens 的两个真实入口子例，不能由此推断整个 gate 不写个人文件。

随后 p2 被观察到的 history mtime-only 变化中止：`TestInteractiveCmd_MaxTokensFlag` 的局部 makeCfg 仍构造空路径，即使 exit 被去重，History.Save 也会原样重写文件。本次仅将 makeCfg(t) 改为隔离 helper，保留两个 flag 子例和全部断言；额外明确授权的 main trace 入口保留既有 Session/History，只补 Memory/Audit 两字段。已一次穷举当前源码的全部 33 个 `newInteractiveCmd` 调用、31 个测试函数及四处直接 slash struct 构造，详见 [入口覆盖表](test-entry-isolation.md)。这次完整入口集合的双 tag 验收和后续原始 gate 仍须实际执行；生产代码与 binary `1e74` 未因此改变。

## 本轮明确保留的边界

MCP discovery 与远端 MCP 执行仍保留原 unknown effects、正常审批、精确 generation lease 和完成证明要求。发现其 durable 路径存在尚未声明本地完成证明的缺口；本轮不把 unknown 改成 control，不按名字赋予证明，不增加全局默认，也不宣称 MCP durable discovery→call 已修复或已验证。无法证明完成时继续 fail closed。此项与当前 basic tools、审批/Skip、动态 Skill/Hook 的真实 UI 验收分开记录。

## 真实证据要求

临时 fixture 的随机 canary 只出现在 Skill 正文，预期 JSON 放在模型 workspace 外，不写入 metadata、配置、hook 输出或用户 prompt。成功 `pre_tool_use` hook 的 stdout 原本不会显示在终端，因此用受限的项目内事件文件证明实际调用；拒绝型 hook 同时核对真实工具 receipt、文件未改变与 `core.hook.deny` 日志。

`/skills search` 是 metadata 查看；`/skills load` 会把正文作为待执行用户输入并启动模型，不能当作只读命令。未重新批准的新文件必须先令 `/reload` 拒绝并保留旧 generation；随后只为本轮唯一 canonical workspace 执行真实 grant，再回到同一交互进程 `/reload`，下一 run 才应使用新 Skill/Hook。真实 UI 截图由主代理直接捕获；fixture 输出或 PTY 转录不能冒充截图。

失败保旧的验收语义需要修正：保留的是旧 generation、快照和资源所有权，不能保证旧授权永远可执行。正常 grant 一个随后在 catalog 编译阶段失败的候选会改变当前 trust epoch；`/reload` 必须不部分发布并保留旧 generation，但旧 plan 此时应因 authority 已失效而拒绝执行。恢复合法源、重新 grant 并 `/reload` 后才可继续。临时 `invalid-skill` 仅把既有正文入口的 frontmatter name 改为合法 YAML 字符串 `live/check`，正常 capture 读取字节、catalog 单独拒绝斜线；精确 `restore` 不改 config/hook。后续真实 R04 已观察到候选拒绝、generation 保留，以及恢复/regrant/reload 后同进程完成任务；没有在非法候选阶段提交模型 prompt，不能外推为旧授权仍可调度，也不能为“旧计划继续跑”绕过当前授权。

Skill 和 Hook 的热更证据分步采集：`skill-v2` 只改 SKILL.md，`hook-v2` 只改 hooks.yaml，`v3` 只将 hook 改为拒绝 read，后两步保持 Skill v2。转换记录保留各文件摘要，原 config/storage 字节固定，避免无关冷配置变化造成要求重启。

个人 HOME、provider 配置和凭据不被重写。Memory、session、history 明确指向任务 workspace；正式 binary 使用原 trust-store 路径并只操作本轮唯一项目 key。durable authority 位于标准 user config 的 `go-cli/workspace-quarantine`，按本轮 canonical root 的 domain-separated workspace ID 精确记录和清理，不能删除共享 authority 目录或整个 trust store。
