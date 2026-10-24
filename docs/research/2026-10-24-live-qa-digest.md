# Go CLI 真实评测与失败记录摘要（2026-10-24）

本摘要提炼 2026-10-14 Magpie 真实 CLI 验收及 2026-10-15 广州持续对话评测的结论、修复路径、失败边界与后续事项。旧 `docs/qa/2026-10-14/live/` 与 `docs/qa/2026-10-15/guangzhou/` 含大量临时会话、原始日志、逐轮截图、渲染环境和运行输出；整理后删除这些原始运行数据。此处保留可供后续开发使用的结论，不把已删除的原始材料称为仍可复查的证据。

## 2026-10-14：真实 Magpie CLI 验收

最终记录对应源码 `bdb76efb0b31152e4c554268b43d39ef6285115a`。本机签名版 Magpie 通过 loopback 调用已登录 Codex；最终真实 CLI 会话记录 20 条用户轮、36 个成功上游推理请求、55 张终端图像，194 项检查全部通过。实际覆盖工具读写与搜索、审批允许/拒绝、Bash 策略拒绝、长任务期间补充输入、PPTX 生成及修改、生产上下文压缩和取消后显式新会话。历史会话中的未知副作用状态没有被伪装成成功或自动重放。

主要实现结论：工具回执只说明结果，不能单独证明外部资源已停止；durable run 要求具体进程组 join 或同步 inline completion 证明。模型请求的本地 deadline 与用户取消需分别分类。持久化摘要选择截断位置时，必须预留最终摘要正文、角色和不可信内容前缀的 provider-visible 预算，实际压缩后仍做最终估算。ACP 的 `EPERM` 只有在确认目标进程组已消失且探测无错误时才能忽略。

失败与修复过程保留如下结论：

- 实际 Grep/Find 曾返回正常工具结果，但 durable owner 仍报告 `quiescent=true, outcome_known=false` 并停止后续模型请求。首次修复覆盖有效 workspace whitelist，后续真实重放暴露默认空 whitelist 的另一条注册路径未带 completion metadata；修复分别保留既有范围/信任语义，只为被审查的同步路径增加完成证明。未知或异步自定义 registry 继续 fail closed。
- 一次真实流式请求超过 10 秒 idle deadline，CLI 将 provider-local timeout 记录为错误，旧终端分类却把 `context.DeadlineExceeded` 当取消，导致 transcript acknowledgement 不匹配。修复按 enclosing run context 区分请求 deadline 与 run cancellation，并提供显式有界 timeout 配置；框架未配置时仍为 30 秒总时限/10 秒流式空闲，Magpie launcher 单独配置 10 分钟/4 分钟。
- 一次摘要模型请求成功，但截断算法只为短 placeholder 留空间；真实摘要替换后 provider-visible 预算超过上限，durable projection 被拒绝。实现改为按配置允许的摘要正文上限预留预算，仍以最终真实结果预算为准，超限不提交。
- 自动化 harness 的多次失败分别来自响应/usage/route 关联不足、运行异常丢失部分证据、旧审批视图被当作当前 modal、TUI 完成标记过时、PTY 大输入阻塞、fixture 位于 Git ignored 目录、控制器状态错误以及 Bash 工具超时。修复了采集和判定逻辑；这些失败属于真实探索过程，不应与后续通过拼成一条“连续成功”。

最终 `make verify` 在干净 `bdb76ef` 上退出 0：8,861 通过、6 跳过、0 失败；`make test-e2e` 退出 0：4,856 通过、6 跳过、0 失败；拼写检查 0 typo。AST 扫描保留 1,807 条 warning。验证边界：单一主机、Magpie/Codex 路由与选定场景；PTY 字节经 headless xterm 渲染而非原生 Terminal 截图；PPT 使用 LibreOffice 渲染而非原生 PowerPoint；没有证明操作系统级 sandbox；显式 token 与 timeout 配置不代表模型 context 上限测量。

## 2026-10-15：广州持续对话与工具评测

原始正式比较使用共同的 30 秒总时限/10 秒流式空闲。它没有完成 104 轮：main 接受 33 轮、完成 32；develop 接受 49 轮、完成 47。main 在 T033 PPT 长推理后自然退出，develop 曾由评测器 Esc 辅助恢复并在 T049 因同步 ToolSearch completion proof 缺失退出。两组早期中断必须保留为失败，不由后续实验改写。

之后分开完成两项新实验：

- **B：纯文本配对 104 轮**。两边对相同提示词逐字节输入，同模型与 30 秒/10 秒参数，独立会话、交替顺序、禁止工具/审批/重试/重启。main 与 develop 都完成 104/104。独立审阅未发现明确实质内容错误；受最多 5 行格式约束影响，develop 101/104、main 103/104 严格通过。只在完成配对内计算的 develop-minus-main 完成耗时中位数约为 `-1.020s`，探索性 bootstrap 95% 区间 `[-1.215s, -0.773s]`。网关缓存、生成长度、供应商调度和轮次相关性仍是混杂因素，因此这不是稳定的性能排名。
- **A：develop 实际工具/PPT 104 轮**。不同于公平对比，采用 10 分钟总时限/4 分钟 idle，只测修复后的 develop。报告记录 104/104 轮成功、142 次上游推理、38 个唯一工具调用和 38 个 durable receipt；37 次审批中 36 allow、1 deny；模型通过真实工具创建并迭代五版 8 页可编辑广州旅行 PPT，40 页渲染逐页审查通过。该实验不能与 30/10 的 main 比速度或任务成功率。

评测还发现了中文提示中工具名紧贴中文时的检索缺口。为让正式对比能验证审批，提示词实验版临时在工具名两侧加空格；这只控制了测试输入，没有修复产品问题。之后 worktree 补上 ASCII 工具标识符边界匹配，新增相邻中文召回及 `thread`、`rewrite`、`foo_read` 等英文子串误匹配否定测试。`tool_search` 对未知 completion 的保守策略曾导致 T049 退出；后来只对明确检查过的同步本地 registry 链提交 inline completion proof，不给任意自定义/异步 `List` 实现授予证明。审批拒绝回执也增加有限值来源与选项字段，只有真实 TUI 按键记为 `user`，取消、无效响应和 callback 各自保留来源。

验证装置的失败也影响结论边界：旧 PTY renderer 的 8px padding 会裁底部，修正后增加屏幕边界采样；旧完成截图早于 TUI 收尾，后续先被动等待真实 idle/exit。LibreOffice 初次缺少中文字形，控制实验仅调整私有 `FONTCONFIG_FILE` 和字体 fallback 后显示正常，未改 PPT 文件。resume fixture 最初遗漏全局 dispatch authority，补入原字节后仍因父目录权限不符被安全检查拒绝；后续复制 authority 且恢复严格目录权限，才完成副本恢复验证。这些是测试装置问题，不是模型或生产功能通过的证据。

## 尚待处理

1. 为中文紧连工具名的召回、同步本地 registry completion proof 和 custom async registry fail-closed 各增加真实模型请求复验，并保存本次输入实际 advertise 的工具 catalog；当前提交包含源码回归，不将过去的 104 轮当作新补丁的实测。
2. 复核审批/策略失败时 UI 回执的先后标签歧义、重复空 assistant 标签、长会话侧栏密度；`Tokens used/0` 代表 context 容量未知，不应显示成已知零容量。
3. 如需速度比较，固定或多次抽样模型/路由和随机条件，并报告同任务配对及失败率；不可把单次 104 个不同任务当独立性能基准。
4. 如需对外声明 macOS 原生窗口、系统 sandbox、原生 PowerPoint、其他模型/主机或真实旅行库存/价格，需单独设计并执行对应验证。本评测没有预订，也没有证明报价实时可用。

## 保留边界

上述摘要概括历史实验，不保存逐轮用户提示/模型回答、session JSONL、trace、PTY 原始字节、构建与门禁日志、数百张屏幕截图、临时 worktree 或渲染环境。其余设计、动态运行时规格、复审、执行票据、测量摘要与可复现源码脚本仍留在仓库；本摘要把历史结论与新一轮代码变更的证据边界分开。
