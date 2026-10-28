# 首个真实 Go attempt 的 runtime 失败复核

**历史范围：379派生B的首题故障与旧C设计。最新main已由上游恢复相关生命周期，不移植本页旧C方案；当前状态见[迁移审核](main-migration-review.md)。**

日期：2026-10-28。只读分析；Reviewer 未调用真实模型，未读取 gold patch 或隐藏测试断言，也未修改产品实现。

`jobs/nav-pilot-20261028/instance_go` 的首个真实 attempt 用时 21.13 秒、3 次上游 HTTP 200，实际模型/high/输出契约与 usage 覆盖完整，最终 exit 1：`core: tool completion did not prove local resource quiescence`。这应保留为有效 Harness runtime 失败；不是网关拒绝、测量缺失或 subagent 调用失败。官方评分另由 fresh clean replay 决定。

## 定位

- 先前单独 bash、ls 均完成。第三个模型回合请求 bash/read/git_status/read；四个工具均产生 success receipt，随后全局完成证明失败。安全摘录见同 attempt 的 `harness-error-evidence.json`，不复制题目参数或文件内容。
- durable transcript 中 git_status 是唯一仍为 `cleanup_pending` 且没有 resolution 的动作；同批 bash/read/read 均有 quiescent 完成证明和 resolution。这一逐动作证据由资产核查者独立交叉确认。
- `internal/tools/builtins.go:registerDefaultTool` 只对已审查的读、写和 Bash 类型选择 completion 注册。`GitStatusTool` 落普通 `reg.Register`，因此没有 completion proof。
- `internal/tools/git.go:DefaultGitTool.Status` 的 `ensureRepo` 检查和 status 都调用旧版 `runReadOnly`，后者使用普通 `exec.CommandContext`/`CombinedOutput`。这尚不能证明可能运行的 Git filter helper、进程组和本地管道读取者都已经退出。已有 `runReadOnlyManaged` 为 diff 路径实现了管理进程组和 reader join，可作为修复构件。
- `internal/core/run_commit_consumer.go` 的批处理最终检查正确拒绝缺失/未知证明；不能因已有 success receipt 就放宽该规则。工具业务输出成功和资源完成证明是两个不同条件。

## 通用最小修复设计

对 exact `*GitStatusTool` 且底层为非空 exact `*DefaultGitTool` 增加私有、受审的 completion-only 注册。保留空/unknown effects，不能因为名字是 status 就授予纯 workspace-read 能力：仓库 clean filters 仍可能执行命令。普通 `Register`、替代 `GitTool` 和未审查 wrapper 不得继承证明。

Status 的所有 helper（包括 rev-parse 检查）必须经过受管生命周期。只有全部 helper 的 leader、所拥有的进程组及读取者完成后，工具层才能发出一次真实的聚合完成证明。不能逐 helper 重复覆盖单次 invocation 的证明，也不能仅凭 Execute 返回无条件发 inline 证明。取消、强制关闭管道、无法确认 containment 应保留 unknown/pending 语义。现有进程组约束不能证明故意 setsid 逃逸且关闭所有继承管道的任意后代；修复不得扩大这项安全声明。

## 可执行 TDD 验收

1. 临时普通 Git 仓库，仅请求一次 git_status，然后应进入下一模型回合；当前版本应红。这即可复现，不依赖题目或并行。
2. 无害并行 bash/read/git_status/read，全部动作各有正确 proof/resolution，最终回答完成；真实原失败的类型顺序用于回归，内容独立构造。
3. clean/dirty/not-repo、预取消、helper 非零退出，核对业务错误与资源完成状态分开处理。
4. 合成 Git/filter helper 启动持管道后代，及关闭管道仍存活的同组后代；取消后必须 join/清理，不能早发证明。forced-close/unconfirmed containment 保持 fail-closed。
5. 自定义 GitTool、普通重新注册和未审查 wrapper 不获证明；重用既有封装生命周期测试，不添加一条名称白名单绕过。

当前仅为设计门禁，产品修复与新二进制须由 Builder 按父上下文授权实施。修复后用新源码 patch SHA/binary SHA 标记 C；保留 B 的真实失败和费用，不把 C 回归结果覆写 B。后续两道冻结任务可用于未针对题目调优的验证。

另校正上下文预算说明：正常 ConfigLoader 默认 `compaction.max_tokens=128000`；Assemble 在没有配置时的 fallback 8000 不能当作本次实际值。本轮没有修改原生 compaction 行为。
