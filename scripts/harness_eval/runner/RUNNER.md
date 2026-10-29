# 固定模型双 CLI 评测装置

本工程使用 SWE-Bench Pro V2 的官方任务资产与官方 clean patch replay 验收；因本机 Harbor 的 nftables 内核探针不支持，执行层使用薄 Docker driver，不能称为完整运行了 Harbor 原生 runner。

当前profile为 `main-5736e88-personal-memory-off-v1`，源码基础为 `5736e8828c80913fd4a32dc7b6e22eb82e5ec2b1` 加本次Thinking修复；实际版本另由payload manifest绑定。上游已经恢复受管Git等能力，不迁入旧379派生C的六只读实现。旧B/Pi首题分数与旧C无费结果保留原样，不作为新版本成绩。

新增可选profile `main-bash300-personal-memory-off-v2`。不传`--profile`仍使用原profile，其Go配置和两款CLI的参数保持兼容；新profile只在Go配置增加`tools.bash.timeout="300s"`，Pi配置、原生时限策略、模型参数和共同预算不变。两款CLI的同一批实验都应显式选择同一个profile ID。`configured_go_bash_execution_window_seconds`记录Go宿主声明的30/300秒，不能当成Pi窗口或实际观察值；`go_supervisor_mode`同属宿主声明，实际验证另见`go_supervisor_environment_validation`。

仅新profile向任务容器固定加入`GO_CLI_LINUX_SUBREAPER=0`，并在启动容器前用Docker inspect验证该key唯一且值为0；未知、重复或值1都停止。它不读取宿主的supervisor变量，也不提供自由duration/env覆盖入口。工具描述/schema哈希应从实际wire中的tools取得，本profile不会根据常量伪造观察；真实CLI双臂fixture与正式任务结果仍需另做验证。私有本机31秒命令对照不能代替这个标准profile的双CLI验证。

relay的`tool_metadata`逐项记录实际工具index/name、description UTF-8 SHA和parameters对象的canonical JSON SHA，不保存原文。只有实际bash描述中的固定Go子句会给出`go_host_window_claim=30s/5m0s`，其余或冲突为null；这只是广告声明，不是秒表或实际执行时限证明。`command_only_schema`按完整实际schema给true/false/unknown，Pi带timeout等已知不同形状为false，不替Pi补Go默认值。同名工具逐项保留，消息/命令/工具原文不写日志；转发原始请求字节、usage和预算逻辑保持原样。

固定候选模型 `codex/gpt-6.1-sol`，Chat Completions，effective `high`，主请求最大输出 16384。更小的原生内部请求上限允许原样通过。保留各 Harness 原生 system prompt、工具、子 Agent、重试和压缩；Go 不配独立 small model。所有模型调用经过同一审计 relay，其他模型与 effort 不符的请求在上游前拒绝。

Go自动个人记忆在产品默认开启；本profile显式 `memory.enabled=false` 关闭自动提取和注入，fresh容器不挂用户HOME，用于隔离跨任务状态。这是公开的实验选项，不是修改产品默认，也不关闭原生session或压缩。所有实际后台/子Agent请求仍须计入审计与共同预算，不假设它们不存在。

采样配置不显式设temperature/top_p/seed/parallel_tool_calls；relay在 `sampling_parameters` 记录实际存在的安全标量，原样转发body，不替模型补默认值。省略与服务内部有效默认不同，日志不能证明权重或隐藏路由未变化。

任务参数为官方 3000 秒、1 CPU、4 GiB；Docker overlayfs 未执行 storage_mb=10240 的逐容器硬盘硬配额，结果明确记录。共同安全预算为最多 80 次上游请求，涵盖原生重试、摘要和子 Agent；usage 缺失后阻断后续调用。没有整题 token/美元硬限额。遇到 cap、缺失 usage、参数契约错误分别标注预算停止、测量停止、接入问题，不能把这类结果当成纯解题失败排名。

## 无模型费用检查

```bash
.venv/bin/python -m unittest discover -s tests -v
.venv/bin/python run_trial.py --task third_party/swe-bench-pro/v2/tasks/完整任务ID \
  --arm go --out evidence/新的目录 --probe-only
.venv/bin/python run_trial.py --task third_party/swe-bench-pro/v2/tasks/完整任务ID \
  --arm go --out evidence/新的目录 --fixture-smoke
.venv/bin/python run_trial.py --task third_party/swe-bench-pro/v2/tasks/完整任务ID \
  --arm pi --out evidence/新的目录 --fixture-smoke

# 可选300秒Go窗口的独立profile；Pi对应尝试使用相同--profile ID。
.venv/bin/python run_trial.py --task third_party/swe-bench-pro/v2/tasks/完整任务ID \
  --arm go --out evidence/新的300秒profile目录 --fixture-smoke \
  --profile main-bash300-personal-memory-off-v2
```

fixture 使用本地固定协议服务，经过真实 CLI 和工具，不调用模型、不解真实题。Go 默认 `--fixture-scenario parallel` 请求 read/bash/git_status/read→final；`--fixture-scenario single-status` 请求 git_status→final。Pi 没有 git_status 时保留 read→final。成功门槛为同模型/high/输出限制、至少两次 accepted 请求及实际工具结果；Go 还须逐动作检查 quiescent、非空 completion proof 与 durable resolution，不能只匹配 final 标记。不能把 fixture token 数或结果当模型成绩。

网络由独立 internal network 与只接两张网络的固定目标 relay 构成。任务容器没有公网网络、宿主端口挂载或 Docker socket。门禁验证 relay 目录可达，GitHub/PyPI/npm、固定公网 IP、host.docker.internal 网关、本次 internal gateway 均不可直接访问；relay 不实现 CONNECT 或任意 URL 转发。目录 readiness 重试只是基础设施启动等待。

黑箱empty/reference校准使用 `scripts/asset_preflight.py control`。控制容器现在与clean grader对齐1 CPU/4 GiB、pids512、cap-drop ALL、no-new-privileges、/app、sleep entrypoint及bridge网络；验收同为TERM/15秒kill grace/3000秒，外层3045秒。reference与Agent隔离，只读取reward/exit/hash，完整输出留私有文件不解析。每次新校准另存当前源码/镜像/评分资产哈希侧车，旧control缺失字段不回填；source合同通过不等于真实oracle已得1。

## 真实尝试与评分

省略 `--fixture-smoke` 会调用真实模型并可能计费。项目协调者统一确认无费 gate 后才执行，单题每款 CLI 一次调用，无额外补提示、重启或代写代码。

```bash
.venv/bin/python run_trial.py --task third_party/swe-bench-pro/v2/tasks/完整任务ID \
  --arm go --out jobs/任务组/instance_go
.venv/bin/python grade_trial.py --task third_party/swe-bench-pro/v2/tasks/完整任务ID \
  --source jobs/任务组/instance_go --out jobs/任务组/grade_go
```

对 Pi 同理。`grade_trial.py` 导入固定源码的官方 `PatchReplayAgent`，只将该次完整 git patch 交给新建的相同镜像，再运行原始 `tests/test.sh`，复制独立 verifier 输出。它不会复用 Agent 改过的解释器、测试、环境或缓存。官方 verifier 网络模式为 public，按资产配置保留。

当前入口强制instruction/task.toml哈希与image digest/实际ID；replay/verifier哈希仍属于记录，尚未在每个入口自动验证官方checkout HEAD和全部tree。当前选中三题另做了只读Git metadata/hash人工预检，HEAD符合官方锁、目标tracked clean、无目标untracked、目标blob与固定commit一致，未输出任务/gold/验收内容。后续自动资产门另行实现；不能把仅记录哈希说成自动验证全部官方资产。统计时无费fixture与真实模型attempt分开，不把合成usage当真实成本。

每批运行前至少对当批任务执行下面的无内容输出检查，失败就停止。替换任务ID；它不替代完整blob/tree预检或后续自动门。

```bash
(
set -e
OFFICIAL_SWE_ROOT="$PWD/third_party/swe-bench-pro"
OFFICIAL_SWE_COMMIT=66f92766bba642462d4bbe5479e83f91f9211862
OFFICIAL_TASK='v2/tasks/<完整任务ID>'
test "$(git -C "$OFFICIAL_SWE_ROOT" rev-parse HEAD)" = "$OFFICIAL_SWE_COMMIT"
git -c core.fsmonitor=false -C "$OFFICIAL_SWE_ROOT" diff-index --quiet "$OFFICIAL_SWE_COMMIT" -- "$OFFICIAL_TASK" v2/tooling/patch_replay.py
test -z "$(git -C "$OFFICIAL_SWE_ROOT" ls-files --others -- "$OFFICIAL_TASK" v2/tooling/patch_replay.py)"
)
```

任务 prompt 按 UTF-8 文本原样传入。二进制和配置、任务、完整 patch、官方 replay 与测试脚本有哈希。CLI exit 0 不代表成功；任务完成只由 clean grader 的 reward 决定。超时与异常产物仍保留，开始但未完成的 provider 请求计作 usage 未知。

Go 启动日志路径在该基线中硬编码为 `/app/.go-cli/logs`，因此它可能进入完整 patch。本实验保留并披露，不在收集后删除或过滤。Go 模型单次调用超时显式设为 5 分钟、流空闲 2 分钟；原生 200 轮/压缩策略不改。此差异与 Pi 的原生行为一同公开。

## 旧基线预检证据（历史）

- `evidence/preflight/payload-manifest.json`：原始 Go 379134d 二进制、Node 官方 archive 校验、Pi 1.4.0 与依赖锁身份。
- `evidence/network-nav-probe-1`：relay 未就绪的基础设施失败，未发模型请求。
- `evidence/network-nav-probe-2`：基础网络门禁通过。
- `evidence/nav-go-fixture-1` 与 `nav-go-fixture-2`：基线 Go 的 high 配置未到达 wire，三次契约拒绝，零上游请求；这是接入发现，不是模型任务失败。
- `evidence/nav-pi-fixture-1`：Pi 两次 high/16384 请求与真实 read→final 通过。

Go基线问题由评测发现：CLI的openai路线构造Eino HTTPChatModel，而该编码器漏传Thinking。最新main仍有同一编码缺陷，因此移植通用一行修复与八case测试；不代表需要重做已由上游修复的Git生命周期。当前main的真实CLI无费证据、完整检查与任何后续收费运行分别新增记录，不覆盖这些历史目录。

## 纯离线结果汇总

`summarize_trials.py`只读取显式给定的`run_trial.py`结果和`grade_trial.py`独立评分JSON，不启动Docker、模型、下载或评分，也不读取原题、gold、隐藏测试、候选patch或raw轨迹。下例在导出的外部工作区针对已经存在的job运行：

```bash
.venv/bin/python summarize_trials.py \
  --trial jobs/任务组/instance_go/result.json jobs/任务组/grade_go/grade.json \
  --trial jobs/任务组/instance_pi/result.json jobs/任务组/grade_pi/grade.json \
  --output jobs/任务组/summary.json

# 尚未评分时用-，不会伪记0分。
.venv/bin/python summarize_trials.py --trial jobs/任务组/instance_go/result.json -

# 小合同测试，无模型和Docker依赖。
python3 -m unittest discover -s tests -p test_summarize.py -v
```

输出文件必须不存在，避免覆盖旧记录。相同result重复输入、grade的task/patch/image或`source_trial`路径不匹配会拒绝；因此应在原job布局汇总，搬迁证据后需先保留并核对路径来源，不能为通过检查擅改原始记录。输入JSON不会修改；输出只含允许字段和输入文件SHA，未知私有字段不会自动复制。

输出分三层：①`all_attempts`与逐次`official_score`原始账本保留所有尝试；②**主指标`end_to_end_delivery`包含Harness运行失败**，有效评分下的runtime、共同预算触顶/超时、API契约和measurement-stop都计交付失败，不从主分母删除；只有真正无效的infra、未评分/无效grade或offline fixture为unknown并单列。没有显式原因的非零CLI退出标`runtime_unclassified`，不猜成infra。缺usage但仍完成有效交付的记录另标`delivered_measurement_incomplete`，不能把测量缺失隐去。

③`solve_diagnostic`才是纯解题诊断子集，其`eligible_solve_diagnostic=false`绝不表示主评测没有失败，更不能用该子集给Harness排名。历史Go崩溃应同时保留raw官方0与主交付0；Pi为1。缓存/reasoning细分未知不阻断已知input/output或有效交付统计。

分组按版本、task、arm、profile及**任务/评分资产身份**：image、instruction/task.toml hash、官方replay/verifier hash与已报告的grader资源配置不同都隔离，不能把同taskID的不同资产版本混池；必要资产未知则逐trial隔离，不补推断。Go的source/binary不同不合并，Pi不因共同payload里捆绑的Go版本变更而自动变成另一个Pi。Go revision/source-diff/binary或Pi version/lock/Node任一必要身份未知，标`identity_complete=false`并逐trial隔离，不合并未知版本；不回填旧job里未报告的Pi锁。主交付按success/failure/unknown分别给耗时和usage；诊断子集另列success/failure/excluded。Agent和grader墙钟始终分开；成功交付摊销量包括同组全部失败/unknown attempt消耗，零成功或总量不完整时为null。

**总token只算input+output**。cached-input是input子集，reasoning通常是output子集，不再相加。只有每个请求input/output完整时才给精确`total_tokens`；否则保留`known_total_tokens_lower_bound`与`unknown_usage_attempts`。字段`known_sum`是部分已知观测，不能写成完整总量；缺失细分为null，不补0。美元只保留已有报告值，不查询价格或估算，当前job美元为null。工具不产生排名、显著性或自动配对结论，小样本只作诊断；不能只拿纯解题子集通过率忽略Harness故障。
