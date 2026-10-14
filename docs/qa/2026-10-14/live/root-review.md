# 主代理规划与审查记录

2026-10-14，整体验收仍在进行；本记录区分已审查的局部事实与最终门禁。

## 取消恢复 · 7d5dfd0

审查了 REPL 集成、私有证据校验、命令处理与回归测试。只有明确 UI 取消、父上下文仍有效、实际 owner join、quiescent + unknown、精确物理会话/run/owner generation 发布、唯一选中 terminal 和空闲 run slot 才允许提示 `/session new`。普通提示在此期间不调用模型；新会话命令再次验证绑定，旧 receipt 和 terminal 不改写，不自动重放。既有有效 local-resource resolution 可能已经解除资源风险；其他未解决的 workspace marker 仍阻止变更。未放宽 Core 或 TUI 的 unknown 校验。

执行代理报告 CLI/Core race、vet 和 CLI lint 0 issues；独立审查代理未发现阻塞问题。真实最终 Esc→新物理会话→新模型响应复测仍待完成。

## Bash 策略拒绝 · 0c1242e

审查了可信注册模式、完成信号、normal/streaming Bash 明确 `Sandbox.Validate` 拒绝路径和两个回归文件。仅在进程启动 helper 之前的同步拒绝处提交 inline completion；已经启动的进程仍必须证明 owned process group joined。没有报告、未 join、混合报告、强制管道关闭不变成已证明完成。真实 durable REPL 集成测试核对错误回执、未产生文件、之后的模型请求与新用户轮；该测试使用模拟 provider，不能替代真实推理证据。

执行代理报告完整 tools/CLI normal、race、vet 通过；绝对路径 lint 0 issues。真实最终拒绝命令→模型解释→下一轮复测仍待完成。

## 真实 PPT 局部验收 · 20261014-174155-f895248a

该诊断轮使用旧 pinned race binary（源码与差异以 run-report 为准），整轮因后续 compaction 错误保持 failed，不能作为最终版本全场景通过。PPT 文件本身由 Dot 的真实 Write/Bash 工具生成、下一用户轮经 Edit/Bash 修改，独立 OOXML 检查通过：四页、13 个 editable text shapes、零 picture shapes，精确 fixture 数据与声明保留；只有 slide 4 的 INITIAL→REVISED 变化。

主代理通过 view_image 逐页查看了未改动的实际 LibreOffice 渲染 PNG，原始尺寸均 1601×900。四页文字完整，标题/正文/声明无截断或重叠；边距一致、对比清晰；第 4 页清楚显示 REVISED。内容为合成 QA 测试输入，页面声明可见，不是产品指标。通过的是此产物结构与视觉检查，未声称原生 PowerPoint 客户端操作或整轮通过。

v1 SHA256：`202165d15d777b86694555d12946f285a79ab724f104fb7462ce986c2b97883a`。v2 SHA256：`11a97de1cb7575c6c0f41b214bf9d3bd1cc2ac794cae2d19b9686372c8ce940e`。v2 与真实 workspace 产物逐字节一致。参见 [独立检查](terminal/20261014-174155-f895248a/pptx-review.json)、[修改后 PPT](terminal/20261014-174155-f895248a/presentation-after.pptx)、[实际渲染](terminal/20261014-174155-f895248a/pptx-rendered/)。

## 持久化压缩 · 49dbf97

审查了 provider-visible 文本计数（角色、名称、call ID、JSON 参数以及 content-block 优先级），来源明确的历史在超预算时直接选择带 typed projection 的 summary；无 summarizer、摘要失败或无 projection 均不回退为不可信的 micro/truncating 投影。旧的非持久化 micro-first 路径保留。来源明确的防御副本清除过时缓存，原始输入不改动；不能 JSON 编码的参数图保守计为 MaxInt，累加与摘要切点使用饱和加法。

核心来源、当前用户输入及 durable commit 的生产校验未改动。新测试在旧已完成 Write 工具组上建立合法摘要边界，核对 envelope/compaction 估算一致、精确当前输入保留与唯一 projection commit；对当前输入无法完整保留的预算仍拒绝提交。审查修正了工具组测试的原子边界：预算 26 可容纳当前用户 8 加一条回执 18，但不能容纳完整 81 的工具组；因此错误地保留单条回执仍会被测试发现。

完整 compaction/Core normal、race、vet 和绝对路径 lint 0 issues 通过，独立审查无阻塞问题。最终注释更正后 gofmt/diff-check 通过，功能代码未再改动。干净源码 49dbf97 的仓库全量门禁与真实全场景复测正在进行；此处不提前判定最终通过。

## 第三次真实完整运行 · 20261014-184011-290bc55c

此轮基于干净已提交源码 78a7f48；30 次真实 Codex 6.1 完成响应经独立审查，响应 ID、Magpie route ID、session 和 usage/cache 精确关联。原始 report 保持 failed，不能将内部 nonstream 摘要请求（序列 30）当作第 4 个 pressure 用户轮。前三轮各约 30KB 的真实 bracketed paste 均到达 provider 并完成；第四轮只完成了生产摘要推理，tokens_after 102760 超过 102400，完整结果预算保护正确拒绝发布，后续 recall/取消恢复没有执行。

真实 PPT 生成及下一用户轮修改局部通过，canonical MIME、四页/13 editable text shapes/零图片、exact fixture 数据和单处 INITIAL→REVISED 更新均通过。主代理再次逐页查看了实际 LibreOffice 渲染 PNG（1601×900）：标题、正文、QA 合成数据声明完整，清晰，无截断/重叠，第 4 页显示 REVISED。此处只认定产物结构和渲染通过，不声称整轮通过。

v1 SHA256：`75e7b11c8f37af414aa5d69b86d892a4e10416a0eb1adc5f0355e367e9d13404`；v2 SHA256：`e01ea49a2a3637c942882432559c1eb000d9774cf35c8d33645d582b3b902aac`。参见 [原始失败报告](terminal/20261014-184011-290bc55c/run-report.json)、[独立审查](review/184011-independent-review.md)、[实际渲染](terminal/20261014-184011-290bc55c/pptx-rendered/)。

## 取消期间命令分派 · 1897678

注册表中的真实 HelpHandler 和 durable SessionHandler 决定允许的恢复命令，避免硬编码别名和 SCAN010 错误。nil、伪造 handler、legacy session handler、无恢复回调的 new、多余参数均仍拒绝；普通消息 fence 保留。完整 CLI race、vet、lint0 与 scanner0ERROR/1807WARN 通过；最终全量门禁及真实恢复仍待完成。

## 摘要正文预留 · ec267df

真实184011摘要推理成功但结果超预算，说明切点采用短 placeholder 不足以代表正文。修复仅在 durable 且正文上限为正时预留 projected user role、untrusted prefix 和 estimator-based maxSummaryTokens；按完整工具组搜索较大的合法前缀。最终实际结果预算校验、source/current-input/Unknown 校验和无合法 typed projection 时拒绝的策略保持。非 durable 的旧 placeholder 路径保持。

定向测试证明 cap20 与 budget112 的精确边界能保留完整当前输入；one-token durable fixtures 明确设置 cap1，预算7表示角色/前缀/正文各1，加系统/当前各2，不能使用旧低估预算6。oversized-current 负例仍拒绝。对于非可加 estimator 或空摘要 placeholder，最终实际校验仍拒绝超预算，未假定预留一定成功。完整 compaction/Core normal、race、vet、lint0 与独立审查通过。最终干净源码真实运行和全量门禁已启动，未提前验收。

## 最终源码真实 PPT 局部检查 · 190417

主代理逐页查看真实模型产物的四张实际渲染 PNG（1601×900）：标题、全部合成 fixture 文本和免责声明完整，边距一致，无截断重叠；slide4清楚显示REVISED。检查器报告canonical OOXML、4页/13 editable text shapes/零图片、只改slide4状态通过；独立代理复渲染及视觉检查也通过。完整运行仍在进行，工具回执/谱系及其余场景待最终报告核对。

## 稳定源码全量门禁 · ec267df

`make verify` 398.318秒通过：8855函数开始，8849通过、0失败、6预期skip，28包；vet、lint0、构建、scan0ERROR/1807WARN，以及额外log/leak gates通过。`make test-e2e` 171.316秒通过：4850函数开始，4844通过、0失败、6skip，7包。两个门禁前后HEAD及源码差异均相同且为空，未有未跟踪生产源码。详细原始日志/哈希在gates/。日志中的ERROR: boom是门禁负例的预期样例，不是全量失败；1807WARN仍如实保留，不称为零警告。

从e0493e5至最终生产修复，Makefile、lint、根Go模块、CI与scanner规则未改变；生成临时Go程序通过ignored QA workspaces下的独立模块隔离，原项目仍28包。信息损失指标是quality.go中token数削减比率，不是语义丢失评估；coverage是输出/输入item数。真实压缩验收另用typed entry/source boundary/current input保留与早期事实精确recall证明，不能从这两个比率推断任意历史事实全保留。

## 最终真实运行 · 191314-3ac94c9e

干净ec267df race binary SHA256 `09650e64b03c1341f516265e7d1f086945247a594b2e8259ec0acfbf71ec662c`，194/194检查、20轮记录、36完成请求（35interactive+1内部nonstream）、54截图、CLI0/raceclean。源报告SHA256 `36d453483795e6fdd0dcaea03eb4265a139c51b1a2a6a902d7518967c326177e`。主代理逐页检查PPT四页渲染及回忆、取消warning、fence/new/fresh真实PTY截图；结果与报告一致。

生产压缩61→10项、108034→33375估算，max102400；真实summary后同一durable entry含canary并覆盖Read回执seq106，后续最新输入保留及early key=value精确回复。旧物理会话s_ddaf28e3418c00d23b6b6c6dc3b93fc5取消时unknown/quiescent保持；普通输入不进入provider/用户历史。新会话s_ee19c93117d1e69eb0cd35b8c92dec6a的真实no-tool回复、seq1 durable user与success terminal绑定，旧receipt/terminal字段完整不变。原延迟文件未产生。

最终PPT v1/v2 SHA见README；主代理检查1601×900四页文字、QA声明及REVISED完整可见，无截断/重叠；所有产物由Dot真实工具生成修改，没有父代理修复或替代。旧190417只有184/185passed且缺fresh模型轮，仍failed；harness接受空新文件后的实际provider+durable证明完整，不能把旧失败重新分类为通过。

独立全场景证据审查最终PASS，详见review/191314-independent-live-review.md。35 interactive +1 internal摘要精确response/route/session/usage/cache关联；capture sequence明确为本地标签。无单独upstream requestId；未建立OS sandbox验收。审查器原固定上轮token数的误判已修正为实际阈值/策略/结果约束并重跑通过，未改原运行报告。

## 远端整合 · bdb76ef

最终推送遇non-fast-forward拒绝，获取远端595d059，保留其历史后合并。远端旧content-only metadata预留与本轮provider-visible计数语义重叠，自动文本合并仍会双扣预算，甚至在压缩可删除的旧metadata超过预算时提前拒绝。窄整合helper改名为preparedCompactionMessageBudget；元数据JSON/estimator/overflow校验保留，不再双扣/提前以旧metadata总量拒绝，既有multimodal reserve保持，最终envelope guard保留。

新回归原输入20、metadata16、目标预算12，压缩合法后完整system+最近tool组+current恰为12；普通prepared和provideroverflow两条路径均先确认旧helper拒绝、后确认修复通过，caller输入不变。remoteinvalid估算负例保持，直接helper测试30−7=23和12−7=5。独立Core/TUI普通测试、ownerCore/compaction/TUI普通/vet/lint0、Core/compaction race及changed legacy spinner race通过。owner完整TUI race在2m13无结果时提前中断，不能算通过/失败；root最终全量gate补齐该证据。

remotelegacy spinner在mutex内捕获Tick值接收者，防止异步命令读取后续状态；canonical durable进度路径未改。remote receipt同步测试、longtask失败诊断及_typos两份QA片段白名单原样保留，其他受保护文件不改。bdb76ef的完整真实实测与稳定源码全量门禁正在执行，191314只作为ec267历史验收。

## 合并版本最终验收 · 193912 / bdb76ef

194/194通过、36完成请求（35interactive+1内部summary）、20用户轮记录、55真实PTY截图，CLI0/raceclean，源码build前后无差异。新race binary SHA `b6744ff75ba13ce930f1b519a06b08fb045f539e5691edf719618d037ebac30e`，原报告SHA `f9654044de3f0a45ac1ba15e79dd2425677c3a2495564436060f08996a715449`。主代理再次逐页检查四张真实PPT渲染及压缩记忆/新会话回复图；布局完整、声明清晰，无截断重叠，REVISED和exact canary/fresh回复可见。独立全场景审查PASS，保留本地捕获序列/真实response-route ID的区分及无upstream requestId限制。

生产摘要108584→33247（61→10，max102400），同一typed durable摘要seq120绑定成功273673字节Read回执seq106且保留canary/boundary markers；后续latest prompt/recall真实完成。Esc资源quiescent但操作unknown保留，fenced普通消息无provider/用户行，明确新物理会话s_32aa72d3f943ed4ec351782ef6886e58后新真实无工具回复和durable success，旧状态不变。此轮没有使用父代理替代PPT或伪造LLM。

稳定源码makeverify407.634秒：8861pass/0fail/6skip、lint0、scan0ERROR/1807WARN，完整TUIrace已补齐。tagged E2E170.323秒：4856pass/0fail/6skip。两个门禁源码及HEAD稳定；前一ec267通过结果保留归档。bdb76ef已用非强制HTTPS快进推送并ls-remote核对一致，remote595历史保留。随后QA提交只增加证据及脚本，生产源码须与bdb76ef保持逐字节一致。

QA打包额外完成PR工作流同版本typos1.32.0：0typos/exit0。未改根_typos规则；QA-local ignore只区分immutable PTY/run-report/source patch/日志、自动viewport摘录及临时profiles/cache。raw记录显式跟踪、保留原字节，attr禁用text转码/diff空白校正；文档/脚本继续检查。index排除139项LibreOfficeprofile/Pythoncache，不提交私有HOME/deps/bin；工作区只加入安全module boundary和ignore。
