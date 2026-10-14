# Dot develop · 本机 Codex / Magpie 真实验证

2026-10-14。远端595d059已保留并整合，合并版本完整真实运行和两项全量门禁均通过，生产代码已推送并核对，QA证据提交随后推送。最终生产源码为 `bdb76efb0b31152e4c554268b43d39ef6285115a`。完整真实运行 [193912-aafaafae](terminal/20261014-193912-aafaafae/run-report.json)通过 **194/194 检查**：20条用户轮记录、36次真实完成推理（35个interactive请求、1个内部nonstream摘要请求）、55张终端截图，CLI退出0、race检测无报告。主代理规划/合并/审查，三个 GPT 6 Luna Max 子代理分别执行安装与修复、真实终端测试、独立证据审查。上轮[模拟验收](../README.md)单独保存，不代替本次真实上游证据。

## 安装与本机连接

官方 Magpie `v0.1.1231` 已安装到 `/Applications/magpie.app` 并启动，服务只监听 `127.0.0.1:3425`。官方arm64包SHA256为 `c0a1ab3bc7b1b880289c10971cdd6e0853dba1a9305c30b8e8f67c24a8db6af6`，签名/公证检查通过；[安装与连接证据](magpie/README.md)。现有已登录Codex由Magpie发现，认证材料未读取、输出或提交；全局Codex配置未修改。

在本仓终端运行 `./scripts/magpie/run-dot.sh` 即可使用本机Codex，默认模型 `codex/gpt-6.1-sol`。脚本只为Dot设置provider连接参数，保留调用目录，不改全局Codex接入方式。连接配置使用显式10分钟total/4分钟stream-idle，以覆盖真实PPT长推理；可通过环境变量覆盖。框架未配置时的30秒/10秒默认值保持。模型catalog不代表账号可用性；本报告以实际完成推理为依据。

## 场景验收

| 场景 | 实际结果与多角度证据 |
| --- | --- |
| 连续对话、多工具 | 实际Read/Ls/Grep/Write/Edit/Bash；随机fixture标记、调用ID、回执、durable join及文件字节核对。每个interactive请求带当前用户nonce，响应/route/usage精确关联。 |
| 审批拒绝及允许 | 当前模态逐页审阅后真实按键拒绝，确认无副作用；下一轮允许写入并编辑，文件内容正确。不是自动放宽审批。 |
| 默认策略拒绝 | 批准尝试Python命令后实际Sandbox.Validate拒绝，进程未启动、文件未产生；已知error回执进入后续真实模型解释，下一轮可正常执行。 |
| 长任务和补充指令 | 20秒Bash实际启动，运行中进度可见；follow-up输入/ACK、开始/完成文件时序、后续provider请求和对应回执均核对。 |
| PPT生成及下一轮修改 | Dot真实Write/Bash生成四页editable PPTX；下一用户轮Edit/Bash只改slide4 INITIAL→REVISED。保存v1/v2、Go helper前后版本、exact tool/receipt lineage；OOXML、实际文件渲染及双重视觉审查通过。 |
| 生产上下文压缩及记忆 | 实际Read273673字节成功回执，随后六次约30KB真实bracketed paste；生产summary真实推理并提交typed compaction。估算108584→33247，61→10 items；同一durable entry的来源边界覆盖原Read且保留canary，后续真实请求含最新输入，精确回答早期key=value。 |
| Esc及安全继续 | 25秒Bash启动后按Esc；owned资源quiescent，操作结果诚实保留unknown。原延迟副作用未发生，普通提示被拦截且无provider请求/用户记录。明确 `/session new` 新物理会话后真实no-tool回复成功，durable用户行及success terminal绑定新ID，旧unknown回执/terminal保持原样。 |

[汇总验收与哈希](acceptance.json)。完整源证据是[原始报告](terminal/20261014-193912-aafaafae/run-report.json)及[主代理审查](root-review.md)。独立协议/产物/会话审查保存在[独立最终审查](review/193912-independent-live-review.md)。本次先保留失败、诊断原因，窄修复经审查及回归后复跑；旧失败报告未被改成通过。[问题账本](issues.json)分开记录产品、测试装置、产物与provider问题；[轮次索引](rounds.md)列出所有保存的运行。

## 关键截图及产物

终端截图是headless Chromium中xterm.js渲染的**真实PTY字节**，不是原生Terminal窗口；输入也真实发送到PTY。PPT图片来自真实文件的LibreOffice/PDF渲染。Magpie的[两张原生截图](native/README.md)单独标注，后续本机锁屏使原生窗口追加截图不可用。没有把headless截图称为原生窗口截图。

| 过程 | 查看 |
| --- | --- |
| 工具完成 | [终端回答](terminal/20261014-193912-aafaafae/02-live-tools-answer.png) |
| 审批拒绝/允许 | [拒绝](terminal/20261014-193912-aafaafae/03-deny-approval-deny-page-1.png)、[允许](terminal/20261014-193912-aafaafae/04-allow-approval-allow-page-1.png) |
| 真正运行中的长任务 | [进度](terminal/20261014-193912-aafaafae/06-long-bash-running.png)、[补充指令](terminal/20261014-193912-aafaafae/07-followup-submitted.png) |
| PPT生成和修改 | [生成完成](terminal/20261014-193912-aafaafae/14-pptx-created.png)、[修改完成](terminal/20261014-193912-aafaafae/16-pptx-edited.png) |
| 压缩及记忆 | [生产边界](terminal/20261014-193912-aafaafae/19-context-compaction-boundary.png)、[压缩后精确回答](terminal/20261014-193912-aafaafae/21-post-compaction-recall.png) |
| 取消/拦截/新会话 | [警告](terminal/20261014-193912-aafaafae/11-escape-cancelled-stable.png)、[输入拦截](terminal/20261014-193912-aafaafae/12-cancel-fenced-prompt-warning.png)、[新会话真实回复](terminal/20261014-193912-aafaafae/14-after-cancel-fresh-session.png) |

[生成的v1 PPT](terminal/20261014-193912-aafaafae/presentation-before.pptx)、[修改后的PPT](terminal/20261014-193912-aafaafae/presentation-after.pptx)、[四页渲染PDF](terminal/20261014-193912-aafaafae/pptx-rendered/presentation-after.pdf)、[OOXML与渲染检查](terminal/20261014-193912-aafaafae/pptx-review.json)。v1 SHA256 `5211e2e7de3e6b9f7e27700c0aacaa1e4251ca0950a52ba60d99dcef841ce3a1`；v2 `36e175504b3eddf40ccfc4901887559dd3e6b393d6259d04caa76611323334b6`。13个editable text shapes、零picture shapes，workspace文件与v2逐字节相同。页面明确标注synthetic QA inputs，不是产品指标；主代理及独立代理逐页查看未改动1601×900图片，文字完整，无截断/重叠，REVISED可见。

## 修复与功能回退检查

| 修复 | 行为与保护边界 |
| --- | --- |
| 搜索生命周期证明 | Grep/Find报告实际helper join或同步inline join；默认空whitelist完成注册不丢失；没有真实证明不推定完成。 |
| 工具响应循环判定 | 比较内容、工具名及稳定JSON参数，忽略随机call ID；空文本但不同工具请求可继续，真正重复仍拦截，大小限制保留。 |
| 超时分类和配置 | 本地model deadline不误当用户cancel；后续轮可继续；新增可选有界超时，未配置默认保持。 |
| 长任务进度 | canonical started/result按精确run/assistant/call/index更新；并行任务未全部完成仍保留进度，stale/duplicate不复活旧spinner。 |
| unknown取消恢复 | 仅精确owner join/quiescent/选中terminal/session/run/generation后提供显式新会话；旧风险不改写、不自动重放，其他未解决marker仍阻止变更。 |
| Bash启动前拒绝 | 只对可信本地Bash的同步policy拒绝提交no-process completion；真实启动后仍须owned process group join，未知路径不转为成功。 |
| 持久化压缩 | 估算provider实际可见角色、工具参数及包装；durable超预算必须typed summary，不以micro/truncation绕过来源合同；保留系统与当前输入。 |
| 摘要正文预算 | 持久化切点预留正文上限+角色+untrusted前缀；最终实际结果预算仍校验，非法来源/current input/unknown仍拒绝。 |
| 远端预算整合 | 保留remote595d059 spinner/test改进；元数据校验保留、计数只在压缩器进行一次；不以全部原历史metadata提前拒绝可压缩请求。prepared/overflow精确预算12回归通过。 |
| ACP关闭边界 | EPERM仅在实际进程组已消失且探测无错误后忽略；持久权限错误、活进程组、未join及强制关闭仍报错。 |

[详细源码和回归审查](root-review.md)、[安装/修复诊断](magpie/diagnosis/)、[独立源码审查](review/)。所有修复提交与源码身份以git及问题账本为准，子代理没有提交。

## 全量门禁

两个门禁均在干净提交bdb76ef执行，前后HEAD和生产源码差异相同；28个根项目package，无QA临时生成器混入。

| 门禁 | 结果 |
| --- | --- |
| [make verify](gates/verify.json) | 407.634秒；8861 pass、0 fail、6 skip；vet/lint0/build通过；scan0 ERROR、1807 WARN；log/leak gates通过。 |
| [CI同版本拼写检查](gates/typos.json) | typos1.32.0，exit0，0typos；原始采集数据按字节保留，文档/脚本检查。 |
| [make test-e2e](gates/test-e2e.json) | 170.323秒；4856 pass、0 fail、6 skip，7个包；race开启。 |

[受保护文件对比](protected-files.json)：Makefile、CI、golangci-lint、根go.mod/go.sum及scanner规则未改；远端595d059的_typos.toml QA片段白名单原样保留。ignored workspaces独立Go模块隔离模型生成程序，不改变根检查范围；[原始门禁日志](gates/)保留早期失败及最后通过，扫描WARN如实保留。

## 真实性边界与复现

透明观察代理只加入会话关联元数据，逐字节转发真实响应；没有替换模型输出，也没有父代理代写PPT。每次响应单独保留upstream响应ID、状态、served provider/model、finish/DONE、Magpie route/usage/cache精确关联；暴露的ID不符时失败，不用时间顺序掩盖。captureBridgeSequence是本地捕获标签，不是网关返回序列。内部摘要与interactive请求分别分类，失败或取消请求不算完成。

本次显式local max-tokens128000（触发预算102400）、10m/4m长请求profile；这不是模型原生context window测量。压缩coverage/info_loss是item数量/token削减比率，不是语义保真评分；已证明的是本测试的来源边界、当前输入和early fact，不声称任意历史事实全部保留。取消不表示副作用回滚；本测试仅证明已启动延迟副作用被终止，并在unknown保留的情况下显式新会话恢复。未声称原生PowerPoint客户端操作、所有操作系统/模型组合或零扫描警告。

[终端复现脚本与环境](../../../../scripts/e2e_live/README.md)、[产物验证脚本](../../../../scripts/e2e_live_review/)、[计划](plan.md)。只需本机Magpie的已登录Codex及构建/渲染依赖，工具用最小子环境和隔离HOME/workspace；此轮验证审批及命令策略，没有建立操作系统沙箱隔离验收；凭据、运行二进制、依赖缓存和私有home不提交。

来源：[Magpie项目](https://github.com/yetone/magpie)、[官方Codex app-server接入说明](https://developers.openai.com/siwc/token-sharing-open-source/codex-app-server)。推送记录见[push.json](push.json)，最终git远端HEAD另由主代理实际核对。

原始PTY、自动生成的viewport摘录/报告、source patch及门禁日志按原字节保存并以哈希验证，显式加入Git；QA目录的ignore/attributes仅把这些不可编辑采集内容排除于文本格式/拼写处理。人写文档和脚本继续检查。渲染器profile、pycache、私有HOME、二进制与依赖缓存不提交。
