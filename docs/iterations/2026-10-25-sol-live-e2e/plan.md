# Magpie Sol 实测计划（2026-10-25）

本轮从合并后的源码重新构建和取证。旧运行材料已清理；历史报告只用于选取回归点，不能代替本轮结果。测试控制链为实际 Go binary → owned PTY → 浏览器 xterm → 现有 Magpie → 真实 Sol，CUA 是唯一 UI 输入来源。截图必须标为“真实 PTY 的浏览器终端画面”，只截实际 viewport。

启动前只读 metadata 确认 Magpie 0.1.1480、PID 2300 监听 `127.0.0.1:3425`；`/v1/models` HTTP 200，目录包含 `codex/gpt-6.1-sol`。这只证明目录和本地服务可用，实际推理身份另验。

1. 准备唯一私有任务目录，构建实际 binary，登记 SHA、源码与进程所有权。所有 session、history、memory、audit、models.dev cache、trace、Go/npm cache、临时文件均在该目录。`HOME` 保持原值；trust/authority 使用真实生产路径，只处理本轮 canonical keys。verify：八项 storage guard 离线合同、三项投影合同、配置预检和个人文件 opaque SHA/stat 基线。
2. 先以现有 config loader 做安全预检，不装配 Agent，不执行来源。pretrust 检查 global 后台来源并读取任务配置；真实 grant 后再以真实 FileTrustStore 检查 actual merged config。输出只包含来源计数、服务器名称、布尔状态、provider 名和允许的路径字段；不输出命令、环境值、URL、正文或凭证。verify：四角色 eligible；任何未审阅 ambient 来源阻止 ready。
3. 协调者完成构建和审阅后写入匹配 SHA 的 ready gate，再由 CUA 启动真实 trust terminal、输入 TRUST、启动交互。verify：每个角色的实际 PID、physical SID、storage path、approval flag 与已审阅配置一致。
4. 执行下表，每次先设置标签；交叉核对 UI、durable receipt、文件副作用及 provider 独立审计。verify：每次模型请求均有 response ID、route ID、session 的精确匹配；served model 是所请求的真实 Sol。任何 ordered fallback、冲突或缺失都不计为验收通过。
5. 关闭全部 owned PTY/bridge，核对 inflight 为零、进程退出，再真实 revoke 本任务 keys。按 ownership manifest 精确清理；保留脱敏证据和已检查截图。verify：个人文件基线对比、无 owned process/authority/trust 残留；无法归属的记录不得删除。

| ID | 真实操作 | 可执行验收 |
| --- | --- | --- |
| B01 | 中文紧连“请用read读取input.txt” | 请求实际 advertise read；真实 read receipt、value=41、正常 terminal |
| B02–03 | 显式 tool_search 搜索 read，下一轮纯文本 | tool_search receipt 已知完成，两个 turn 共用 PID/SID，第二轮完成 |
| B04 | ls、grep、find、read | 实际四工具 receipt 与 fixture 匹配，无 Bash 替代 |
| A01 | ask write，CUA 按 n | 文件不存在；receipt decision source/choice 对应真实按键；run 正常结束 |
| A02–03 | ask write/edit，CUA 按 y | diff 展示 41→42；真实字节/hash 变化与 read 一致 |
| A04–05 | ask write，CUA Esc，下一轮 read | 被取消文件不存在；明确取消来源；下一轮同 SID 完成，不外推所有 in-flight cancel 分支 |
| A06 | ask 请求无害 Bash | 按既有 hard policy 拒绝，不为了测试改变 classifier |
| U01 | `--approve auto` write/edit/read/无害 Bash | 无审批 modal；真实文件 value=42、Bash marker、正向 terminal |
| C01–02 | auto 最后运行任务内 `sleep 30` 与晚标记命令，运行中真实 Esc | 实际 `canceled` receipt/terminal、进程组及后代已 join；30 秒后晚标记仍不存在；既有 unknown-effect fence 拒绝同 SID 下一 read、零新增 provider。不得换 SID 冒充继续成功 |
| N01–02 | `--approve deny` write，下一轮 read | 无 modal、mutation 未调度；只读下一轮正常完成 |
| D01 | skill_search→skill_load→read | Skill 正文私有 canary 仅从真实 load 得到；metadata/prompt 不预置答案 |
| D02 | 仅 Skill V1→V2，先 reload，再真实 regrant/reload | 未批准时 needs-trust；批准后同 PID/SID 下一 run 得 V2 canary；hook/config SHA 不变 |
| D03 | 仅 Hook V1→V2，真实 regrant/reload | Skill/config SHA 不变；真实 hook-events 新增行均为 V2 |
| D04 | 非法 Skill name `live/check`，再恢复原 V2 字节 | 候选拒绝、generation 不部分发布；非法阶段不提交模型；恢复/regrant/reload 后同进程任务成功 |
| D05 | Hook V3 仅拒绝 read | search/load 完成，read 为 known not-dispatched，模型报告 READ_BLOCKED；不绕过拦截 |
| M00–M00R | 模型明确调用 `mcp_discover(server=live-catalog)`，随后普通 read | unsupported completion 合同在任何外部 effects 前非 fatal 拒绝；receipt 为 `not_dispatched/error`，连接 marker/RPC 日志均不存在；随后同 PID/SID read 正常 |
| M01–02 | `mcp_list(query=live)`→answer→第二轮纯文本 | 只返回 live-catalog 名称/状态，无 command/env 私有 canary；未生成连接 marker、0 MCP 进程/连接；同 SID 正向完成两轮 |
| M03–05 | 用户在 idle `/mcp load live-catalog`，模型请求调用远端 lookup，随后普通 read | 真实 initialize/initialized/tools-list 和 loaded 状态；下一请求 catalog 新增远端 schema；durable remote call 在 tools/call 前被 sealed preflight 拒绝，之后同 SID read 完成 |
| D06 | 最后真实 trust revoke 后再提交 | provider admission 前拒绝；HTTP、durable user、hook 日志均不推进；保留真实退出/错误类别 |

MCP 正路径范围为本地目录 metadata 查询，以及用户显式 idle activation 的连接与 schema 加载；负路径检查 unsupported completion 的 sealed preflight 拒绝。远程工具执行及其 unknown effects、completion proof 不因目录、连接或预检拒绝而得到正向验证。M03 必须在全部 Skill/Hook 变更和 regrant/reload 完成后执行；D05 后先恢复 Hook V2、重新 grant/reload，再执行 M00–05，避免 active MCP 跨 trust epoch 的既有 restart-required 边界混入热更验收。harness 不伪造工具、模型输出、durable terminal 或 route；HTTP 200 单独不代表场景通过。

动态变更由任务目录内 fixture helper 逐项执行，每次记录 changed files 和前后 SHA；每次使用真实 revoke/regrant 规则，来源变化未经批准不得继续执行。Canary 和预期结果在模型 workspace 外；MCP descriptor 的 command/env 含仅供本轮检查的私有 canary，不是账号凭证。

本轮新增离线 TDD：先运行 11 个测试，出现 3 failures/3 errors；补充 metadata 和 cache guard 后 11/11 通过。它们不启动产品 binary、不调用模型，只验证 harness 合同。真实 UI、模型和最终门禁结果另写报告，不能由计划预判通过。

选定 MCP command/env 私有字面量另补四项离线合同，先出现 4 errors，补丁后 dynamic 15/15 和 shared 20/20 通过。bridge 只在内存扫描解码 JSON 字符串，保存 checked 布尔和命中标签数组，不保存 literal 值或正文；空命中只证明选定 literal 未出现，不外推其他编码或所有信息流安全。

正式截图采用 CUA 操作的独立 Chrome 窗口，真实 Go PTY 经现有 xterm 页面绘制，包含 browser chrome；它们不是原生 Terminal 或 full-page 截图。IAB surface 缺失与并发操作者的未提交缓冲属于校准历史，不当作模型输入或产品失败。正式放行前无产品进程、无模型请求。

实测修正：A06 真实 ask Bash 弹出了审批，推翻“必 hard-policy deny”的静态预期；不通过改变 classifier 或人工 n 将其改记为 policy deny。协调者决定对原无害 printf 真实按 y，按实际 output、receipt、terminal 记录，并新增 A07 同 SID read。原表中的 hard-deny 假设不计为通过。

## Ask UI/stdio parity 后续验收

旧候选 a6ef 的 basic/Ask 共11回合、25个强关联请求已冻结。由于完整 UI 与非 TUI 默认 Ask 的 resolver/classifier 不一致，root 暂停 ready，要求新候选验证统一行为；旧 PID/SID 和截图保留独立归因。

新完整 UI 至少复跑 B04 基础工具组、A01 n、A02 y、A03 edit diff、A04 Esc、A06 无害 Bash y、A07 同 SID read。启动为新 Go 进程，新 binary SHA 和 SID 单独保存，不将旧 PID 归为新候选。其余 auto/deny/dynamic/MCP 按原矩阵在最终新候选执行。

headless 使用最终同源码的 edge 构建（headless,no_plugin，另记精确 SHA），复制精确 bytes 到唯一任务目录的 headless-live/bin。独立 childroot 仅一个 ask workspace，storage、cache、audit、source fixtures、ready、control、ownership 各自隔离；沿用冻结 harness，无新 backend 动作。直接 CUA 输入真实 xterm，避免现有控制 textarea 的 bracketed-paste framing；先核对实际 durable user SHA。

HY→真实 Bash y→HYR 同 SID read；HN→真实 n→HNR 同 SID read；HE→真实 EOF→0 executor/实际退出。stdio y/n 依真实提示发送字符及 Enter，不将 EOF 后的新进程当作同 SID 恢复。任何误输或设备回复混入真实 turn 必须保留 attempt 并停止归为通过。

### EOF 计划假设的实际澄清

HE 实测分为两个上下文：审批等待时 EOF 得到 input_ended、0 intent/execution 后回到 idle；随后 idle EOF 才正常退出0。若将原“EOF→0执行/退出”字面读作单次审批 EOF 必须同时杀掉会话，该预期未满足并被观察推翻；不把该单次 gate 标为通过，也不为配合计划改产品。实际通过项分别是审批 EOF 安全拒绝与空闲 EOF 正常退出。
