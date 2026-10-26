# 独立真实测试证据审查（阶段性）

审查日期：2026-10-25
范围：`B01` 与 `basic-four-turns` immutable cuts。此记录不代表本轮整体验收。

## 已审范围与结论

| 场景 | 结果 |
| --- | --- |
| B01：中文紧连 read | 通过：真实模型请求、read 调用、持久成功回执、正常 terminal 与 fixture 结果相符。 |
| B02：tool_search 搜索 read | 通过：记录了实际 `key:y` 输入，tool_search 广告、调用与成功回执相符；execution evidence 含 `inline_pipeline_joined`。 |
| B03：搜索后的纯文本续聊 | 通过：同一物理 CLI 会话继续完成，无工具调用。 |
| B04：ls、grep、find、read | 通过：四个实际工具按序调用并成功；没有 Bash 调用，terminal 正常结束。 |

`basic-four-turns` 收录 10 个真实模型请求、6 个成功 durable tool receipts、4 个成功 run terminal。所有记录属于同一个 ask 角色的 CLI PID 与物理 session。四条真实用户输入的 hash 都与审阅过的 prompt 精确相符；其中 142 字节输入由 129 字节 prompt、xterm bracketed-paste framing 和回车组成。prompt 本身没有 OSC 或其他 ESC 控制序列。CUA 输入记录中的 `key:y` 是实际按键事件，不是从截图推断。

## 真实模型与工具证据

10/10 请求均记录为 provider `codex`，请求模型和响应头返回的 served model 均为 `codex/gpt-6.1-sol`。对应的 Magpie per-request route snapshot 显示单次成功尝试、`default` tier 和完成状态；裸 `served` 名称省略 provider 前缀，route `model` 与响应头保留完整模型标识。这个判断基于真实响应与 route/usage 记录，不基于模型目录或请求字段单独推断。

桥接器在每个 chat response 结束后立即读取该 service session 的 route endpoint，并将返回结果投影为 per-request snapshot。`captureBridgeSequence` 是桥接器的本地采集关联号；Magpie 返回的 route sequence 单独保留。10/10 请求的 response ID、route ID、service session 与唯一 usage 行精确相连；usage 行与桥接器采集的字段一致，token 对账通过，没有顺序回退。物理 CLI session 与 Magpie service session 是两个层次，报告中不把它们写成同一个 ID。Magpie 最终 routes 列表是 rolling latest，只含最新 route；它不用于证明较早请求的完整历史。

每条实际请求都记录 30 个广告工具及 per-tool schema hash 和整体 schema hash；每个实际工具调用都出现在该请求的广告工具集中。请求间的 schema 会随 tool_search 的可用状态变化，因此不把不同请求的 schema hash 误写成恒定值。

## durable 与副作用核对

持久记录中，四个 run 均以 `success` terminal 结束；6 个 receipt 分别对应 read、tool_search、ls、grep、find、read，状态均为 `success`。`input.txt` 与记录的基础 fixture 字节一致；immutable ledger 中两个预期存在的 fixture 文件与实际字节及摘要匹配，七个预期输出文件均不存在。截图中的搜索、查找和读取结果与 fixture 相符；没有执行写入操作。

我查看了 `12-ask-chinese-read-completed.jpg`、`13-ask-tool-search-approval.jpg` 和 `15-basic-four-tools-completed.jpg`。三张图均是带 Chrome 浏览器边框的真实页面，内部 xterm 展示 156×23 PTY 输出。第 13 张停在 tool_search 审批提示，最终按键和成功 receipt 由输入 metadata 与 durable ledger 证明；第 15 张展示基础工具完成。文件现为 JPEG，位于 corrected-format manifest 管理的图片集合中。

两份 immutable cut 的副本均通过各自 manifest 的摘要和字节数校验，safe-ledger 摘要也通过。basic cut 的原始源文件在之后的交互中继续追加，所以本审查以切片副本为准，不以当前滚动文件替代历史切片。

## B01/basic 检查点的历史状态

以下阶段尚无本审查认可的 immutable cut，不纳入本次通过结论：

- ask 审批的拒绝、允许、Esc 取消和 Bash hard-policy 边界；
- auto 与 deny 角色、运行中取消及其 unknown-effect fence；
- skill 与 hook 动态更新、非法候选恢复、hook deny 与恢复；
- MCP metadata 零网络查询、用户 idle activation/schema 加载和 sealed preflight 负路径；
- 最终 trust revoke、provider admission 前拒绝、进程退出、inflight 与 residual 检查。

本阶段不证明远程 MCP 工具成功执行，也不证明远程 completion proof 或 unknown effects 已闭合。原计划对真实远程工具执行仍标为 unsupported。

**该检查点结论：** 当时仅 `B01` 和 `basic-four-turns`（10 请求、6 receipts、4 terminals）独立通过。后续追加审查见本文件下文；这不是当前矩阵的结论。

## Ask-complete：旧候选 `a6ef` 的追加审查

这份 cut 属于旧候选 `a6ef`，不归因到后续统一 Ask 行为的新 full/edge candidate。`manifest.json` 中 9 份原始副本的摘要和字节数均匹配，safe-ledger 摘要也匹配；审查只使用该 immutable cut，不以之后继续追加的滚动文件替代。

| 范围 | 结果 |
| --- | --- |
| Ask 真实回合 | 11 条 durable user、11 个 `success` run terminal。 |
| Provider 请求 | 25 个已完成请求，均 HTTP 200、provider `codex`，请求模型与响应头 served model 均为 `codex/gpt-6.1-sol`；无 fallback。 |
| 关联与用量 | 25/25 的 per-request route snapshot 与唯一 usage 行以 response ID、route ID、Magpie service session 精确相连；usage 全页取完、token 对账通过，无 ordered fallback。 |
| Tool receipts | 合计 12 个成功 receipt；另有 2 个 `not_dispatched/error`、`user/deny`。 |
| 物理会话 | 所有记录属于同一个 Ask CLI 进程和物理 session；该物理 session 与 Magpie service session 是不同层次。 |

对每条请求，我核对了原始 provider snapshot 和 safe-ledger：route snapshot 在响应完成时由 `LiveForwarder.finish()` 按当前 service session 实际读取；本地 `captureBridgeSequence` 与本地请求序号一致。Magpie route 的成功尝试、served model、route ID 与 response ID 均与相应 usage 行精确相连。最终 `audit.routes` 是 rolling latest，本 cut 不用它声称包含完整历史。25 条 usage 分页完整，每条请求的 input/output token 对账均匹配。每次请求的 tool schema hash 可用，实际调用名称都出现在同请求的 advertised catalog 中。请求字段或 model catalog 本身不作为 served 身份证据。

11/11 条 durable user 内容都与任务 prompt registry 精确匹配，消息正文没有 ESC/OSC 控制字符。input metadata 记录了 11 次 `text+enter`、4 次 `y`、1 次 `n`、1 次 Esc 以及 3 条 xterm-input 元数据；`key:n` 和 Esc 对应的持久决议都记录为 `user/deny`，因此 Esc 这一项不是独立的 `canceled` 结果。

A01 的 `n` 和 A04 的 Esc 都留下 `not_dispatched/error`，对应目标文件不存在。A02 的 write 和 A03 的 edit 均成功；edit 审批截图显示 `41→42` diff，目标文件效果记录为存在且 8 字节。A05 在同一会话中两次 read 成功；A06 的 Bash 请求实际弹出审批并由用户 `y` 放行，执行记录最终为 `owned_process_group_joined`；A07 随后同会话 read 成功。截图 22 的真实 PTY 输出同时展示了 Bash 结果与后续读取。所有 11 个 run 均以 `success` terminal 结束。任务 owned-resources 元数据另确认该旧 Ask 进程已 exit 0、inflight 为 0。

我查看了 `16-ask-write-deny-review.jpg`、`18-ask-edit-diff-41-to-42.jpg`、`19-ask-escape-before-key.jpg`、`21-ask-bash-observed-approval.jpg` 和 `22-ask-bash-and-same-session-read.jpg`；它们与任务目录中的原图字节一致。图像是带 Chrome 浏览器边框的页面截图，内部为 156×23 的真实 PTY viewport，并非原生 Terminal 或 full-page 截图。若干图是在审批提示等待按键时截取，最终 n/y/Esc 结果由输入元数据与 durable receipt/terminal 交叉确认；页面底缘可见裁切的 JSON 页脚。

**旧候选阶段结论：** Ask A01–A07 的 25 个真实请求、12 个成功 receipts、2 个用户拒绝 receipts、11 个成功 terminals 独立通过；A04 的 Esc 实际落为 user/deny。A06 证明旧 TUI 当时将 Bash 暴露为 Ask 并在真实 `y` 后执行，不能外推新版 headless/stdio Ask parity。此结论不覆盖新版 full/edge candidate，也不覆盖 auto/deny、运行中取消、Skill/Hook 动态变更、MCP 场景或最终 revoke/清理；远端 MCP 成功执行仍不在已支持合同内。

## 新版 full candidate Ask：`final-full-ask`

此阶段属于新版 full candidate，使用新的 Go 进程与新的物理 CLI session；不与旧 `a6ef` 的 25 个请求合并。`final-full-ask/manifest.json` 的 10 份源副本与 safe-ledger 摘要均通过；对应的 `ask-final-start.json` baseline 摘要也匹配。新物理 session 文件从 offset 0 开始，旧 session 不在其中；共享 approval-audit 文件则按 baseline 的字节前缀验证后只计入本阶段 append suffix。输入 metadata 的本阶段后缀摘要也通过。原始 durable JSONL 重新投影后与 safe-ledger 逐字段相同。

| 范围 | 结果 |
| --- | --- |
| Ask 输入和终态 | 5/5 durable user 内容精确匹配任务 prompt registry，正文无 ESC/OSC 控制字符；5/5 run terminal 为 `success`。 |
| Provider 请求 | 17/17 完成的 Sol 请求；每一条均以 response ID、route ID、Magpie service session 精确匹配唯一 usage 行。 |
| Tool receipts | 10 个成功 receipt；F01、F02 各有一个 `not_dispatched/error`、`user/deny` receipt。 |
| 物理会话 | 5 个回合和全部 durable rows 属于同一个新版 Ask 物理 session；它与 Magpie service session 分开记录。 |

逐请求核对原始 provider snapshot 与 gateway usage：17 条都为 HTTP 200、provider `codex`，请求模型和响应头 served model 均为 `codex/gpt-6.1-sol`；每条 per-request route snapshot 的本地 capture sequence、service session、完成状态、单次成功尝试和实际 route ID 均通过核对，usage 分页完整且 input/output token 对账通过。没有 ordered fallback。最终 `audit.routes` 只有 rolling latest，因此没有用它替代逐请求 route snapshots。`LiveForwarder.finish()` 的源码路径是在实际响应完成后按该 forwarder 的 Magpie service session 读取 route；本地 bridge sequence 只是采集关联号，不能当作 Magpie route 序号。每次实际工具调用都存在于对应请求的 advertised catalog，且该工具的 schema hash 存在。

执行顺序和结果：F01 的真实 `n` 与 F02 的真实 Esc 都对应 `user/deny`、`not_dispatched/error`；因此 Esc 的 durable 结果是 user deny，不是独立的 `canceled` receipt。F03 实际执行 ls、grep、find、read、tool_search 五个只读工具，tool_search 经真实 `y` 批准并成功。F04 经真实审批完成 write、edit、read；图片展示 `41→42` diff，后续读取为 42，`ask-approved.txt` 的效果记录为存在且 8 字节。F05 Bash 经真实 `y` 获准，执行证明为 `owned_process_group_joined`，之后同一回合内 read 成功并读回基础输入值。所有 5 个终态均成功。输入 metadata 记录 5 次 `text+enter`、4 次 `y`、1 次 `n`、1 次 Esc 和 3 条 xterm-input 元数据。

原 cut 的 `effects` 白名单没有 F01/F02 实际目标文件名，因此它本身不能证明两个目标不存在。另一个 effects supplement 已用原 cut 的 manifest 摘要绑定，并由 supplement manifest 校验通过。该补证只保存 F01/F02 两个标签的 `exists=false` 与空字节数字段；provenance 说明目标从真实 durable tool-call 参数派生、限制在 owned workspace，采用 `lstat` no-follow。补证的 F01/F02 source run ID 与 safe-ledger 对应的 durable user run 及该 run 内唯一 write tool-call 相符。测量发生在原 cut 之后、正常 EOF 退出之后、下一 parent role 开始之前。退出后输入 metadata 与 cut 的 14 个既有事件精确成前缀，只多一个 `Ctrl+D` EOF 事件；provenance 记录新 model prompt 为 0、owned write dispatch 为 0、进程 exit code 为 0。因此可以确认两个目标在本阶段退出时不存在，但这不是与原 cut 同一时刻的观测。另有 F02 后检查点记录两目标 absent 且与对应 run/prompt 摘要相符，但该检查点不在 cut/supplement manifest 内，只作为未冻结的旁证，不计入上述结论。

approval-audit append suffix 的字节边界、摘要和 32 条投影均通过。该日志的 `session_id` 是统一的通用 `interactive` label，不等于物理 CLI SID 或 Magpie service session。对 10 条 `tool_outcome`，我从原始 provider snapshot 读取 ToolCall.ID，在内存计算其 SHA-256，并按摘要与工具名逐条匹配；10/10 均唯一匹配到本 cut 内唯一 turn（12 个模型 ToolCall.ID 在本 cut 内唯一）。另外 22 条 classifier proposal/resolution 没有 CallID/RunID/PreparedID，不能逐条连接到具体调用。故 audit label 不作为物理会话证据，也不把 proposal/resolution 说成已逐 prompt 关联；结果仍以 durable receipt、输入事件、run terminal 与真实 PTY 截图交叉核对。

我查看了 `25-final-ask-n-review.jpg` 至 `30-final-ui-bash-and-read-complete.jpg`。它们是带 Chrome 浏览器边框的真实页面，内含 156×23 PTY viewport；25、26、29 截在审批提示等待按键时，27、28、30 展示工具完成、diff 和 Bash 后续读取，页面底缘有裁切 JSON 页脚。纠正后的 image manifest 绑定原 full cut、candidate、进程和物理 session；六张图片的 JPEG/JFIF 签名、SOF 2780×1878 尺寸、SHA、bytes、`.jpg` 后缀及 repo 副本逐项相符。更名前后的原始字节相同，没有重编码。更正包另列 26 张任务原始截图，格式与 SOF 尺寸检查均通过；16 张公开截图副本均与任务原图逐字节相同。原 `final-full-ask` cut manifest 本身不含图片，修正包通过引用它的摘要补足图像绑定；先前按 PNG 偏移生成的错误图像 manifest 被明确 supersede。

**新版 full Ask 阶段结论：** 17 个真实请求、10 个成功 receipts、2 个拒绝 receipts、5 个成功 terminals 与同 session 工具链通过独立核对；F01/F02 的文件不存在状态由单独的退出后补证确认，并保留其晚于原 cut 的时间边界。此结论只覆盖新版 full Ask；headless edge Ask 及 auto/deny、取消、Skill/Hook 动态变更、MCP、最终 revoke/清理仍待相应 immutable cuts。远程 MCP 工具成功执行仍未获得正向验证。

## Headless edge Ask：Y/N 与 EOF

### Y/N 正向与拒绝回合

`edge-yn-positive` 是新版 edge binary 的独立 cut。其 manifest 副本与 safe-ledger 摘要匹配；基线精确落在单独保留的 calibration cut 之后。早先那次错误输入及其 2 个模型请求没有并入接受范围。接受后缀为 8 个真实模型请求，4 条 durable user 都与 prompt registry 精确匹配，没有未知输入；请求模型和响应头 served model 都为 `codex/gpt-6.1-sol`，每条请求的 per-request route snapshot 与唯一 usage 行以 response ID、route ID、Magpie service session 精确关联，usage 对账通过，未发现 fallback。

4 个 turn 在同一 owned Ask 进程与同一物理 CLI session 内完成：真实 `y` 后 Bash 成功，紧接的只读回合读回 41；真实 `n` 使 Bash 留下 `not_dispatched/error`、`user/deny`，下一回合同 session read 仍成功。合计 3 个成功 receipts、1 个拒绝 receipt 和 4 个成功 run terminals。服务 session 与物理 CLI session 分开记录。

我查看了 `13-edge-exact-paste-bash-y-review.jpg`、`14-edge-n-review.jpg`、`15-edge-yn-same-session-complete.jpg`，也查看了单独 calibration 的 `12-edge-calibration-denied.jpg`。这些是含浏览器 chrome 的真实页面，内部 xterm 展示 156×23 PTY。被接受的三张图分别展示等待 y、等待 n 和同 session 的最终读回；calibration 图只证明保留了那次输入校准尝试，不作为正向验收。对应 image manifests 与 cut、candidate、PID、物理 session 绑定；源图和文档副本字节一致，实际 JPEG SOF 为 2780×1878。

### EOF 审批边界

`edge-eof-negative` 的 11 个 manifest 副本和 safe-ledger 摘要通过。`edge-before-eof` 基线的 session 字节、provider snapshot 和输入 metadata 前缀均与接受的 Y/N cut 相符；该 cut 只新增 1 条 HE user，精确匹配已登记 prompt，正文没有 ESC/OSC。基线之后输入 metadata 只有 2 个 xterm-input 事件和 2 个 `Ctrl+D`，没有额外 user。durable suffix 的 5 条记录逐项与 safe-ledger 的 ID、run、sequence、type、content bytes 和摘要相符；approval-audit 追加的 2 行也与 raw append suffix 相符。

cut 追加了 2 个真实 Sol 请求。两个请求均为 HTTP 200、无 fallback；route snapshot 与对应 response ID、route ID、service session 和唯一 gateway usage 行精确匹配，token 对账通过。最新模型响应明确解释了审批输入结束后命令没有运行；它的 durable tool 结果字节数和摘要与 provider snapshot 中最后一条 `input_ended` Bash receipt 相符。该 receipt 为 `not_dispatched/error`，未进入 intent/execution；tool-audit 没有新增行，文件效果列表与前一 Y/N cut 相同。审批审计使用逻辑 session label，不能当成物理 SID 或 Magpie service session。

实际边界是两步：第一次 `Ctrl+D` 发生在审批等待态，拒绝当前 Bash 并回到 idle；第二次 `Ctrl+D` 在 idle 时使同一进程正常 exit 0。两次期间没有新模型 prompt。计划表把 HE 写成“真实 EOF → 0 executor / 实际退出”；零执行与实际退出都观察到了，但它们不是同一次 EOF。因此，按计划的单次 EOF 合并条件，HE 子项未按原文完整通过；本记录把它拆为“审批态 EOF 拒绝”和“idle EOF 退出”两个观察，不把第二次 EOF 回填成第一次的结果。durable suffix 记录了 user、assistant、tool、assistant、checkpoint，两个 provider response stream 都正常结束；此 cut 没有单独的 run-terminal 计数，所以这里不额外声称一个 `success` run terminal。

我查看了 `16-edge-eof-before-key.jpg`、`17-edge-eof-rejected.jpg`、`18-edge-eof-normal-exit.jpg`。三张图均是带 Chrome 边框的真实页面，内部为 156×23 PTY；16 显示审批等待态，17 显示第一次 EOF 后的拒绝与 idle 提示，18 显示第二次 EOF 后的 `Session ended`。corrected image manifest 绑定 cut、binary、PID 和物理 session；三张源图及文档副本均为原字节 JPEG，实际尺寸 2780×1878。

### PTY 字段补正范围

四个已审 v1 cut 的原 `safe-ledger.ptyID` 字段是 dict 或缺失值，不能作为正确的 PTY 标识；immutable v1 cut 没有改写。各自的 pty-field supplement 绑定原 cut manifest，source-check 五项为真；我另以 owned PID、输入 metadata basename、实际 copied exec/input 文件、provider snapshot 和 gateway usage session 交叉复核。公开 v2 投影现在以字符串表示 PTY ID。物理 CLI session 与 Magpie service session 保持分层，不相互替代。

补正中的 `sourceHelperVersion=v1` 指产生原 cut 的旧 collector；其源码副本与公开 v1 appendix 字节匹配。公开 v2 collector appendix 是独立版本，已核对 source SHA。它们分别标记旧 cut 来源和新 collector 来源，不把旧字段投影改写成 v2 运行时事实。

## Auto：普通成功链与运行中取消

`auto-positive` 的 cut、safe-ledger 与来源副本校验通过。该阶段有 1 条 durable user、5 个已完成真实模型请求，均为 provider `codex`、served `codex/gpt-6.1-sol`；每个 per-request route snapshot 都在该响应后采集，并与唯一 usage 行以 response ID、route ID、Magpie service session 精确匹配。四个实际 tool call 名称都出现在对应请求的 advertised catalog，且 schema hash 可用；write、edit、read、Bash 四个 receipt 均为 `success`，对应 run terminal 正常结束。任务文件效果为 value 42，Bash 输出有成功标记。四条请求没有 fallback，usage 对账通过；PTY 与物理 CLI session 都属于同一 owned 进程。图 31 展示完整 auto 链，没有审批 modal。

`auto-cancel-negative` 是同一进程和物理 CLI session 的下一阶段。cut 的 9 份源副本、safe-ledger 摘要与 session append 边界校验通过：新后缀 8 行，起始字节前缀与 cancellation baseline 精确相同。真实 Bash/sleep 运行时收到 Esc；Esc 发生在启动后约 17.62 秒、30 秒计时结束前约 12.38 秒。进程观察记录确认 owned process group 和子进程随后消失；193.584 秒的晚检查仍未发现晚标记。图 32 展示 Bash 仍在运行时的取消前画面，图 33 展示取消后的 unknown 状态与后续 read 被 fence。

我重新从原始 session JSONL 核对生命周期字段，而非只读 collector 投影：intent 后是 `dispatch_state=dispatched` 的 `cleanup_pending` execution，再是 `quiescent` execution 与 `completion_proof=owned_process_group_joined`。同一 action 随后有 typed `execution_resolution` checkpoint，证据为 `local_resources_quiescent`；tool receipt 行明确是 `receipt_outcome=unknown`、`dispatched`，run terminal checkpoint 也明确为 `run_outcome=unknown`。这些是 append-only SessionEntry 中的真实 typed records。`resource_state` 单独只是资源观察，但完成证明与 resolution checkpoint 说明本地 owned resources 已 join；它们不说明命令副作用结果已知。对应字段定义见 [session types](../../../internal/session/types.go)、[run status](../../../internal/core/run_status.go) 与 [run commit](../../../internal/core/run_commit.go)。

因此计划 C01 所写的 `canceled` receipt/terminal 没有观察到：本轮记录的是 `unknown` receipt 和 `unknown` run terminal。运行资源已经 quiescent，副作用结果仍然 unknown；不能把这写成成功取消或已知无副作用。计划 C02 的下一 read 输入 frame 与已登记输入匹配，但没有形成 durable user 或新的 provider 请求；同 SID 的 fence 生效，不能把它称为成功恢复。此前描述中的“无 canonical receipt/terminal”不准确，现依原始 JSONL 更正为“typed unknown receipt/terminal”。

auto 取消阶段模型请求仍单独以 response ID、route snapshot、route ID 和 Magpie usage session 关联核对；本地 physical SID、service session 和 ownership PID 分字段，不互相代替。31–33 三张图为真实 Chrome 页面中的 xterm/PTY 截图。corrected image manifest 绑定原 cut 与 candidate；图像是原字节 JPEG，实际尺寸 2780×1878，repo 副本字节一致。

## Deny-all：策略拒绝与续聊

`deny-all-policy` 的 7 份 manifest 源副本与 safe-ledger 摘要校验通过。两个 durable user 都逐字匹配登记 prompt，正文无 ESC/OSC 控制字符。四个真实模型请求均返回 HTTP 200，provider 与响应 served identity 为 `codex/gpt-6.1-sol`；我直接按每个请求中的 route snapshot 与四条唯一 gateway usage 记录核对 response ID、route ID、service session。4/4 关联精确，route 都是一次成功尝试、`default` tier、无 fallback，token 对账通过。per-request snapshots 用于这四个请求；cut 的最终 `audit.routes` 只含 rolling latest 一行，没有被当作全历史证明。物理 CLI session 与 Magpie service session 保持分层。

两个真实模型 tool call 分别请求 write 与 read，但 `deny_all` classifier 对二者均直接拒绝：durable tool rows 都是 `not_dispatched/error`，没有 intent/execution，也没有 executor 调用；审批审计中两项均是 `deny_all` proposal/resolution。两个对话 run terminal 均为 `success`，表示模型回合完成，不表示任一 tool 成功。write 目标从实际 durable tool 参数派生并在任务工作区做 no-follow `lstat`，确认为不存在；safe-ledger 的目标效果标签也为 absent。第二轮 read 同样被拒绝，助手如实说明无法读取；同一 owned 进程和物理 session 继续完成这轮对话。

计划 N01–02 预期 write 不调度、随后 read-only 回合正常完成。N01 的拒绝和 absent file 符合预期；N02 的 read 没有执行，因当前 deny-all classifier 会拒绝所有工具，不能写成只读成功。该结果符合本次观察到的 `deny_all` 行为，但计划中的 read-success 子项未通过；本阶段也不证明另一种 read-only permission mode。真实截图 34 展示 write 与 read 都被策略拒绝、随后用户继续对话；我查看了原图。它是 Chrome 浏览器内的真实 xterm/PTY 画面，corrected manifest 与 cut 绑定，JPEG 原字节和 repo 副本匹配，尺寸为 2780×1878。

## Dynamic Skill 与 Hook：D01–D03

三个 immutable cuts 的 manifest 副本、safe-ledger 摘要、baseline manifest 关联、session 原始前缀/append suffix 与输入事件前缀/suffix 均通过。三个阶段各追加 4 个 Sol 请求，provider、响应 served model 与 route snapshot 均对应 `codex/gpt-6.1-sol`；每个 response ID、route ID、Magpie service session 都唯一匹配 gateway usage，单次成功 route attempt、无 fallback。每个真实 tool call 名称都出现在对应请求的 advertised catalog，schema hash 可用。D01、D02、D03 的每条新增 durable user 都精确匹配登记 prompt，同一 prompt 在三个阶段内容相同，且没有 ESC/OSC 控制字符；三阶段属于同一 owned PID 和同一物理 CLI session。Magpie service session 单独核验，不用物理 SID 替代。

### D01：Skill V1

D01 新增 1 个真实 user 与 4 个模型请求。模型实际依次调用 `skill_search`、`skill_load`、`read`，三项 durable receipt 都是 `success`，run terminal 为 `success`；read 返回测试输入值 41。原始 durable `skill_load` 结果含一个版本化 tag，最终 assistant 回答与该次真实 load 结果匹配，用户 prompt 不含该 tag。V1 候选没有被预置到用户消息。

我查看了 [40-dynamic-v1-iab.jpg](screenshots/40-dynamic-v1-iab.jpg)。它是实际 IAB 页面的截图，内含真实 xterm 91×23 PTY；JPEG 原图为 826×862，phase image supplement 与 D01 cut manifest 绑定，repo 副本逐字节相同。

### D02：Skill V2 reload 与授权边界

D02 又新增 4 个请求，三项实际工具 receipt 与 run terminal 均为 `success`。我将 D02 的 successful `skill_load` 原文只在内存与 D01 比较：正文确实变化，D02 最终回答匹配新 load 的 V2 tag、不匹配 V1 tag；该 prompt 与 D01 相同，prompt 本身未含 V2 tag。D02 的 V2 load 与其后 D03 的 load 完全相同。V2 tag 的字面值不写入本报告。

D02 原始 PTY 中先出现一次 `/reload` 的 `needs-trust` 拒绝，之后另一次 `/reload` 和同一 prompt 的成功 V2 回合。输入 metadata 中两个 `/reload` frame 的 SHA 与实际字节长度相符；任务内 trust-grant 输入 metadata 有一个 `TRUST` frame 落在两次 reload 之间，后续模型 prompt 在第二次 reload 后。该 trust-grant terminal metadata 不属于 D02 cut 的 manifest sources，故我把它标成 cut 外的 task-local 佐证，不声称它由 D02 cut 单独封存。V2 phase transition metadata 显示只有 Skill 文件变化，Hook、project config 与 MCP config 的观测摘要保持相同。

我查看了 [42-skill-v2-reloaded-iab.jpg](screenshots/42-skill-v2-reloaded-iab.jpg)。图像显示成功 reload 后实际读取输入值和 V2 回答；它是 IAB 截图，xterm PTY 为 91×23，JPEG 原图尺寸 826×862，image supplement 与 D02 cut manifest 绑定且 repo 副本字节一致。图 41 的 needs-trust 截图在 task raw 中可见，但没有 image supplement；本节对拒绝依据 D02 冻结 PTY 与输入元数据，不依赖未绑定的图 41。

D01 与 D02 的 observation supplements 分别通过摘要校验并绑定相应 cut。它们只保留所选 MCP literal 检查的布尔/标签结果：每请求检查标记均为 true，命中标签列表为空；没有输出 literal 内容。provider evidence 中没有原始完整 upstream request body，因此这里引用的是这份按 request 扫描生成、绑定 cut 的安全观察结果。

### D03：Hook V2

D03 新增的 4 个请求同样精确匹配 Sol route 与唯一 usage；`skill_search`、`skill_load`、`read` 三个 receipt 均为 `success`，run terminal 为 `success`，read 返回输入值 41。Skill load 字节与 D02 完全相同，最终回答仍与 V2 load tag 匹配。Hook phase transition metadata 显示仅 Hook 配置文件改变，Skill、project config 与 MCP config 未变。

Hook event 文件没有作为 D03 raw source 文件复制进 cut；我用 safe-ledger 的每阶段文件长度与摘要，对任务目录 append-only 文件的对应前缀逐字节复核。D01 的 24-byte 前缀、D02 的 48-byte 前缀和 D03 的 72-byte 前缀均与各自 cut 的摘要相同；只解析匹配前缀后，事件计数依次为 V1=3、V1累计6、V1=6/V2=3/V3=0。因此 Hook V2 的新增三条事件有 cut 内历史摘要支持，且没有将之后的追加混入 D03 计数。

D03 的 selected-MCP-literal observation 和图 43 image supplement 后续已补齐并绑定 D03 cut。观察值来自原始 provider snapshot 中已记录的 request flags；它不是对未保存 upstream request body 的新扫描。我查看了 [43-hook-v2-reloaded-iab.jpg](screenshots/43-hook-v2-reloaded-iab.jpg)：实际 IAB 页面含 91×23 PTY，image manifest 与 cut 绑定，JPEG 原字节和公开副本一致。

**D01–D03 结论：** Skill V1、V2 fresh load 与 Hook V2 行为均有真实模型/工具/持久记录支持；版本 tag 只核对匹配关系，未记录字面值。D02 reload 前的 needs-trust 边界与之后 successful V2 load 有真实记录，grant terminal 另有 task-local metadata。D03 的 Hook V2 event 增量已按 cut 固定摘要独立确认；图 43 已绑定。三个阶段的 literal sidecar 现有逐请求来源关联，但没有完整 upstream request body 供独立重扫。

## D04：非法 Skill 候选拒绝与 D04R 恢复

`dynamic-invalid-candidate` 的 manifest 副本和 safe-ledger 摘要通过。session 文件长度与开始边界相同，没有新增 durable row；唯一新增输入是一个 `text+enter`，provider sequence 基线之后没有新模型请求，safe-ledger request 数为 0。冻结 PTY 显示 reload 返回 `invalid catalog name`。D04 的 transition metadata 只记录 Skill 文件改变，project、Hook 与 MCP 配置观测均未变。此 live cut 能证明拒绝发生且没有模型输入/持久会话追加；它不暴露 active generation 数值，本报告不猜测 generation。

`dynamic-invalid-recovery` 以 invalid 阶段之前的 session/input baseline 捕获恢复后的 append。边界摘要和 raw 副本通过；追加 18 行 durable records、1 条 user 和 4 个新模型请求。该 prompt 与 D01/D02/D03 相同，匹配登记内容且无 ESC/OSC。四条新请求均为真实 Sol，route/usage response ID、route ID、service session 精确关联，单次成功 route attempt、无 fallback；skill_search、skill_load、read 均有成功 receipt，run terminal 为 `success`。D04R 的 `skill_load` 正文与 D02 V2 load 完全相同，最终回答与此 load 的 V2 tag 匹配且未在 prompt 中预置；read 再次返回 41。恢复后 skill fixture 摘要与 D02 V2 的 after 摘要一致，实际文件摘要也匹配；恢复 transition 只改 Skill 文件。

D04R 输入 metadata 覆盖 invalid attempt 与恢复 run；另一个 cut 外的 task-local trust terminal input 记录把真实 Trust grant 定位在第一次 reload 拒绝之后、第二次 reload 之前，随后才是同 prompt 模型输入。这个 grant terminal 是 cut 外旁证，不写成 D04R manifest 已封存的数据。恢复后的 trust preflight 为 eligible/project verified。该阶段属于原 PID/物理 session，恢复不是另起 CLI。图 44 展示 invalid error，图 45 展示恢复后的 read 与 V2 回答；两张均为真实 IAB 页面截图，含 91×23 PTY viewport，JPEG 原图 826×862，image supplement 精确绑定各自 cut，repo 图片副本与原始字节相同。

D04R 的 selected-MCP-literal observation 后续已补齐并绑定其 cut；它逐条投影 raw provider snapshot 中原有的检查标记，不是对不存在的完整 request body 重新扫描。

**D04/D04R 结论：** 非法候选在没有新模型请求或 durable user 的情况下被拒绝；恢复阶段从同一 session 用已核对的 V2 Skill 完成 search/load/read，并成功读回 41。没有使用 generation 数字或未绑定图像作结论依据。

## D05：Hook 拦截 read

`dynamic-hook-deny` 的 manifest、9 份 source copy、baseline session 前缀与 append suffix 摘要均匹配。该阶段新增 1 条 prompt、4 个 provider 请求；请求、route snapshot 与唯一 Magpie usage 的 response ID、route ID、service session 逐条匹配，均为真实 Sol、单次成功 route、无 fallback。调用的 `skill_search`、`skill_load`、`read` 均在各自请求 advertise 的工具目录中且有对应 schema hash。

`skill_search` 与 `skill_load` 成功。`read` 没有 durable intent 或 execution，canonical tool receipt 是 `interrupted`；截图显示 interceptor 取消，助手返回 `READ_BLOCKED`，run terminal 为 `success`。这证明该请求被 Hook 拦截且对话完成，不是 `not_dispatched/error` receipt，也不证明 read 执行成功。没有第二次 read 调用。safe-ledger 的 Hook event 文件前缀摘要与任务文件对应前缀匹配，Hook transition metadata 只标记 Hook 配置改变。

我查看了 [46-hook-blocked-read-iab.jpg](screenshots/46-hook-blocked-read-iab.jpg)。image supplement 与 D05 cut manifest 绑定，原 JPEG 与公开副本逐字节相同；截图是实际 IAB 页面，含 91×23 PTY viewport，画面显示拦截结果和 `READ_BLOCKED`。

该阶段四个 request metadata 都记录 fixture-fact scan checked 且没有命中。后来补的 selected-MCP-literal observation 与 cut 和 provider metadata 绑定，逐条记录四个请求的原有 flags；它不保存完整 upstream request JSON，因此我无法独立重扫 request body。

**D05 结论：** Hook 拦截与模型回合完成有真实记录；计划要求的 known not-dispatched 子条件与实际 `interrupted` receipt 不同。不能把该 receipt 改写成 `not_dispatched/error`。Literal sidecar 已绑定，但只投影了 capture-time flags，未提供可供独立重扫的 body。

## MCP：目录查询、发现拒绝与显式加载

五个 MCP cuts 的 manifest、source copies、baseline session prefix/append suffix 与 input event prefix/suffix 均匹配。M01、M02、M00 和 M03 remote 阶段共 10 个新 provider 请求；逐条 response ID、route ID、Magpie service session 关联精确，provider/served identity 为 Sol，单 route 成功、无 fallback，token 对账通过。它们与 D01–D06 共用 PID 和物理 CLI SID；Magpie service session 另行关联，不与 physical SID 混用。四个有模型请求的 MCP phases 的 per-request fixture flags 均 checked 且无命中；9 个 phase literal sidecar 共 30 条记录，经 cut、source metadata SHA、request sequence、request/response SHA 与 response ID 复核后全部一致。

### M01–M02：本地目录 metadata 和续聊

`mcp_list` durable receipt 为 `success`。我从冻结 session raw row 解析了实际工具输出，只检查结构和值类型，不把内容复制进报告：返回 1 个对象，字段恰为 `name` 与 `state`，包含计划中的 server label；没有 command、env 或其他配置字段。该阶段 `mcpStartMarkerExists=false` 且 MCP RPC method count 为空。随后 `read` receipt 成功并读回 41。下一 cut 只增加一条纯文本 user turn、一个 Sol request 和成功 terminal，无 tool call；PID/SID 连续，仍无 MCP marker/RPC。这支持 metadata-only 查询和普通续聊，不证明建立了 MCP 连接。

### M00–M00R：discover 的 sealed preflight 拒绝

模型实际调用 `mcp_discover` 后收到 typed `not_dispatched/error` receipt；该 call 没有 intent/execution。receipt 内容指出 durable completion 无法验证。MCP process marker 仍不存在，RPC 计数为空。随后 `read` 在同一 owned PID/physical SID 成功，run terminal 为 `success`。因此负路径没有连接或加载 server，且对话能继续；不把 HTTP 200 或助手文字当作工具成功证据。

### M03：idle activation 与远端调用拒绝

`mcp-idle-activation` 的 input boundary 新增 1 个 idle text frame，session 无增长，没有 durable user、provider request 或 tool row。实际 IAB 截图 50 显示 `/mcp load` 和 loaded 状态。绑定的 MCP RPC log 精确记录各一次 `initialize`、`notifications/initialized`、`tools/list`。之后的 provider request catalog 首次出现已加载的远端 lookup 工具和 schema hash；此前 M01/M02/M00 请求 catalog 中没有这个工具。截图 51 显示实际审批提示，input metadata 记录随后的一次 `key:y`。

收到 `key:y` 后模型发起的远端 call 仍由 sealed completion preflight 拒绝：typed receipt 是 `not_dispatched/error`，无 intent/execution。M03 remote cut 的 MCP RPC log 与 idle activation cut 字节完全相同，仍只有三条握手方法，没有 `tools/call`；下一步 `read` 成功并以正常 terminal 结束。这个结果证明 schema 被加入模型请求目录、远端执行在 dispatch 前被拒绝并可继续本地对话，不证明远程工具成功执行或其效果可回放。RPC raw log 只保留方法请求，不包含 `tools/list` 的响应正文；因此 provider catalog 里的 schema hash 可核验，但无法从该 cut 直接重算服务端原始响应 schema hash。

我查看了截图 47–52：它们均为实际 IAB 页面和浏览器内 91×23 PTY 画面；对应 image supplements 与 cuts 绑定，JPEG 原字节、尺寸和公开副本 SHA/bytes 一致。图 47 显示目录字段与 read；图 49 显示 discover 拒绝后 read；图 50 显示 idle load；图 51 显示 approval；图 52 显示 sealed preflight 拒绝后 read。图 48 显示同 session 纯文本续聊。

## D06：真实 revoke 后的 provider admission 拒绝

`dynamic-revoked-admission` 的 12 份 source copy、baseline session prefix 和唯一新增 input event suffix 摘要通过。该 phase 有 1 个 text+enter frame、0 新 provider requests、0 durable entries；session byte length 不变，provider sequence 仍等于 baseline，approval audit byte length 不变。safe effects 与 M03 remote phase 相同，Hook event prefix 未追加，MCP handshake counts 未变化。图 53 是真实 IAB/PTy 截图，画面显示 process exit 1 以及 stale-trust/unbound-transcript 错误；image supplement 与 phase cut 绑定。

独立的 `dynamic-revoked-admission-trust-terminal` supplement 的 manifest 及 3 份文件摘要通过，并绑定 D06 cut。安全 terminal projection 记录 parent-dynamic role、`actualArgvVerified=true`、trust/revoke 操作标签与 exit code 0；目标 live PID 与 physical SID 与 D06 一致。记录时间语义明确为 filesystem capture metadata timestamps，而不是进程真实 start/end 时间；我没有读取 raw exec/PTY 中的 trust key 或命令正文。结合 D06 主 cut，实际 revoke 后的尝试在 provider admission 前被拒绝，没有新的 provider HTTP 请求、durable user 或 Hook 更新。该 terminal projection 与 model cut 分层引用。

## Selected literal evidence 的范围

九个 phase 的 observation sidecar 覆盖 D01、D02、D03、D04R、D05、M01、M02、M00 与 M03 remote，共 30 个 request record。每份 sidecar 的摘要绑定相应 phase cut 和 raw provider metadata；我逐条核对 sequence、request SHA、response SHA、response ID 与 source snapshot，并确认 30/30 的 capture-time flags 是 checked、命中标签列表为空。sidecar 的 source kind 明确说明它只是既存 bridge flags 的精确投影，不是新扫描。当前任务 artifacts 没有完整 upstream request JSON，因此独立复核无法重新解析原 body；这个结果只支持 capture-time selected-literal scanner 的检查记录，不外推到未选 literal 或一般信息流安全。

## Root/cleanup owner 提供的清理证据

此处只审 safe receipt 与 pinned stop proofs；我没有打开 cleanup archive 的 authority 子目录、raw authority 或个人配置。这些是 root/cleanup owner 的清理记录，和上文的真实模型场景复核分开归因。

- `frontends-safe-to-stop.json` 记录全部工作已完成、closed bridge snapshot 存在、active CLI 计数为 0，且标记 frontends 可停止。`frontends-final-stop.json` 的 SHA 与 root 提供的 pin 相符，也链接到该 safe-to-stop 文件；两项 frontend 记录均有 stop 前身份与 birth metadata，发送 `SIGTERM`，最后 `ps` 输出为空且标记 absent；同时记录未触碰 Magpie。
- `cleanup-scope-stop-before-final-revokes.json` 标记 owned CLI、frontends、Go processes、task writers 均已停止；stop observation 中 known process present 为 0、binary/CWD foreign holders 为 0。
- `cleanup-revokes-final-20261025T082749Z/trust-decision-receipt.json` 与其 5 份 exit metadata 逐份核对：decision receipt 的 pin 匹配；每个 exit metadata SHA 与 receipt 匹配，`exitCode=0`、binary SHA 与 decision 记录相同，启动时间晚于 pinned stop observation；每份 stopReceipt SHA 都回链到已核 safe proof。这里仅检查安全投影的摘要、状态和时间关系，没有打印或读取 `canonicalRoot`、argv、cwd 或环境值。
- `cleanup-archive-20261025T082955Z/execution-receipt.json` 记录 `partial=false`、归档 authority 5 项、移除本任务 trust tombstone 5 项、保留未知 authority 4 项；receipt 同时标记 hard-personal/original baselines 与 unrelated trust semantics unchanged，并保留 stable lock。这里只核对 receipt 中的安全计数与状态，不读取归档内容。

因此，owned process stop 与授权清理现有 root/cleanup-owner 安全凭据支持；我没有直接查询系统进程或检查 authority 内容。独立复核结束时，reviewer 尚未独立核验任务临时目录最终删除及删除后的 absence；后续 root 的删除与 absence 记录见 [cleanup.md](cleanup.md) 和 [cleanup-summary.json](cleanup-summary.json)。

**独立场景结论：** B01/basic、旧候选 Ask、新版 full Ask、headless edge Y/N、auto-positive、auto 取消与 fence、deny-all、Dynamic D01–D06 和 MCP cuts 已逐阶段复核。edge HE 的单次 EOF 合并条件未满足；auto C01 的 receipt/run terminal 为 unknown；计划 N02 的 read-success 子项未满足；D05 read receipt 为 `interrupted`，不是计划中的 `not_dispatched/error`。MCP discover 和远端 lookup 均被安全拒绝后同 session read 成功；M01 list 为 metadata-only；M03 仅证明 handshake/schema 暴露与 sealed preflight 拒绝，不证明远程执行。D06 的 revoke terminal 与 provider-admission 拒绝有分层证据。Selected-literal 记录是 capture-time flags 投影，原 upstream JSON 未保存。远程 MCP 工具成功执行及 completion proof 仍不在本轮已支持合同内。就独立复核当时的证据而言，reviewer 未直接检查最终 task 临时目录的删除后 absence；之后 root 的删除及 absence 时间见 [cleanup.md](cleanup.md) 与 [cleanup-summary.json](cleanup-summary.json)。

## 2026-10-26 文档收尾

本次收尾只澄清独立复核结束时与之后 root 清理的时间边界。root 后续删除了 canonical private task directory，并于 `2026-10-25T08:45:14.291795+00:00` 记录删除后 absence；现有依据为 [cleanup.md](cleanup.md) 与 [cleanup-summary.json](cleanup-summary.json)。该后续系统检查归属于 root，不表示独立 reviewer 亲自查询了系统。未修改 live raw、cuts、scenario JSON、图片或原验证计数。
