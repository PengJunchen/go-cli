# 计划、扩题锁与新main迁移复核

创建：2026-10-28；发布迁移复核：2026-10-29。这是Designer产出后的文档/元数据复核，不替代独立Reviewer对D代码、网络、实际测试及付费运行的门禁。

结论：文档已从本地checkpoint `65cf537`迁移到基于main5736e88的发布工作树，按PR9新能力更新；没有把旧base风险当新main事实，没有继承旧PASS为D通过。B0/Pi1保留，C仅无费未评分，D完整commit/diff/binary与新base验收pending，见[版本账本](version-migration.md)。

| 复核项 | 结果与限制 |
|---|---|
| R0–R5 | 保留六轮进入/AC/退出与小批失败停修；先复用新main真实二进制、120轮、受管工具journey，再Nav开发回归/未见小批 |
| 新main覆盖 | Git所有默认类型与owned helper、state/worker、durable子Agent/MCP/hook、会话管理和PTY输入owner已有实现/测试；旧“尚未实现”章节已替换 |
| 测试链接/命令 | coverage/R2改用main真实reviewed_runtime/durable_reviewed系列；不依赖未移植的C tools测试文件。新D额外CLI回归仅在最终文件/commit确认后登记 |
| 长任务边界 | 新main已有durable截断marker+三次prompt入口重开，不再称只有core单summary；仍不冒充100+轮独立二进制多次压缩/事实保留/跨进程副作用恢复 |
| 受控profile/辅助调用 | 产品memory默认on；首批fixture及正式D均明确memory-off/fresh HOME，第三方extensions/skills/MCP未加载，核心工具/session/compaction保留；偏离产品全部默认配置，memory-on另profile后测。所有实际辅助调用计账，不凭空假定没有 |
| 扩题选择 | 原seed/hash排序与4/3/3配额不变；额外10题排除原3，3development+7holdout，后者批次3+3+1；未解封、未跑模型 |
| 元数据复核 | 原只读复算task tree、instruction/task.toml OID全部匹配；独立Reviewer亦用固定git tree复算78/52/58候选与全部OID，0问题；未读新增指令/gold/tests或拉新镜像 |
| 历史结果 | round-01标明B历史、实际Pi→Go、B0/Pi1、分段耗时与usage/hash；数值未因base迁移改变。C/D无分数，不混均分 |
| 发布边界 | Markdown链接指向主repo入口/reports/真实源码，本地rawjob仅code路径；未复制原始轨迹、候选解答、隐藏断言、凭据或二进制 |
| 本分项操作 | 仅迁移/更新本目录文档与不变JSON；没有修改Go、规则/CI/保护文件，没有运行tests/模型或自行git stage/commit/push |

扩题锁SHA256仍为`2e8f4f9409f2ce4d61d5f59651802f3486f446325f2b1646622380c51bbd456b`。公开材料置于正常跟踪的`docs/evaluation/`；不修改gitignore、不force-add内部`docs/iterations/`或`docs/research/`。

旧Flipt暂停控制已归档infra-interrupted、reward=null并清理，不作empty0。Nav既有环境empty0/reference1和B/Pi独立评分保留，但D正式批次须按当前runner/profile重新确认环境门。当前不与代码/CI抢资源启动重型控制或模型。

后续阻断：D版本身份、有效模型参数、全量/目标/二进制/120轮实际日志与网络/usage门禁未齐前，不宣称认证完成；Flipt/Vuls fresh控制未过不跑正式模型；R5长项目ID仍待另冻。各轮checkbox保持未完成，不把计划表当验收结果。
