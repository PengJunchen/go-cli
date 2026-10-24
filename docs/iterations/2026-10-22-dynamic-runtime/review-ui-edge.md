# UI、轻量构建与 RPC 消费独立复审

日期：2026-10-22。工作区：`dynamic-capability-runtime/go-cli`。实现完成后才进入统一验证；保护文件未改。本文记录过程与最终结果，失败不计作通过；完整数值见[实测报告](../../research/iteration-2026-10-22/results.md)。

## 设计与实际消费

`headless,no_plugin` 使用同一 `cmd/cli`、Assembly、AgentLoop、Skill/MCP、权限、会话与 durable REPL；按 build tag 排除 rich TUI 与 native `.so` loader 的依赖。`runtime.profile: edge` 控制装配资源与渐进加载，编译标签控制链接体积，二者独立。Skill目录/模型投影先metadata；verified project为trust完整捕获有界正文，不能称全部项目正文启动零IO。用户完整正文才按需读缓存。

公共审批 DTO 移到 approval，TUI 保留旧类型 aliases；CLI 对 theme 使用接口，rich renderer/tea program binding 在 `!headless` 文件。headless event consumer 不累积 token/stream 输出，只留当前 durable snapshot 与 Todo，仍校验 canonical identity、复制 snapshot 后 ACK、等待 terminal publication 与幂等 Quit。交互沿用行输入、SIGINT 与同步审批；rich in-flight 键盘 steering/modal 不在 headless 合同内。

扩展 provider 使用显式 `llm.NewExtensionProvider` / `RegisterExtensionProvider` 的密封 RPC spec，模型声明白名单与 resolver-issued guarded candidate。首次 provider Build 在 dispatch authority 获准之后；拒绝 endpoint/credential/APIKey，不查询宿主 credential source。SDK 的 provider.generate 只传 public DTO，Stream v1 为完整结果的一次 final chunk。

## 独立静态复核

- Manager 用单 candidate gate、desired revision 与 epoch 拒绝过期 candidate；最多 current + candidate/retiring 两代。Acquire/Release 与发布在同一锁下，child invocation 独立留存 owner，最终 Release 才清理旧代。Close deadline 限制等待；未完成 builder/finalizer 仍被跟踪，不冒充已终止。
- Harness/TurnRunner 在 run-slot admission 后 acquire generation；prepared registry、hook plan 与 metadata 随 run context 固定。tool admission 与完成检查当前授权；epoch revoke 会取消 callback 并拒绝忽略取消后返回的结果。旧 generation 的正常在途 run 可以排空，不要求等同当前 generation ID。
- RPC 明确构造 env allowlist，不继承宿主环境；frame ≤1 MiB、inflight ≤64、有 request deadline、response ID 匹配、未知/重复 response 失败关闭。Unix 清理进程组，其余平台清理直接子进程。它不是文件/网络/CPU/RSS 沙箱。
- 可执行入口按同一复制流计算 SHA256，再执行 owner 临时目录内捕获副本；失败 setup 与 Wait 都清理目录。入口 hash 是完整性，不认证发布者；解释器、argv 参数脚本与动态库仍是额外依赖，复制动态链接入口也可能改变相对库定位。公开 sample 采用独立 Go worker。
- SDK API 仅依赖标准库，外部 worker sample 仅 import public API + stdlib；RPC Skill DTO 尚未协商，不把它写成已经可运行的 remote Skill 加载。

## 发现及处置

| 发现 | 处置与状态 |
|---|---|
| 引入中立审批 DTO 后旧 HITL 测试直接写私有 `program` 字段，default compile 失败 | 改为原公开 `SetProgram` seam，保留原全部行为断言；后续 default 已越过编译 |
| headless test tags 排除了被其他测试引用的通用 helper，且 slash registry 旧 expected 未覆盖新命令 | 仅隔离 rich 专属测试；通用 helper 保留在无 tag 测试文件。更新 expected capabilities/reload/skills；五包 headless race 已通过 |
| RPC 打开 FIFO/device artifact 可在候选初始化期间无限等待 | 已交 runtime 作者修为 regular-file 校验、Unix nonblocking/no-follow open、open 后 Stat、复制取消检查；回归已在 headless extension race 通过 |
| 已 dispatch RPC 返回合法 JSON 但错误 Result shape，被归为普通 known error | 已交 runtime 作者改 `RPCOutcomeUnknownError` 并关闭 worker；回归已在 headless extension race 通过 |
| public Serve 捕获 handler panic 后编码普通 callback_failed，丢失副作用 unknown 语义 | 作者已增加 callback_panicked/outcome_unknown 分类，host 保留 unknown；回归已在 headless extension race 通过 |
| 热改 prompt budget 后实际 prompt builder 仍用启动旧 limit；MCP active limit 未跟随新配置 | 作者已让 prompt 使用 pinned compiler/budget；MCP limit 改变明确 restart-required；headless CLI race 已通过 |
| 初始空 Skill/MCP 时未安装 discovery 控制工具/owner，新增首个能力后 reload 可能表面成功但 model 不可发现 | 作者已保留空 Skill/MCP discovery 控制器，首个能力可 reload；headless CLI race 已通过 |
| 真正 cmd 入口 SDK provider dispatch 成功后，旧 cost tracker 仅接受两种 HTTP origin，拒绝 extension origin | runtime 作者窄修仅接受 `OriginExtensionProcess + ProtocolExtensionRPC`；真实入口双标签 race、成本正反回归和两标签 lint 已通过，整仓最终 gate 由主代理复验 |

## 验证结果

首次验证暴露并修复 Role string cast、HITL 旧字段构造、slash expected、headless 外部 rich 测试选择和 Windows syscall.Stat_t 依赖。早期失败日志产生于task temp，未计为通过；临时数据最终清理见verification。富终端 case 原断言保存在 `!headless` 文件；bridge/subagent/SSE/transcript projection、ACP 与 compaction 的中立合同继续运行。

| 验证 | 实际结果 | 生成日志（task temp upstream，最终清理见verification） |
|---|---|---|
| headless 五包完整 race：cli/tui/llm/approval/extension，`-p 1 -count=1 -tags headless,no_plugin` | 全通过 | `headless-race-final.log` |
| 全仓 `-race -tags headless,no_plugin -run '^$' ./...` | 所有测试 API 编译通过 | `headless-all-compile-final-3.log` |
| headless Phase4 entry chain + ACP 选择实际 race | 两包通过 | `headless-e2e-contract-final.log` |
| headless + e2e 三标签编译 | 两个外部 tests 包通过 | `headless-e2e-tag-compile-final.log` |
| default 外部 tests 编译，保留 rich case | 两包通过 | `default-external-compile-final-2.log` |
| 最后 cold toggle/source identity 合同 headless race | 通过 | `headless-last-guards-final.log` |
| 专用 `golangci-lint run --build-tags=headless,no_plugin ./...` | exit 0 / 0 issues | `headless-lint-final-2.log` |
| edge 原始 entry-loader → trusted edge profile → Harness，default / headless 两种 race | 均通过；4 请求 read-edit-read；未选 MCP 0 请求；Skill 正文未入提示；1024-byte budget 在 dispatch 前拒绝 | `edge-entry-default-race.log` / `edge-entry-headless-race.log` |
| 公开 SDK 真正外部 module，GOWORK=off / CGO=0 | build 与 go list -deps exit 0；仅 public API + stdlib，0 internal 依赖 | `public-sdk-consumer/result.json` |
| 新 cmd/cli 测试两种标签 lint | default / headless,no_plugin 均 0 issues | `cmd-default-lint.log` / `cmd-headless-lint.log` |
| public SDK command/provider 真正 cmd entry + Snapshot permutation | runtime 作者 default/headless targeted race 与两标签 lint 全通过；整仓最终 gate 另见verification | runtime 最终验收记录 |

edge 入口 fixture 直接使用原 runWithTrustManager 与 CaptureProjectSources，只在 task temp 创建 project/trust store，不改用户 HOME/store，也不新增 profile 环境变量。每个请求实际 tools schema JSON 为17419字节；这是该固定工具集的测量，不是100/1000目录规模结论。真正外部 `example.org/consumer` 的 go.mod 经 tidy 对齐仓库 go1.24.2，69个依赖仅 `pkg/extensionapi/v1` 来自本仓库；它在独立模块下构建成功。

六个构建已经全部通过：darwin/arm64 full + headless、archive baseline、Linux amd64/arm64、Windows amd64。统一 Go 1.26.1、CGO=0、`-trimpath -buildvcs=false -ldflags='-s -w'`；生产 import graph 无 Bubble Tea/Glamour/Lip Gloss/Chroma/stdlib plugin。最终6产物已在全部生产修复与最终gate后复建；darwin CGO0 full为22,651,666B、headless为14,213,890B，其他目标及SHA见持久化机器记录。交叉编译只证明编译；Windows private ownership 验证目前 fail closed，不等同完整 Windows 运行支持。

构建/SHA、1233个Go源与mod/sum的hash及350次数值已持久化在[measurements.json](../../research/iteration-2026-10-22/measurements.json)。最后复核源manifest未漂移；主代理最终 `make verify` 与受控E2E均exit0，初轮失败及资源约束细节见verification。

## 测量边界

实际 CLI 的 baseline/full-UI/headless/PI 1.3.0/OpenCode 2.0.32 功能 pilot 已执行：ready 与 read-edit-read 全部 10/10 通过，所有进程组与采样 PID 均退出。pilot仅作校准，不产生headline；最终隔离协议已完成每arm/场景5 warmup+30 measured，共350/350成功，数值与完整限制见[results](../../research/iteration-2026-10-22/results.md)。首轮 pilot 的泛化提示词使 Go 既有 Top30 筛掉 read，已锁定共同真实任务描述，首轮失败纳入报告摘要并修复fixture的重试阶段推进。固定 loopback model、同一 read-edit-read fixture 检查请求与文件结果；不同默认工具集要记录 schema 字节，不能称完全相同工具负载。full/headless render benchmark 采用相同历史行数与流字节规模；历史正文分别为 `loaded canonical row N` / `history`，不作逐字节等价或精确UI速度倍率结论。parent wait4 RSS 与整个进程树占用不同，OpenCode standalone 可启动服务子进程；harness 已增加每 50 ms 的 PID/PPID tree RSS 采样，单独保留 parent 指标、sample 数和误差边界。采样可能错过短峰值，RSS 相加可能重复计算共享页。二进制、包安装 footprint 与 Node runtime 依赖分别报告。

最终交互图已在实际浏览器复核before/after、节点边界点击、播放到8/8与320px布局/表格换行；repo canonical与inline副本SHA256相同。所有本代理preview server与临时浏览器tab已关闭。
