# 当前发布审核

日期：2026-10-29。基础源码为 `5736e8828c80913fd4a32dc7b6e22eb82e5ec2b1`；核心三提交已推送至[42ce224](https://github.com/PengJunchen/go-cli/commit/42ce22477d900265b186677762ad4eff4aea0e26)，[Draft PR #10](https://github.com/PengJunchen/go-cli/pull/10)。旧379分支的结果与全量检查只作历史，不代表本次主分支状态。

## 核心修复范围

核心修复为三文件：`internal/llm/provider.go`、`internal/llm/provider_thinking_test.go`、`internal/cli/git_read_completion_test.go`。HTTP兼容编码器复用已有Thinking编码逻辑；八子例检查Generate/Stream的high、medium、none和unset，并保留工具/采样参数。通用CLI回归验证单status及并行工具后续回合和pending清空，显式隔离自动个人记忆。

完整检查另发现上游Git取消测试把启动挤进1秒门槛，故增加两份test-only变更：原测试文件移出该case，新Unix夹具分开验证“ready后取消”与“leader退出自动回收”。保留1500ms取消界限、具体错误类别及PID/group消失断言；不修改生产Git/process-scope代码。

上游main已提供更广的受管Git生命周期；**不迁入旧C生产实现、旧注册来源期望或旧QA fixture**。

## 验证状态

- 新main红测确认四个high/medium子例漏字段；none/unset四例通过。修复后Builder目标race：LLM1.584秒、CLI2.536秒通过；Reviewer已独立源审。
- 新main首次完整检查：9663 started / 9653 passed / 1 failed / 9 skipped，唯一失败为既有Git夹具1秒内未见子PID；隔离race五次通过。新A/B夹具目标race三次通过（9.321秒），Reviewer源审通过。
- 第二轮完整 `make verify`：**exit0，通过**。使用CI同版lint、`GOFLAGS='-trimpath -p=2' GOMAXPROCS=2`，保留原15分钟gate时限和规则。主race **9666 started / 9657 passed / 0 failed / 9 skipped，31包**；scan gate通过（允许的WARN保留），log10/10、leak6/6均无失败。fmt/vet/lint/build通过；goimports缺工具按Makefile显式跳过，未声称执行。普通真实二进制工具矩阵17.88秒、durable resume7.09秒通过。
- 新main真实Linux CLI无费fixture：**已通过并独立复核**。single-status两次请求、一个动作；write-parallel三次请求、五个动作。每个Go动作各有quiescent/正向proof/durable resolution，后续回合完成。新Pi有两次请求及真实read成功。双方均exactmodel/high/16384、四个采样字段实际省略，未调用真实模型。
- 单独tag120真实二进制测试：**2 started / 2 passed / 0 failed / 0 skipped**。120次真实本地HTTP、各轮当前prompt、最后25条messages、早期turn省略及至少一次compaction日志得到检查。它不是自主120步任务，不含独立进程重启，也不证明至少两次压缩或业务canary保留；R1b仅在独立工作树开发候选测试，尚无该真实二进制旅程结果。
- 评测源包导出合同先47项通过（14.341秒），控制profile对齐增补后 **49项通过（13.871秒）**，含15个汇总合同、新profile/采样、镜像pin和write-parallel协议检查。只用本地mock/官方Replay接口，不是模型能力成绩；官方Replay既有ResourceWarning保留未改。
- D干净提交5aa063的GitHub检查 **13项全部成功**。Nav开发回归已完成：官方reward1、CLI exit1、自定义完整交付0，见[第2轮报告](../../../docs/evaluation/harness-certification-2026-10-28/round-02.md)。更多题与全工具/长任务认证仍未完成；[辅助策略审计](auxiliary-policy-audit.md)明确当前small_model为空，不做LLM摘要。

## 提交边界

核心三文件及两份test-only夹具已由协调者提交推送。评测脚手架、依赖锁、元数据和 `docs/evaluation/` 指南另作提交；后续资产入口自动门不扩入本次核心修复。实际stage/commit/push由协调者执行，不推送旧分支历史。

旧B在Navidrome的官方分为0，Pi为1；旧C只有无费回归。这些记录继续独立保留，不能改写成新main成绩。参见 [迁移与profile说明](main-migration-review.md)；旧完整校验日志、工件与source hash留在外部执行区和本地checkpoint，不作为本次已通过的证明。
