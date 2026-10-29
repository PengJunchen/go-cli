# 当前发布审核

日期：2026-10-29。[Draft PR #10](https://github.com/PengJunchen/go-cli/pull/10) 基于恢复能力后的 main `5736e8828c80913fd4a32dc7b6e22eb82e5ec2b1`；旧379/C生产补丁未移植。已发布的Thinking修复和通用Git回归保留。本轮源码在HEAD `098c350` 加冻结改动时验证，随后由协调者分四提交发布至[081289b](https://github.com/PengJunchen/go-cli/commit/081289b29bfdeed997e7eef8f5574c1dd35c54db)，Go内容与验证时相同。

## 本轮提交分组

| 分组 | 已发布提交 | 范围与变化 |
| --- | --- | --- |
| ACP夹具 | `df625c3` | 3个ACP测试文件；active disconnect时peer保持输入至实收帧，EOF-first不补帧。生产协议与原时限不改 |
| host Bash窗口 | `ea224bd` | 10个config/tools/cli源与测试；tools.bash.timeout正duration，nil沿用30秒，无效提前拒，runtime/父ctx只收紧，模型schema不扩 |
| Linux outer | `b88de71` | main与5个subreaper源/测试；standalone显式opt-in、non-TTY，默认off、库不读开关，内层proof与Unknown不改 |
| R1b | `081289b` | 3个新增测试文件；public interactive两个进程60+60恢复，真实文件/一次副作用/多次压缩/逐动作证明与完整导出 |

共22个Go变更文件，无保护配置修改。上游已恢复受管Git，不移植旧C六只读生产补丁。

## 当前组合验证

- 完整make verify **exit0，472.44秒**。CI同版lint、GOFLAGS='-trimpath -p=2' GOMAXPROCS=2，原15分钟gate和规则不改。主race **9708 started / 9699 passed / 0 failed / 9 skipped，31包**；fmt/vet/lint（0 issues）/build/scan通过，log10/10、leak6/6通过。goimports缺工具按既有规则skip，未声称执行。1362份Go输入SHA与mode前后及独审时完全相同。
- current tagged gate **5 started / 5 passed / 0 failed / 0 skipped**：既有120-turn、PTY follow-up、PTY Escape取消，以及R1b（23.14秒）。这是Darwin真实race二进制、本地确定性peer，不是Linux opt-in或模型能力分。
- R1b原件独审：125实际请求逐条hash一致、120公开输入、2个不同PID各60次/同物理session/均exit0，117个不同durable compaction及boundary（A57/B60），5个不同动作各成功receipt/正向proof/typed resolution，恢复前后pending0。task-state/effects/final原件完整，随机canary从首个真实user输入独立取值核对，最终阶段2且副作用仅一次。mock没有真实usage，不能换算推理token/费用。
- host窗口独立Darwin CLI边界：默认30秒的exec sleep31超时unknown；host300秒同命令成功并继续回合；双方资源proof闭合。工具日志30006/31020ms，原wall含peer收尾，不作纯agent速度比较。
- Linux outer具有native ARM kernel/helper、四个有效实际CLI诊断臂及唯一取消补验的证据；后台资源闭合仍保留Unknown，前台与&wait成功。取消补验现场确认工具尚活后，native pidfd向outer发一次TERM，实际子进程S→由outer收养的Z→消失；CLI0.546秒exit1/context canceled/Unknown，唯一动作proof与typed resolution闭合，无shutdown pending。原两次测量无效且未发TERM的尝试保留；reader join仅由密封proof证明，没有直接导出的内部布尔。amd64 Rosetta显式opt-in因额外FD不支持而提前拒绝，不能称D amd64已过；Darwin full gate不能替代Linux平台证据。

全量日志SHA256：`75eefff2faffa595340c2114a479a7e9694fbf100780144115f4f1eb00eb3ac2`；tagged日志SHA256：`c3496e170f10e77acfe0aa1674ee8ef6a940960f0783f5f08017c648648b4821`。原日志与合成请求/会话留外部执行区。

## 脚手架与模型结果边界

已提交signal前process snapshot的source包导出52合同通过。新增有限Bash300 profile的24文件manifest `f93df191…`/61合同是封存历史，默认Go/Pi配置与argv对098c350精确兼容；新profile只改Go窗口并固定supervisor off。随后增加真实wire的安全tool描述/schema哈希、有限窗口claim与command-only三态观察，raw转发/预算/拒绝策略不改；缺失/nonfinite/非法UTF8未知为null，不记录描述/消息/命令原文。当前25文件manifest为`f14539b53aa0f4e28924fb28b84de95623b012f384eb4e7b5b6fde32f8414613`，导出同25份源码逐hash匹配，pinned Python3.12/Harbor0.25 **64合同PASS（14.444秒）**，Reviewer源审与hash核对通过。最终日志SHA为`c3c89054200d3a5f086bae5b3f29d3beb994078c3c129d76ed700430286df9ae`；早期14.948秒旧导出保留、不冒作修后证据。实际newprofile双CLI有效配置/wire仍待核，claim不是实际OS时限。

NavD仍是clean5aa063的一次开发回归：官方reward1、CLI exit1、自定义delivery_v1=0，37模型请求，原记录不改。delivery_v1是约定退出/协议指标，不是跨Harness完整OS资源闭合认证。旧B/Pi/D外部清理目标数0/22/2均无原/proc状态，不能当活进程/僵尸数或Pi资源更强。见[第2轮](../../../docs/evaluation/harness-certification-2026-10-28/round-02.md)、[第3轮机制证据](../../../docs/evaluation/harness-certification-2026-10-28/round-03.md)。

Flipt黑箱empty0/reference1及asset/profile独审通过；fresh初态15个配置候选不存在。这不能代替生成配置/有效assembly核验，也不自动放行模型。后续最终source/binary/runner/payload/profile冻结后只能按预注册工程pilot运行；R2/R3仍partial，不称R4正式认证或整体排名。

旧B官方0、历史Pi官方1、旧C仅无费回归继续保留。不同机制测试、开发题、平台不能合成对Pi的普遍胜负。[辅助策略](auxiliary-policy-audit.md)与[context审计](../context-budget-audit.md)的限制仍适用。协调者分组stage/commit/push；无需等待新模型成绩才发布已验证工程修复。
