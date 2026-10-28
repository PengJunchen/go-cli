# 发布基线与评测版本迁移

更新：2026-10-29。公开发布基于已合并[PR9](https://github.com/PengJunchen/go-cli/pull/9)的main **`5736e8828c80913fd4a32dc7b6e22eb82e5ec2b1`**。旧工作分支与main大幅分叉；只移植本轮必要Thinking修复、评测自有源包与脱敏文档，不推送旧开发历史，也不回退main已有恢复能力。

## 版本账本

| 标签 | 源码身份 | 已有证据/评分 |
|---|---|---|
| A | 旧`379134d679dcae5c996b730eeae464fdb61fe853` | 无费接入发现Eino漏thinking；未做真实模型任务评分 |
| B | A+Thinking修复，差异SHA`4fb22ef54bb55d89b22c3e8d415fc0c14eb863d8af3381c1f0d2e0954d43639a`，binary SHA`749416f8a5bc20eb8d875aa54ed413a09e9c4602dbace04b51f3d294bd0777b5` | Nav首attempt官方0，Pi1.4.0官方1；历史永久保留 |
| C | 旧379+Thinking及Git只读聚合修复，最终payload差异SHA`4682a0ca3f2df258f2b153ac60f79b02a53fa4e70362e043709b9b9a26722f3c`，binary SHA`45776eb15e13dd7621bf218c06f23f1f4909a42666bb0c3b03183001f0288919` | 有无费单status/并行真实CLI与目标测试证据；**没有真实模型任务评分**，不能补造Nav分数 |
| D | 干净发布提交`5aa063ee647f753f7f0fc7282a3baf5b6e691932`，binary SHA`099f9b687a96737722ec7e0eec9264e00fe8f38488bc4f0e72ad39d76ba71894` | Nav开发回归官方1/CLI exit1/自定义完整交付0；37请求、完整usage；[第2轮报告](round-02.md)；不与历史Pi作同期pair |
| E/后续 | 如D实际暴露新通用问题，另冻修复版本 | 尚未建立，不预分配成绩 |

旧本地checkpoint为`65cf537`，只用于保留调查/移植来源，不是新发布commit。D已绑定最终40位commit、空diff SHA和payload清单；早期dirty构建与e8f候选清单仍独立保留，未回填旧job。后续若有dirty源码，必须同时给diff SHA与binary SHA，不能用base5736e88冒充完整身份。

## 新main带来的变化

- exact第一方Git全家族completion-only与受管`runOwnedCommand`、前台自动maintenance、未知merge停止probe、commit查询失败显式错误，已经合并。旧R2“g.run未受管”只描述旧379/C，不是D待重新实施的任务。
- state/owned worker、父子独立durable session、MCP discovery/call/unknown、post-tool hook独立action、会话历史管理已有更广测试；先复用实际跑，再补缺断言。
- durable截断已支持固定omitted marker与三次prompt入口重开投影；仍不能等同100+轮二进制多次压缩后的事实保留/副作用恢复。
- PTY已改等真正REPL输入owner marker；binary module root不再依赖被trimpath去掉的runtime路径。旧fixture补丁不能覆盖这些main修复。
- 自动memory产品默认开启，会改变请求与辅助工作。首批D明确采用`main-5736e88-personal-memory-off-v1`，**无费fixture和正式任务均memory.enabled=false、fresh HOME**；双方不加载第三方extensions/skills/MCP，保留核心原生工具/session/compaction，并核对实际广告面。这是受控profile，偏离全部产品默认能力；memory-on及可选集成另设profile后测。所有实际模型/摘要/子Agent调用仍纳入共同预算，不假定辅助配置为空就没有后台请求。

完整覆盖与待运行命令见[coverage baseline](coverage-baseline.md)，Git补验见[R2设计](r2-git-design.md)。这些判断来自只读源码/测试断言，迁移文档本身没有运行tests或模型。

## D进入收费运行前的门与实际状态

D完整verify、无费真实Linux工具/wire门、单独120轮机制及CI检查已通过；新Nav empty/reference校准分别0/1，随后只做一次Go开发回归与fresh官方评分。详细计数、失败和哈希见[第2轮报告](round-02.md)。以下门槛保留为流程，不能据此宣称全部工具、跨进程长任务或后续题已完成。

1. 新base上确认Thinking有效值，重建并冻结实际Linux payload，不混用B/C二进制。
2. 先实际运行现有真实二进制工具矩阵/两进程resume、受管local+bare remote journey与unknown资源负例、120轮SSE子项与durable截断重开。日志逐项记录started/pass/fail/skip；不把测试存在或旧C PASS当D通过。
3. 新工作树目标race/全量verify、外部Linux真实CLI single/parallel fixture、wire/model/high/output/usage和网络门禁按协调者资源安排重新确认。
4. 这些工程门通过后先一份Nav **D开发回归**并fresh评分，旧B0/Pi1单列；如Pi不重跑，明确是历史对照。再按冻结规则解封原Flipt/Vuls未见验证，逐题pair，失败先停修。
5. 额外10题`pro-v2-expansion-lock.json`保持原字节/SHA，不重新抽题：排除原三题，3development+7holdout，目前未解封、未跑模型。只因base迁移不能把Nav重新标成未见题。

跨版本、开发/holdout、不同profile的成绩不合并。对D修复若用到任务反馈，该题仍只能作开发回归；是否有总体改进，要靠后续尚未见反馈的冻结小批验证。
