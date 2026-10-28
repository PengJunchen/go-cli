# 最新主分支迁移与当前评测配置

日期：2026-10-29。此文件说明当前发布范围；旧基线的实测、修复与失败日志另存，不把它们当作新主分支成绩。

## 为什么更换发布基线

先前执行基于 `379134d679dcae5c996b730eeae464fdb61fe853`。核对远端后发现，最新 `origin/main` 为 **`5736e8828c80913fd4a32dc7b6e22eb82e5ec2b1`**，已经合入更完整的能力恢复。旧分支与它有大幅历史和源码差异，不能整体推送或合并。旧成果保存在本地 checkpoint `65cf537`；本次从最新主分支建立发布分支，只迁入必要修复、通用回归、评测源包和文档，不带旧历史、旧C生产实现或已从main删除的QA fixture。

| 项目 | 旧基线记录 | 当前新主分支 |
|---|---|---|
| Thinking编码 | 原始A漏传；B通用修复后才能有效high | 同一HTTP兼容编码缺陷仍在；迁入一行已有编码器调用及八子例测试 |
| Git完成证明 | B在git_status后退出；旧C只修六个只读工具 | 上游已有exact实现的受审注册、受管命令和unknown/pending处理，覆盖更广Git家族；保留上游实现 |
| 回归测试 | 旧C曾断言owned-process证明与builtin来源 | 只保留单status/并行多轮的通用CLI回归，检查后续回合和pending清空，不绑旧证明类型或来源 |
| 个人记忆 | 旧profile/旧状态按原工件解释 | 产品默认自动记忆开启；本次受控profile显式关闭自动提取/注入，避免跨任务状态与后台请求影响 |
| 官方任务成绩 | B/Nav=0，Pi/Nav=1；旧C官方回归not_run | **新main+Thinking尚无官方任务成绩**，不得继承、覆盖或与旧分数混算 |

当前相关源码：[HTTP编码](../../../internal/llm/provider.go)、[受审注册](../../../internal/tools/reviewed_runtime_tools.go)、[受管命令](../../../internal/tools/owned_command.go)、[完成状态](../../../internal/tools/prepared_completion.go)。上游工具来源仍可为普通registry，但拥有单独受审的完成能力；不能把来源字符串等同于所有安全权限。Git effect维持unknown，未知/取消/未收尾资源不能因函数返回就获得完成证明。

## 当前受控profile

`PROFILE_ID=main-5736e88-personal-memory-off-v1`，基础源码revision为5736e88，实际执行身份仍以该次payload完整revision、dirty patch SHA和binary SHA为准。源码提交后必须重建并重新记录；不得把提交前二进制身份覆写成提交后版本。

- 双方使用同一 `codex/gpt-6.1-sol`、high，主输出cap16384；小于该cap的原生内部调用可原样通过。
- Go设 `memory.enabled=false`。这是**不同于产品默认true的实验选项**：关闭自动提取与记忆注入，显式/memory功能仍属产品能力。fresh容器不挂载用户HOME；原生session、工具、压缩与其阈值不因本次隔离而替换。
- 不配置独立small model；未据此假设所有后台/子Agent请求都不存在。所有实际请求、原生重试、压缩及子Agent共用80次上游请求上限；未知usage后停止后续请求。没有金额/token硬上限。
- provider/model配置不显式设置temperature/top_p/seed/parallel_tool_calls。源码presence链支持省略，不代表已证明服务端内部默认。新审计日志逐请求记录这四个安全标量的实际presence/value，保持原始body原样转发；缺失键用空映射区分，不伪造为0或某温度。
- 共同模型目录声明500000上下文；Go目前没有读取这项容量元数据，其原生CLI压缩预算128000/micro_first、约0.8触发；Pi内部配置500000并保留16384输出余量。**同一模型API不等于内部上下文预算相同**。D保留这些原生差异，未来共同500k另开profile，不能将旧B归因为8k耗尽；详见[上下文审计](../context-budget-audit.md)。
- 任务继续使用固定官方资产、digest镜像、1 CPU/4 GiB/3000秒、隔离模型relay与fresh官方patch replay；10GiB存储只是声明，未执行overlay硬配额。Linux amd64仿真与其他原生环境不能直接混合比较延迟。

此profile回答“单任务、无跨任务个人记忆”的效果；若以后要测个人记忆，应另开事先定义的多任务状态实验，不能把两者混成一个分数。任何真实模型批次仍由协调者统一调度。

## 当前验证证据

- 新main上的Thinking测试先红：Generate/Stream的high、medium四子例漏字段；none/unset四子例通过。最小实现后，Builder目标race：LLM1.584秒、CLI2.536秒通过。Reviewer独立源审确认只迁入三份Go实现/测试文件，旧C工具生产实现、旧来源期望和QA fixture未迁入。
- 新profile和采样日志合同先红后绿；最终源包导出到新临时工作区，Reviewer完整 **47项Python合同通过（14.341秒）**，包含15项汇总合同，无模型费用。官方Replay源码的既有ResourceWarning保留，不修改第三方代码。
- 新main Linux真实CLI的single-status/write-parallel无费fixture：**已独立核验**。一个/五个Go动作各有quiescent、正向proof和durable resolution，后续回合完成，实际high/16k分别两次/三次请求。新Pi无费read回合也通过；两边四个采样标量均实际省略，不能据此证明服务端温度值。
- 新main完整 `make verify`：首轮仅既有Git启动夹具失败；test-only握手修订后第二轮exit0，主race9666/9657通过/0失败/9跳过，scan/log/leak通过，详见[当前审核](release-review.md)。保持 `GOFLAGS='-trimpath -p=2' GOMAXPROCS=2`、CI同版lint、原15分钟gate时限。另有tag120的2/2通过，但不等于两次压缩/重启与业务记忆验收已完成。旧基线检查不当新main结果。
- 新main官方模型任务与全工具/长程认证：**not_run / 未完成**。优先复用上游无费Git写链、Linux Bash/取消合同，再按分轮计划安排新模型pair。

## 历史证据与发布边界

[当前发布审核](release-review.md)给出本轮门禁状态；[旧runtime根因](runtime-quiescence-review.md)、[原Thinking发现](thinking-fix-review.md)、[旧采样源审](sampling-audit.md)保留当时的范围，不是对5736e88的缺陷或全量检查结论。外部执行区的旧profile、driver和payload manifest已按SHA留快照；历史jobs不回写，不复制到公开源包。

公开包仅含自有代码、合同、依赖锁、任务选择/镜像元数据与安全报告，不包含原题、gold、隐藏验收、轨迹、执行二进制、私钥或旧Git历史。正式版本和新的官方成绩需独立记录，不能将本机无费fixture或已有上游单元测试当成新的编码能力分数。
