# 本轮验收记录

日期：2026-10-22。状态：最终源码完整 verify、受控包并行 E2E、追加真实入口验收和六项构建已通过；正式性能与 UI 诊断完成，临时数据已清理，源码 worktree 保留供审阅。

## 环境与来源

- 基线：`38239888f6275f9a329825a9bde791b449280227`（原 `dev` / `origin/dev`）。原 checkout 保留；实现位于独立 managed worktree，分支 `codex/dynamic-capability-runtime`。
- 本机：macOS、Apple M1 arm64、16 GiB RAM；执行工具链 Go 1.26.1，模块要求 Go 1.24.2。交叉构建与目标平台实际运行分别记录。
- 遵循本轮用户指令：先完成生产功能，再统一执行测试；失败修复后重验。未修改 Makefile、lint 配置、CI、infra 规则/任务，也未减少原有断言来取得通过。
- 设计、规格、8 张本地票据与三角色复审已保存；规格仍为 review v0.1，本地票据 unpublished-to-tfs，不宣称已有人类审核或已发布到远端系统。

## 实际合同

运行固定 `configGeneration`、来源与权限身份；`catalogRevision` 表示同代下一模型请求可见的工具集合；`materializedArtifactDigest` 记录实际加载的 Skill 指令正文。三者不互相替代。未选 MCP 不连接；发现后普通工具调用仍经过既有 prepared dispatch、审批和 trust fence。

CLI 数据刷新在 idle 边界生效，覆盖 Skill、prompt 和现有 shell hook。provider、扩展代码/命令、session 等进程级配置与活跃 MCP 上限变化返回 restart-required。不能把通用 Manager 的双代 drain 合同解读为 CLI 已实现任意代码热卸载。

## 失败、诊断与修复

- 初轮编译检查捕获新增 provider 的 Role 类型转换及旧 HITL 测试构造字段问题，已通过原消费 seam 修正。
- 初轮 lint 捕获错误处理、取消 owner、断言与拼写问题；另将 lint cache 隔离到任务临时目录，避免读取其他已删除 checkout 的旧缓存。未改 lint 规则。
- 首次完整 race 执行 8,912 个测试函数，8,905 通过、1 失败、6 跳过。失败是新 RPC 环境隔离测试 2 秒握手预算超时；安静环境原配置连续三次通过。仅提高常规 helper 的启动预算，超时专测仍保留 100 ms 和所有退出/unknown/环境隔离断言。后续最终结果以完整复验为准。
- 第二次完整 race 执行 8,944 个测试函数，8,938 通过、6 跳过、零失败。其后 AST 扫描发现 5 处测试假 key 重复定义与 1 处 MCP 字符串分支；前者复用既有统一测试 Provider，后者改为路由/帮助/补全共用的子命令注册表。未新增扫描豁免，也未更改扫描规则。修复后的 CLI 合同与 SlashMCP 定向 race 分别通过（2.821 s / 2.000 s），仍执行最终完整 gate。
- 既有 grep escaped-descendant 测试曾在并行重负载下触发 1 秒 helper PID 等待。安静串行运行当前与基线均通过；未跳过测试、扩大其超时或改变实现。性能测量不与 race/交叉编译并行。
- 交叉复审额外发现 MCP stdio setup context 生命周期、首次空目录发现接线、prompt 预算实际消费、跨 trust grant 重载、RPC 捕获入口/未知结果及 pre-Init DAG 失败清理问题；逐项由原作者修复，最终日志决定验收结果。

- 最后补充真实 SDK command/provider 入口验收时，发现成本记录器只允许 HTTP 来源，拒绝有效的 extension-process + extension-rpc。最小修复只接受这一精确组合；未知 origin 与错误 protocol 的负向回归保留。两构建的真实入口与排列测试通过后，重新执行最终源码完整 gate。

- 最终源码完整 `make verify` 已 exit 0（8,962/8,956/6/0）；随后 E2E final3 的既有 `TestDefaultGitDiffCancellationJoinsDescendantHoldingPipes` 在第 46 行等待 fake git 子进程启动标记的 1 秒预算失败，未进入取消/管道关闭断言。4,877 项中 1 失败、6 skip。保留失败事实，先以原参数对当前与基线重复验证，再限制包级资源竞争完整复验；不修改源码、断言或 timeout。

安静串行对照采用原 `-race -tags=e2e -count=5`：当前 5/5（3.617 s package / 4.63 s whole）、基线 5/5（3.930 s / 10.72 s，含基线编译），两命令 exit 0。Git fixture、git.go、managed process/Darwin identity 文件 SHA-256 与基线相同；失败在 cancel 尚未发出前。结果支持启动调度敏感的解释，不能证明唯一负载原因，也不能宣称基线在相同并行负载必然失败。随后完整 E2E 使用原 Make target 与 `GOFLAGS=-p=1` 控制包并行，整命令 exit 0（4,877 启动、4,871 通过、6 skip、0 失败）；race、测试集合、断言、timeout 和 gate 均未改变。此成功不证明默认并行下的启动敏感已修复。

## 最终执行结果

| 验证 | 当前证据 | 状态 |
|---|---|---|
| 完整 `make verify` | 最终源码（含 RPC origin 修复与 SDK 真实入口/排列验收）final3 整命令 exit 0；8,962 个测试函数，8,956 通过、6 条件跳过、0 失败，31 包；log 10/10、leak 6/6 | 通过 |
| AST 扫描 | 最终 final3：466 个生产文件；ERROR 0、WARN 1,866，保留原规则与阈值 | 通过（含告警） |
| `make test-e2e` | 最终源码 `GOFLAGS=-p=1 make test-e2e` final4 整命令 exit 0：4,877 启动、4,871 通过、6 条件 skip、0 失败，8 包。默认并行 final3 有一次既有 Git helper 1 s 启动失败，未抹去；诊断见上文 | 通过（受控包并行） |
| headless/no_plugin 五包实际 race | CLI、TUI 中立层、LLM、approval、extension 全部通过 | 通过 |
| headless 全仓及 headless+e2e 编译 | `./...` 的 headless 测试，以及 `./tests/...` 的 headless+e2e 测试可编译；这是编译证据，不冒充全仓测试执行 | 通过 |
| headless Phase4/ACP 实际 race | 两包 1.428 s / 1.918 s；默认 rich 断言保留 | 通过 |
| headless tag lint | 全仓 0 issues；新增入口测试另跑 default/headless 目标 lint，均 0 issues | 通过 |
| edge 原 CLI 入口旅程 | `runWithTrustManager` + 临时项目/信任库，经原 loader 和 Harness 完成 read→edit→read→final；默认/headless race 2.249 s / 1.956 s | 通过 |
| edge 渐进/预算 | 4 次模型请求各 17,419 B schema；未选 MCP 请求 0；Skill 正文未预注入；1024 B 预算在新 dispatch 前拒绝 | 通过 |
| SDK 真外部模块 | 临时 `example.org/consumer`，`GOWORK=off CGO_ENABLED=0 go build` 与 deps；69 个依赖，来自本仓库的仅公开 SDK，无 `internal` | 通过 |
| SDK 真实 CLI 入口与排列不变 | 默认 race：capability 1.250 s、production 1.608 s、cmd/cli 2.661 s；headless/no_plugin：1.474 s / 1.547 s / 2.573 s；两 tag 目标 lint 均 0 issues。公开 SDK worker 临时外部模块经信任 loader 运行命令和 provider，握手、参数隔离、退出与捕获文件清理均断言 | 通过 |
| 最终平台矩阵 | Go 1.26.1、CGO=0、trimpath/buildvcs=false/strip：darwin arm64 baseline 22,222,530 B、full UI 22,651,666 B、headless 14,213,890 B；headless Linux amd64 15,151,266 B、arm64 13,828,258 B、Windows amd64 15,501,824 B，六项构建均 exit 0 | 通过（交叉编译非实机运行） |
| 正式性能 | 新隔离 pilot 10/10；五 arm × 两场景，各 5 warmup + 30 measured，350/350 成功、无缺失或重复；root 独立核验全部 10 cohort 和 1,233 项 Go/module 源码 hash。详见 [结果](../../research/iteration-2026-10-22/results.md) / [机器记录](../../research/iteration-2026-10-22/measurements.json) | 通过（指定 fixture） |
| UI/内存诊断 | full renderer 与 headless consumer 各 1k/5k/10k 历史行、1,000×1KiB stream，benchmark 均 exit 0，B/op 与 allocs/op 已保存。实现功能及历史正文不同，不计算精确等价 UI 倍率 | 通过（诊断，不等于人工可用性评测） |

原始日志产生于本轮任务临时目录，完成后按用户要求清理；本报告、性能摘要、版本/构建参数和可复现脚本保留验收结论。不以早期部分通过或拼接分项结果冒充最终整命令退出成功。

完整 race 的六个 skip 均来自已有条件：3 个需要可选 `fd` 可执行文件，2 个针对本机不适用的 ownership/containment 平台分支，1 个 Linux 内存限制分支。未新增 skip。扫描 WARN 包含 `SCAN-006` 对接口/泛型及跨包调用的启发式误判（例如 CLI 真实调用的 `NewManager` 仍被报仅测试使用）与纯编译模块无 slog 的提示；最终 final3 比 final2 多 4 条 SCAN-006，均为新增排列测试首次引用的只读 Snapshot getter（PromptDigest、CapabilityDigest、PolicyEpoch、TrustEpoch）；未为消告警添加虚假生产调用。未把 ERROR 改成 WARN，也未声明所有告警都已消除。

E2E 的六个 skip 为 2 个可选 fd 测试、1 个 Linux 内存限制分支，以及 3 个原已停用的 CodeHighlighter 测试（已被 Glamour MarkdownRenderer 替代）。对应原始 skip 与原因保留在最终摘要中，不计作 PASS。

## 评价边界

真实 CLI 固定本地模型任务用于测框架开销、工具旅程和文件结果；不能据此评价真实模型编程成功率、SWE-bench 或宣称达到 Rust 的所有负载性能。PI / OpenCode 的版本、发行包完整性、默认工具集差异、安装 footprint 与进程树 RSS 口径另见性能报告。缺失结果保留，不填虚构排名。

本轮没有物理边缘板实测；Windows 的交叉编译结果不能替代运行验证，其项目来源 ownership 检查在不支持平台明确 fail closed。RPC v1 provider 为完整响应，非 token streaming；进程隔离不提供 OS 文件/网络/CPU/RSS sandbox。Honeycomb 官方资料归属尚未证实，采用的机制以可验证的 DeepSeek Harness、PI、OpenCode 与 MCP 源码为依据。

## 配额与保留策略

任一已知配额窗口剩余低于 25% 时，停止开发与验证，保存 WIP/证据/未完成项，归档 managed worktree，并清理任务拥有的测试、下载和构建数据；不消耗重置券继续。成功验收时保留源码 worktree 供 review，只保留结论、协议、测量摘要和可复现脚本，删除本轮临时二进制/缓存/测试数据。

## 最终交付与清理

2026-10-22 已实际删除本轮唯一任务临时目录 `/tmp/go-cli-iteration-20261022.to3oRg`，包含二进制、Go/lint 缓存、上游下载/安装、测试 workspace/trust store、原始日志与预览 wrapper；删除后确认路径不存在。预览进程已停止，清理前未发现仍引用本轮临时路径的进程。源码仓库无生成 binary/coverage/test 产物。

保留本轮源码、8 张 verified-local 本地票据、review v0.1 规格、设计/复审、可复现脚本、数值/provenance 与交互图。图的仓库版与内联版一致，SHA-256 为 `de487a0de8e23df3189a1dd396846494d87da44234b8916b23437bc002d96c4f`，已验证前后切换、节点选择、播放结束和 320 px 布局。

原 `dev` checkout 保持干净；保护文件与依赖文件未改。独立分支 `codex/dynamic-capability-runtime` 的源码 worktree 保留供 review；本轮不推送远端。最终可见 quota 窗口剩余 66%，低于 25% 的暂停条件未触发，未使用重置券；其他未返回的窗口不推断为零或无限。
