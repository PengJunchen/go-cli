# 入门：题库、框架与可靠性

资料核查时间：2026-10-28至29。这里的建议用于选实验，不把厂商自报或演示当作独立结论。

## 先把评测对象说清楚

可以把模型看作工程师的推理能力，Harness是它使用的工作系统：工具、上下文、重试、子Agent、会话、审批和恢复。评测框架则负责准备考场、计时和判卷。更换模型、Harness或判卷方式，都可能改变成绩。[Anthropic的方法文](https://www.anthropic.com/research/demystifying-evals-for-ai-agents)明确区分Agent Harness与evaluation harness。

业务上至少分开四个问题：

| 问题 | 该看什么 |
|---|---|
| 工作做对了吗？ | 独立验收后的任务成功率、回归和人工复核 |
| 能稳定做完吗？ | 多次重复、运行时错误、取消/恢复、人工接管 |
| 交付花多少？ | 包含失败和重试的token、缓存、费用与完整耗时 |
| 等量请求有多快？ | 固定工作负载的首token/回合延迟、服务吞吐；不替代任务正确性 |

不要把四项随意加权成一个自创“总分”。便宜但没交付、偶尔成功但经常崩溃，业务价值都不同。

## 哪些题库值得先用

| 题库/方法 | 主要测什么 | 用法与限制 |
|---|---|---|
| [Terminal-Bench 4.0](https://www.tbench.ai/news/terminal-bench-4-0) | 终端中的程序、数据、运维和系统任务 | CLI Harness的广谱补充；先筛本地CPU/内存可承载题，固定版本。子集不能冒充全榜 |
| [SWE-Bench Pro V2](https://github.com/scaleapi/SWE-bench_Pro-os/blob/main/v2/README.md) | 较复杂仓库的修复，导出patch后独立验收 | 本项目当前小规模Go试跑来源；锁定官方资产、网络边界与fresh评分，不因新版就假设评分无缺陷 |
| [SWE-bench Verified](https://www.swebench.com/verified) | 历史真实issue修复 | 生态成熟，适合作历史坐标和适配器检查；污染与题目质量限制使它不适合作唯一能力证明 |
| [SWE-bench-Live](https://github.com/microsoft/SWE-bench-Live) | 按时间更新的现实问题 | 用新鲜日期窗口互证，按语言/仓库分层；新鲜不能证明底模从未见过 |
| [DeepSWE](https://github.com/datacurve-ai/deep-swe)等较长工程任务 | 更完整的特性与修复、持续探索 | 后续补长程复杂度；先核环境和评分，成本较高，不把跑得久等同可靠 |
| [SWE-Atlas-QnA-AA](https://huggingface.co/datasets/ArtificialAnalysis/SWE-Atlas-QnA-AA) | 读懂代码库并给出正确解释 | 补充只看patch的盲点；有LLM裁判成本和一致性问题，须锁裁判配置并盲审 |
| [METR时间跨度方法](https://metr.org/time-horizons/) | 成功率随人类参照任务难度变化 | 借指标逻辑；没有人类耗时标注，就不声称自己的题等于人类几小时 |
| 自建保留题 | 本项目的压缩、恢复和副作用约束 | 公共榜看不到的能力；题目与验收分开保存，不能用正式题答案造回归 |

题库提供题目；[Harbor](https://harborframework.com/docs)等框架负责沙箱、调用Agent和验收。直接运行框架自带Agent，测到的是那一套Agent，不能算Go/Pi对比。Promptfoo、DeepEval、EvalScope等也能接Agent或做组件评估，但仍需定义任务、CLI接入、隔离和验收，名字本身不保证公平。[Promptfoo编码Agent接入](https://www.promptfoo.dev/docs/guides/evaluate-coding-agents/)、[DeepEval Agent评估](https://deepeval.com/docs/getting-started-agents)、[EvalScope](https://github.com/modelscope/evalscope)。

## 怎样看“牙医”和厂商评测

本次所指牙医是 **karminski-牙医**。[他的演示题库](https://github.com/karminski/awesome-llm-benchmark-prompts)适合学如何把产物变得肉眼可见；[向量库工程题](https://github.com/KCORES/vector-db-bench)则说明可以先检查正确性，再比较性能。重点看能否复跑、是否公布失败、是否独立验收、几次取最好还是求平均。漂亮录像不足以支持总体排名。

智谱、Kimi、DeepSeek、OpenAI、Anthropic和Gemini的发布分数也应逐条核对：具体模型、Harness、thinking预算、输出上限、工具/网络、每题尝试数、额外重试、裁判及数据集版本。未披露就写未知；不能把不同条件的数字排成我们自己的公平榜。公开权重、公开题目、公开执行器与完整可复现是四件事。[AA官方Agent方法](https://artificialanalysis.ai/methodology/coding-agents-benchmarking)也把产品配置作为行，并区分任务成绩和效率；其当前Index含私有部分，不能声称已完整本地复刻。

可靠性可以分三层：**可查方法并能按锁版本独立复跑**最有用；**厂商自报但参数充分**可作参考；**未披露条件、只展示最好一次或主观打分**只作线索。这不是按名气判真假，同一发布者的不同实验也可能处在不同层。

## 长输入与长任务不是一回事

长文档问答测信息提取和推理；100轮mock测工具/上下文机制；真实长工程任务测持续探索和交付。三者都重要，但不能互相替代。恢复还必须检查中断前后的事实、待办和文件是否保留，写操作是否重复，资源是否清理；不能只看“最后出现完成文本”。接下来按[本项目方法](methodology.md)执行。
