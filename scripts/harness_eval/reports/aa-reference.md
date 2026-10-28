# Artificial Analysis 参考核查与分层测试建议

本页保留资料核查时的工程阶段描述；最新main、profile与已执行门禁以[当前发布审核](release-review.md)为准，旧C诊断不代表当前源码仍有同一缺陷。

核查日期：2026-10-28。仅查官方公开资料和实现，没有新增收费模型请求、安装 AA 运行依赖、上传结果或更改进行中的评测代码。当前 Navidrome 的官方成绩与原始 attempt 继续独立保留。

## 先分清四种问题

| 问题 | 应测对象 | 本项目适合的证据 |
|---|---|---|
| 代码到底做对没有？ | 模型与 Harness 的完整闭环 | SWE-Bench Pro V2 的 fresh 官方 patch replay + 原验收 |
| 多轮、并发工具、取消、恢复是否可靠？ | Harness 的运行时契约与状态管理 | 无费真实 CLI fixture、合成故障、单元/race/恢复测试；另用新任务检验泛化 |
| 等量工作需要多久？ | 模型服务、请求形状、缓存、工具与客户端开销 | AA 固定轨迹回放；另测 Go/Pi 对相同本地 fixture 的客户端开销 |
| 修复以后有没有退步？ | 冻结版本间的差异 | 原失败保留 + 无费回归 + 未调优留出题，而不是反复刷同题最高分 |

一次短暂崩溃可能便宜而快；它不能因此胜过成功交付。四类证据应分别报告。

## aa-agentperf-local 实际是什么

官方项目：[aa-agentperf-local](https://github.com/ArtificialAnalysis/aa-agentperf-local)。本次读取固定源码提交 **`1fe80568c1e4b81b338ef276ccc6874a36d3704d`**（2026-10-15），`pyproject.toml` 版本 **0.3.4**；Python 3.12+，代码 Apache-2.0，AA 名称/标志不随代码授权。正式试跑应固定此提交和 `uv.lock`，不能一边运行一边跟 main 更新。[固定版本 README](https://github.com/ArtificialAnalysis/aa-agentperf-local/blob/1fe80568c1e4b81b338ef276ccc6874a36d3704d/README.md)、[包元数据](https://github.com/ArtificialAnalysis/aa-agentperf-local/blob/1fe80568c1e4b81b338ef276ccc6874a36d3704d/pyproject.toml)。

它把录制好的多轮请求顺序送到一个 OpenAI 兼容模型端点。默认 `agentperf-default-v1` 有 **8 个任务、168 个模型回合、172 个录制工具动作**，包括 PinchBench 风格任务与三个 SWE-bench 轨迹；`aa-mini-v1` 是 6 回合安装检查。完整标准回放要求至少 65,536 token 上下文，mini 为 8,192。工具模式可跳过时间、等待录制时长或在 Docker 运行录制命令。[架构](https://github.com/ArtificialAnalysis/aa-agentperf-local/blob/1fe80568c1e4b81b338ef276ccc6874a36d3704d/docs/ARCHITECTURE.md)、[默认 manifest](https://github.com/ArtificialAnalysis/aa-agentperf-local/blob/1fe80568c1e4b81b338ef276ccc6874a36d3704d/agentperf_local/data/replays/agentperf-default-v1/manifest-arm64.json)。

关键在于，**后续上下文与工具动作取自录制，而非新响应决定**。代码 `_request` 读取当前 trace row 的完整 `messages`；`_live_tool_replays` 执行 `recorded_tool_calls_after`。因此即使新模型生成不同答案，回放也不会像真实 Agent 一样沿新决策探索。它测请求和工具回放速度，不测新模型能否完成原任务。[runner 实现](https://github.com/ArtificialAnalysis/aa-agentperf-local/blob/1fe80568c1e4b81b338ef276ccc6874a36d3704d/agentperf_local/replay/runner.py)。

| 项目能力 | 可以说明 | 不能据此说明 |
|---|---|---|
| output tokens/s、首 token/回合时间分布 | 此固定工作负载在此服务上的客户端观测速度 | Agent 能力、任务成功率、完整多用户吞吐容量 |
| F0 传输/响应结构检查 | SSE 能解码、工具调用结构可用、终止原因合规 | 工具选得对、代码正确、任务真的解决 |
| live 工具回放 | 录制命令在该环境中的真实工具耗时 | 当前模型自选动作正确；Go/Pi 原生工具实现优劣 |
| exact 输出长度 | 等量输出负载的较可比延迟 | 正常自然结束时的真实任务效率 |
| 模型/硬件/文件哈希绑定 | 本地证据内部一致 | 远端确实加载指定权重、硬件身份已被独立认证 |

默认单并发顺序回放；不要称之为已经测过并发饱和/QPS。`summary.json` 最后写入；缺失测量为 null，不能当 0。结果格式保留原始与归一化时间，成功回合仅代表传输和配置的回放检查通过。[结果格式](https://github.com/ArtificialAnalysis/aa-agentperf-local/blob/1fe80568c1e4b81b338ef276ccc6874a36d3704d/docs/FORMATS.md)。

## 能否直接接 Go CLI 和 Pi

能接它们使用的**模型端点**，但当前 `run --base-url ... --model ...` 接口不是 Agent CLI adapter。把两份各自生成的轨迹拿去回放，可以研究它们所产生的请求负载；轨迹不同就不是严格等工作量的服务延迟比较。若把固定轨迹套进 Go/Pi 并替换其原生循环，则测的也不再是这两个原生 Harness 的任务能力。

本项目可沿用两条独立路线：

1. **真实能力路线**继续让 Go/Pi 原生闭环解题，同一模型/预算/外部环境，用官方 verifier 判结果。
2. **性能诊断路线**用一份相同、冻结的公开/合成轨迹比较服务条件；用一份共同的本地 mock 多轮工具协议检查 Go/Pi 的启动、dispatch、持久化和收尾开销。后者是本项目性能 fixture，不能冒称 AA 标准成绩。

当前审计 relay 只保留安全元数据和哈希，没有保存完整 prompt；它不是可直接交给 AA converter 的完整 recording。AA converter 要求包含 `model_call`、`request_messages` 或 provider request 等字段。未来若要导出，需专门的 recording 适配、脱敏和 schema 验证；本次不捏造已经具备该能力。[converter](https://github.com/ArtificialAnalysis/aa-agentperf-local/blob/1fe80568c1e4b81b338ef276ccc6874a36d3704d/agentperf_local/workload/recording.py)。

## 可以先运行的最小 AA 检查

以下是已核对参数定义的**待执行示例**，本次没有运行。放在独立目录，使用自有无费本地兼容服务；先执行 `--help`，不要把正在跑正式评测的 relay 改作新工作负载。

```bash
git clone https://github.com/ArtificialAnalysis/aa-agentperf-local.git
cd aa-agentperf-local
git checkout 1fe80568c1e4b81b338ef276ccc6874a36d3704d
uv sync --frozen
uv run agentperf-local run --help

uv run agentperf-local run \
  --base-url http://127.0.0.1:8080/v1 \
  --model local-served-model \
  --replay aa-mini-v1 \
  --output-token-policy recorded \
  --sampling custom \
  --tool-mode none \
  --output-dir results/mini-check-001
```

这条命令是 mini 安装/传输检查，**不是可对照完整 AA 默认回放的分数**。标准 `exact` 使用非 OpenAI 标准字段 `ignore_eos`，会先检查服务是否遵守；`recorded` 允许自然结束并估算归一化时间，须与 exact 分组。默认 standard sampling 注入 temperature=0.7/top_p=0.8/top_k=20/min_p=0；对当前 high 推理服务不能直接照搬。因此例子用 custom 且不显式填采样值；如果以后接当前模型，仍需预检有效 reasoning/output 契约并获得父上下文统一批次安排。[参数实现](https://github.com/ArtificialAnalysis/aa-agentperf-local/blob/1fe80568c1e4b81b338ef276ccc6874a36d3704d/agentperf_local/cli/parsers.py)、[配置实现](https://github.com/ArtificialAnalysis/aa-agentperf-local/blob/1fe80568c1e4b81b338ef276ccc6874a36d3704d/agentperf_local/replay/config.py)。

先用 none，避免为服务性能检查下载大量工具镜像。完整 live 模式要预建镜像、固定网络与架构，防止拉取/编译混入计时；这与当前 Pro amd64 仿真 profile 不同，不能跨 profile 混合延迟。只运行本地结果，无需 `submit`。

## Coding Agent Index 与其它 AA 公开部分

2026-10-28 打开官方方法页与榜单，当前 **Coding Agent Index v2.0（October 2026）**：FrontierCode v1.1 Main、Terminal-Bench 4.0、SWE-Atlas-QnA-AA，分别 100/66/124 题，每题三次，先题内平均再跨题平均，三组件等权。它评的是 Agent 配置；不能拿旧 v1.5 的 DeepSWE 组件解释新分数，也不能把三次的均值写成挑三次最好一次。[当前方法](https://artificialanalysis.ai/methodology/coding-agents-benchmarking)、[当前榜单](https://artificialanalysis.ai/agents/coding-agents)。

AA 还披露 provider 安全拒绝可重跑最多十次，模型主动拒绝则直接记零，以及某些 Agent 会 fallback 到另一模型。本项目“同一模型比较”不能照搬这些行为：任何重跑与换模型都要显式隔离，失败请求仍计费用。它的透明披露有参考价值，不等于这些规则天然适合我们的实验。完整Index另有私有任务与裁判，公开方法并不代表完整本地可复现。

| 工程/数据 | 能本地用什么 | 对本项目的价值与限制 |
|---|---|---|
| [Stirrup](https://github.com/ArtificialAnalysis/Stirrup) | MIT，Python 3.12+；当前源码 `c092cee80ce09cd1dfb93aef053296a9ecd9dbb6`，包版本 0.2.1；本地/Docker/E2B执行、工具与上下文管理 | AA 开源的 Agent Harness，可作第三个原生对照或参考生命周期；它不是整个 AA 私有评测流水线，换成 Stirrup 也不是在测 Go/Pi |
| [SWE-Atlas-QnA-AA](https://huggingface.co/datasets/ArtificialAnalysis/SWE-Atlas-QnA-AA) | Apache-2.0，124题、固定repo/image；公开裁判配置。需要编写/核对CLI接入并承担裁判成本 | 测代码库理解与可验证解释，补充只看patch的盲点；AA保留全部rubric通过才过的规则，并调整部分措辞、使用五票裁判。独立参考答案/评分材料必须与Agent隔离 |
| [Terminal-Bench](https://github.com/laude-institute/terminal-bench) | 固定4.0任务资产与官方运行/验收框架；具体硬件依赖各题检查 | 补shell/环境操作、多工具与系统任务；选CPU可跑小层，不能挑完只报全榜名次。沿用原手册的固定版本与网络控制 |
| [FrontierCode 官方说明](https://cognition.com/blog/frontier-code) | 当前Index使用的Main100题未公开，不具备完整本地复现条件 | 借鉴blocking正确性、回归、范围与测试质量；不能声称本地复刻其官方分数，也不能用发布者自述的质量优势替代独立审计 |
| [DeepSWE v1.1](https://github.com/datacurve-ai/deep-swe) | Apache-2.0，113任务，Harbor格式；v1.1要求提交patch后fresh verifier，官方推荐支持该收集链的Pier | AA旧Index组件，适合作为下一阶段长程编码补充；与同名DeepSWE模型/训练工程分清。须另审verifier与当前依赖，先作empty/oracle黑箱控制再跑；不是今天Index的新组件 |
| [AA-LCR](https://huggingface.co/datasets/ArtificialAnalysis/AA-LCR) | Apache-2.0，100个长文档问答；[Harbor adapter](https://github.com/harbor-framework/harbor/tree/main/adapters/aa-lcr)另有剔除与答案修正，不能混称原版 | 测长上下文跨文档推理，补充资料保留/检索；长输入不等于长时间自主任务。上下文/裁判预算较高，优先合成无费恢复fixture |

Stirrup 的 README、许可证和包版本已通过 GitHub connector 查核。[固定 README](https://github.com/ArtificialAnalysis/Stirrup/blob/c092cee80ce09cd1dfb93aef053296a9ecd9dbb6/README.md)、[许可证](https://github.com/ArtificialAnalysis/Stirrup/blob/c092cee80ce09cd1dfb93aef053296a9ecd9dbb6/LICENSE)。AA 的模型 Intelligence Index 也使用 Stirrup 跑部分任务，但它与保留各产品原生Agent的 Coding Agent Index 是不同实验；不要混用名次。[AA 模型方法页](https://artificialanalysis.ai/methodology/intelligence-benchmarking)。

## 最小多轮改进流程

| 轮次 | 固定内容 | 最小测试 | 报告与升级门槛 |
|---|---|---|---|
| 0：接入与运行时 | 模型配置、原生prompt/tools、版本与wire契约 | 无费真实CLI：read→final；单git_status→final；并行bash/read/status/read；取消/后代/unknown-proof负例 | 全部通过再花钱。mock只证明链路，不证明编码能力 |
| 1：当前失败回归 | 保留B二进制、首Nav记录和官方评分；C另记diff/bin SHA | C重放合成回归，再做Nav诊断回归 | B失败不得删除；C见过Nav故障后，Nav只作回归，不作完全留出题 |
| 2：留出正确性 | 其余两道既冻结Pro题、相同资源/model/high/预算与fresh评分 | C vs Pi按原顺序/随机种子执行 | 原样计失败/timeout/费用；两题仅为小样本证据，不作总体显著结论 |
| 3：跨类型泛化 | 事先选定CPU可跑TB任务与代码库Q&A小层 | 对每类独立pair/repeat；Q&A单独审裁判稳定性 | 按任务家族报告；不要把Q&A部分分与二值patch结果平均成自创AA指数 |
| 4：长程与恢复 | 冻结合成多阶段目标/中断点/验收；另选新鲜长程任务 | checkpoint→进程退出→resume，重复/失序工具、上下文压力、取消收尾；后续DeepSWE小层 | 同时检验最终结果与无重复副作用/资源残留；不能只凭多token或耗时自称长任务能力 |
| 5：性能回归 | 同一AA轨迹、同服务/缓存profile、同输出策略；另留CLI mock性能fixture | 先mini预检，完整固定轨迹重复；热/冷与并发实验分开命名 | 原始TTFT/每回合/工具/总耗时、p50/p95、错误与资源一并记录；性能不替代能力成绩 |

每轮只根据可泛化故障修改产品，先红再绿，并保留新的未调优留出题。首次首题暴露的 git_status 完成证明缺口应扩大**通用运行时回归**覆盖，而非向编码提示加入题目专用技巧。git_diff/git_log 同类注册缺口已只读提醒 Builder；确切修复与独立审核见 [运行时根因复核](runtime-quiescence-review.md) 与 [发布审核](release-review.md)。

公开报告应并排给“任务通过率/失败原因、每成功交付摊销成本、完整任务耗时、运行时可靠性、固定负载速度”，并标记模型/服务侧不可见信息。AA方法值得借鉴的是版本绑定、任务分层与过程证据；不能由一个开源回放工具推断整套官方榜单已完整开源或可在本机无成本复现。
