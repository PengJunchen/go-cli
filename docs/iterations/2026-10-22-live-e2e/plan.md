# 真实模型与统一终端体验验收计划

本文件是设计阶段与 fixture 准备阶段的历史计划快照。正文中的“尚未执行”“等待验证”和未勾选 AC 只表示当时状态，不代表本轮当前结果。实际操作、失败与修复、binary/SHA 及最终验收以[实际报告](report.md)和其中关联的证据为准。

日期：2026-10-22。起点：独立worktree、HEAD `cd0693b`。设计阶段状态：设计/静态审查完成，统一approval入口已实现并写回归case，等待本轮功能齐后统一验证；当时本designer尚未启动测试、真实模型或CUA。本文件不把旧mock、旧live或静态接线当成本轮成功证据。

## 目标与执行顺序

使用本机Magpie与真正可用的6.1 Sol路由，在同一个owned PTY经xterm.js/IAB呈现的完整REPL观察基本工具、审批allow/deny、显式auto、动态Skill和hook刷新。只在隔离临时workspace内修改验收fixture；上游模型可用性必须由本轮真实推理证明，模型目录或显示名不足以证明成功。记录实际provider/model/backend路由、版本与响应ID/请求计数，不输出认证材料。

先完成获准的最小生产修复、可先写回归case；功能齐后统一执行回归/build，再开始真实模型旅程。主代理分配临时目录并拥有启动、截图、停止、清理。此designer不启动测试或CUA、不动原dev/保护文件、不提交推送。quota任一窗口剩余<25%立即停止并保存WIP，清理由主代理协调。

## 已确认接线与最小修复

- 起点审查发现 `repl_session.go` 的parseFlags没有 `--approve`；setupAgent在TTY总传approval channel；`assembleApproval`优先channel并强制ask，`config.approval.mode`未真正消费。prompt已有 `--approve`，但未区分显式ask与默认ask。因此原prompt auto不能替代“同一个完整REPL UI中的auto”验收。本轮修复上述接线，等待真实执行验证。
- 最小方案：两入口共享优先级 `显式 --approve > config.approval.mode > ask`，仅ask/auto/deny。源码确认旧prompt零值本就默认ask，因此没有把旧auto默认改成ask。所选非法模式在内建interactive/prompt的解析及Assembly资源装配之前拒绝；main配置/trace准备可能已发生，未知扩展命令的bootstrap不属于本项承诺，不扩通用启动系统。只在ask+TTY接modal；auto/deny仍走同一审批middleware、audit、prepared fence、trust及sandbox，不新增第二套UI或旁路执行。REPL启动显示实际模式，帮助列出flag。
- `/skills load <name>`返回pendingInput，REPL会立即把所选正文作为新用户输入提交模型；它不是只预加载、等待下一条输入。普通 `/skills`、`/capabilities`、`/reload`结果直接写REPL输出，目前不是Bubble Tea工具/审批卡片；样式一致性必须实际观察。
- `/reload`只在idle切换数据计划。项目Skill/hook新bytes必须先获新trust grant；不能把拒绝后的旧代当作成功刷新。fixture使用lazy MCP且无服务器、extensions关闭，避免eager来源或已有project owner要求restart的额外条件。
- `pre_tool_use` hook支持非零exit阻断；普通超时/启动错误依既有合同放行，不把它设计成失败关闭安全sandbox。本轮用明确exit 1验证阻断，不用超时冒充deny；不使用已明确不支持的prepared post_tool_use。
- unknown scope审批保留once-only及完整细节review约束，不添加always-allow快捷绕过。外部trust撤销后的真实current-store fence由runtime builder处理并另测，不靠历史digest恢复权限。

## 隔离前提与命令

以下为待执行命令模板，`BIN`、`WORKSPACE`、`PROVIDER_CONFIRMED`、`MODEL_CONFIRMED`由主代理填入真实值；不是已经执行的记录。Magpie账号保留在其原安全位置，CLI只使用主代理已建立的最小受控连接配置，不在命令行、plan、截图或提交文件中填写key。禁止重设HOME或改共享全局配置。

```bash
cd "$WORKSPACE"
# 先通过真实TTY审阅冻结manifest，准确输入TRUST。
"$BIN" trust grant

# 新flag修复后，三个会话均用同一个完整REPL入口/owned PTY。
# 受控launcher仅注入允许的provider连接与workspace/session路径，不改HOME。
"$BIN" interactive --no-onboarding --provider "$PROVIDER_CONFIRMED" --model "$MODEL_CONFIRMED" --approve ask
"$BIN" interactive --no-onboarding --provider "$PROVIDER_CONFIRMED" --model "$MODEL_CONFIRMED" --approve auto
"$BIN" interactive --no-onboarding --provider "$PROVIDER_CONFIRMED" --model "$MODEL_CONFIRMED" --approve deny
```

仅任务workspace内的精确项目配置启用 `runtime.dynamic=true`、`skill.progressive=true`、`mcp.loading=lazy`、无MCP server、extensions关闭，provider沿真实安全接入。项目配置/Skill/hook全部在workspace，任何变更重新捕获并在另一个owned PTY运行同一 `trust grant`。只操作本任务canonical root对应的精确trust key，结束由主代理revoke/清理该key；不覆盖其他store条目。禁止用写trust store、脚本代按批准或预生成模型回应替代用户决策。本轮native Terminal的CUA能力被安全策略拒绝；采用真实binary+owned PTY+xterm.js网页，由CUA IAB操作/截屏。截图统一标“真实PTY浏览器终端，非原生Terminal窗口”，报告保留此限制。

本轮补入最小隔离字段 `memory.store_path`，可信项目可在capture/grant前指定绝对任务workspace文件，`session.store_path`与`history.path`同样指定任务目录。显式memory路径失败只保留原有不可用诊断，不fallback个人文件；空值保留旧 `~/.go-cli/memories.jsonl` 默认。该路径是Assembly生命周期字段，变更需要重启，不在/reload中迁移。示例由launcher填真实绝对路径，不修改HOME或共享全局配置：

```json
{
  "memory": {"store_path": "/absolute/task/workspace/memory.jsonl"},
  "session": {"store_path": "/absolute/task/workspace/sessions"},
  "history": {"path": "/absolute/task/workspace/history"},
  "approval": {"mode": "ask"}
}
```

`approval.mode`和`--approve`均接受ask/auto/deny，显式flag优先。启动行 `Approval mode: ...`显示实际值；full ask在TTY用同一modal，headless ask维持同步callback，auto/deny两构建均不等待modal且仍经过trust/sandbox/prepared审批middleware。实际行为待统一验证，不用本段示例冒充已运行。

## 最小真实用户矩阵

每行至少记录一次真实模型run、可关联的request/response/工具receipt、文件前后hash及真实PTY浏览器终端关键画面；slash纯本地步骤另明确无模型请求。模型自行选择工具，若未调用目标工具则该项记未覆盖并以更明确真实提示重试，不用mock补齐。

| ID | 操作/提示 | 必须观察 | 成功条件/截图点 |
|---|---|---|---|
| L01 基本工具 | ask会话：`/help`、`/tools`；要求列目录、grep nonce、read输入，再write新文件、edit一个数字、read核对；auto会话另用bash只运行隔离目录内的无害printf | 实际ls/grep/read/write/edit各自call ID、审批、receipt与真实文件；bash成功覆盖放auto，ask已有策略直接禁止bash | 工具参数/结果可展开；42等唯一结果确实落盘；不会把不同tool-only响应误判loop。截图工具列表、一个执行详情、最终结果 |
| L02 ask deny→allow | 要求写入 `denied-NONCE.txt`；在完整modal审阅参数/diff后按n。下一轮要求不同新文件，再按y | deny receipt、拒绝文件始终不存在；allow一次后文件准确存在 | 同一UI保持可继续输入，拒绝不显示成功/空白卡；y仅批准当前已review request。截图modal、拒绝行、批准完成行 |
| L03 auto | 重启同一REPL入口 `--approve auto`，重复write/edit/read任务 | 启动effective mode、无modal、实际副作用/auto审批审计；trust/sandbox仍有效 | UI与ask共用布局/工具状态，只有审批策略不同；拒绝trust的项目能力不能因auto放行。截图模式与成功工具 |
| L04 deny/优先级 | `--approve deny`要求写临时新文件；配置auto但flag ask、配置deny但flag auto分别做最小核对；非法flag/config在启动拒绝 | 实际effective mode、deny无副作用；非法值在内建入口的Assembly/model/tool dispatch前拒绝 | flag覆盖config，未指定flag消费config，空配置仍ask；不产生意外第二套非TUI路径 |
| L05 Skill动态 | 启动时仅Skill V1；`/skills search live`、`/skills load live-proof`，正文要求模型回答秘密fixture nonce。外部改V2后先 `/reload`，再TTY重新grant、`/reload`、`/capabilities`、`/skills load live-proof` | 未加载前仅metadata；加载后本轮真实模型收到正文；未批准V2拒绝、旧generation保留；grant后新nonce/新generation | 输出与状态明确“下一run生效”；V1/V2 nonce来源可证明，辅助references不被注册为Skill。截图目录、needs-trust、成功reload、V2模型结果 |
| L06 hook动态 | 初始project pre_tool_use hook追加HOOK_V1 marker并exit0；模型做read。外部把捕获的inline hook改为追加HOOK_V2并exit1；先reload拒绝，再重新grant、reload、要求read | hook真实shell副作用、V1工具正常；未trust的新hook未运行；V2阻断receipt且实际read未执行 | 同UI清楚区分“用户拒绝”与“hook阻断”；无假工具成功；截图reload与阻断卡。marker由独立文件检查，不凭模型口述 |
| L07 刷新失败保旧 | 在已批准配置中构造确定非法hook event/timeout或超过prompt预算，重新grant后 `/reload`；随后恢复合法、grant并reload | 候选错误且旧generation保留供诊断、新候选没有被部分发布；新grant已改变trust epoch时旧authority也应拒绝执行，恢复合法来源、重新grant及reload后才恢复可用 | 错误为可读反馈，不破坏输入/残留modal；不得绕旧authority安全拒绝，截图失败与恢复结果 |
| L08 取消与UI一致性 | 审批modal中Esc取消，随后短问答/工具；在120×36与较窄窗口观察同一组状态 | 取消后无迟到写入，输入恢复；完整review不足时y仍锁定；不是取消即成功 | 不出现第二个readline审批、裸ANSI/日志穿插、状态栏覆盖modal、过期请求误批准、最终成功图标掩盖拒绝 |

L05/L06必须分开改变fixture，避免一次grant更新两种来源导致归因不清。配置预算、provider或模式不是热换示范；若触发restart-required应保留真实错误，不改成表面刷新成功。hook receipt marker可写workspace普通输出文件；不要把未捕获的可变脚本当作已冻结hook内容。

## 统一UI观测与判定

同一xterm.js页面/字体/窗口尺寸拍摄ask、auto、deny、Skill load、reload成功/失败与hook阻断，比较：模式可见性、输入焦点、工具名/路径、diff/参数细节、pending/running/succeeded/denied/cancelled状态、可读错误、底部提示、完成后继续输入。异步工具区使用Bubble Tea而slash输出是普通文本，先记录实际混排/滚动/覆盖再决定最小修复，不仅凭源码统一样式。

需要至少以下证据字段：源码与binary hash、构建flags、provider/model实际路由（脱敏）、每run/turn/request/call ID、审批模式/选择、generation与catalogRevision、fixture nonce与前后hash、UI画面来源（真实PTY浏览器终端，非原生窗口）、成功/失败/未覆盖及原因。保存失败截图；每个artifact来源可核对。Magpie透明记录只用于关联，不能替换真实响应；截图/报告不展示keys、session token、认证头、个人目录内容或完整系统prompt。

旧10/14 live发现过不同tool-only响应被loop guard误判、provider-local timeout与durable outcome不一致导致UI退出、grep/find完成归属不一致。本轮只复用失败形态作为观测点，不引用旧成功数替代新模型/新源码验收。真实性、UI、工具副作用、信任与owner清理均需交叉核对。

## 当前AC与待执行状态

- [ ] A1 统一approve flag/config/default与启动模式展示完成，回归case已写，功能完成后统一运行。
- [ ] A2 真Magpie+可用6.1 Sol完成L01–L04，同真实PTY浏览器REPL展示ask allow/deny及auto。
- [ ] A3 L05/L06以真实模型、重新grant和实际nonce/副作用证明动态Skill/hook生效。
- [ ] A4 L07/L08证明错误/取消不误报成功、不部分发布、能继续交互，相关UI一致性问题闭环。
- [ ] A5 实际截图报告经独立审查，失败与未覆盖保留，证据脱敏、进程与临时资源按主代理策略清理。

本文件仅计划。主代理临时目录、最终工具/模型路由、真实操作结果与图片清单待主代理执行后填写，不提前标记通过。

## 共享审批fixture（已准备，尚未执行）

本轮task根下 `workspaces/approval-journey` 已建立唯一canonical工作区，ask/auto/deny复用同一份 `.go-cli.json`，只改变显式 `--approve`。目录内仅预置随机nonce/value=41的 `input.txt`、含唯一字面量的 `search.txt`、配置与空Skill/commands/MCP/hooks来源；所有待验收输出由真实模型工具创建，未预写。session路径是目录，history/memory/trace/production audit是目录内文件，审批audit默认也在目录内。预期JSON与分步prompt保存在task根的artifacts下，位于sandbox之外；原HOME和共享全局配置不变，provider连接与key只由受控harness环境提供。

初始精确grant后不改配置，依次做ask-read、ask-deny、ask-allow、ask-edit、ask-cancel和after-cancel；auto-basic再覆盖无modal写入/编辑/读取与无害bash；deny-write独立确认无输出文件。每条mutation提示要求拒绝或取消后停止，不改用其他工具重试。ask既有策略明确禁止bash，不能把该预期拒绝当成本轮回归。空slice不会清空原全局slice；启动前应只检查有效hook/server数量为零，若继承到非零来源则停止该场景并保留诊断，不能把配置中的空列表当作隔离已证明。

L07的generation保留不等于继续授权：重新grant会推进trust epoch，非法候选即使未发布，旧代持有的授权也不得恢复模型或工具执行。恢复合法配置/来源后需再次精确grant并成功 `/reload` 才继续。实际错误消息、UI反馈与最终结果由主代理报告填写，本计划不预判通过。
