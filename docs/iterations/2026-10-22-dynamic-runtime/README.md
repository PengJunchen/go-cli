# Go CLI 动态能力迭代

本轮把 10/18 研究中选定的 Go 模块化单体方案接入真实 CLI。规格保持 review v0.1；8 张票据仅在本地保存，不代表已发布到 TFS 或已经过人类审核。验收状态以 [最终验证记录](verification.md) 为准。

## 本轮结论

| 目标 | 当前结果 | 证据边界 |
|---|---|---|
| 运行效率与体积 | 相同参数的 full UI 22.65 MB、headless 14.21 MB，后者小 37.3%；固定工具旅程 median 从基线 102.9 ms 到 headless 94.7 ms，full UI 102.6 ms 与基线接近 | 三种 Go arm 为默认兼容配置；edge 满载、冷 OS cache、物理板与 Rust 未测 |
| 扩展性与定制 | 标准 Skill 目录、按需正文、lazy MCP、空目录后续刷新、公开 SDK 和有界 RPC 均进入真实消费者；命令与 provider 有真实 CLI/外部模块验收 | MCP 当前按服务器激活、tools 协议 2025-06-18；任意代码热卸载、MCP prompts/resources、modern subscriptions 与 RPC token streaming 另列后续 |
| 可靠性 | 最终 make verify 8,956 pass、6 skip；受控包并行完整 E2E 4,871 pass、6 skip；正式测量 350/350 成功 | 默认并行 E2E 曾有既有 helper 的启动超时；原失败、基线对照和重跑条件均保留，不能称该敏感已修复 |

部署建议：需要完整流式终端、steering、主题和 modal 时使用完整构建；资源受限设备选择 `headless,no_plugin` 并按需启用 edge profile。数据计划热刷新、公开进程扩展和原生 typed 能力分别承担不同需求，避免为所有场景加载统一重型插件运行时。PI 与 OpenCode v2 的固定配置结果见完整报告，不能由单一 fixture 推导总体产品优劣。

## 先读什么

- [产品规格](../../product/dynamic-capability-runtime.md)：用户故事、预算、兼容与刷新合同。
- [决策图](wayfinder.md) 与 [深模块设计](codebase-design.md)：方案选择、接口与锁/owner 边界。
- [8 张执行票据](../../../.scratch/dynamic-runtime/issues/)：分项与可执行 AC。
- [上游证据](../../research/iteration-2026-10-22/upstream.md)：PI、OpenCode v2、DeepSeek Harness 与 MCP 的固定版本和采用判断。
- [使用说明](usage.md)：edge、标准 Skill、MCP 命令、headless 与外部 SDK。
- [测量协议](../../research/iteration-2026-10-22/benchmark.md) 与 [最终结果](../../research/iteration-2026-10-22/results.md)：真实 CLI 固定任务、构建参数与评价限制。
- [交互架构图源码](architecture-iteration.html)：切换迭代前后，逐步播放能力加载，点击节点查看边界。

## 结构怎样变化

| 方面 | 迭代前 | 本轮实现 |
|---|---|---|
| 装配 | Assembly 固定 prompt/hooks；Skill 正文和 MCP 连接倾向在启动准备 | 组合根编译不可变数据计划；候选校验后发布，下一次运行取得 lease |
| 一次运行 | 既有 run owner、prepared dispatch、审批与 durable session | 继续复用这些边界；准入时固定计划，完成与持久化再次复核当前权限，实际退出后释放 lease |
| Skill | 平铺 Markdown/YAML 与旧装配 | 增加标准 SKILL.md metadata 目录、搜索、按需正文；项目字节仍由 verified capture 固定 |
| MCP | eager 连接、deferred 工具、既有 source generation | lazy 只列服务器元数据；选中后连接和有界分页，schema 在下一模型请求披露；候选预算失败保旧 |
| 定制 | 内部接口、native 插件，部分贡献未进入真实消费者 | 公开 v1 SDK；工具、hooks、prompts、commands、完整响应 provider 有真实消费者；可选有界 JSONL worker |
| 界面/部署 | 富 TUI 与核心依赖交织 | 中立事件和审批 DTO；完整 TUI 与 headless 共用核心，`no_plugin` 去掉原生共享库加载 |

项目 Skill 仍由既有 verified source 流程先有界捕获可信字节；metadata-first 是目录与模型投影合同，不表示项目正文启动时零磁盘读取。用户目录正文才在首次 load 时读取并缓存。

计划身份分为 `configGeneration`、同代工具可见性的 `catalogRevision` 和实际加载正文的 `materializedArtifactDigest`。摘要用于复现与审计；调用权限仍取自现有 policy/trust/operation permit，不能用旧摘要恢复授权。

## 生效与选择

日常兼容配置维持原 eager 行为。希望渐进加载时可选择 `runtime.profile: edge`，或显式配置 progressive Skill / lazy MCP；希望减少链接体积时另选 `headless,no_plugin` 编译标签。完整界面仍适合需要流式绘制、steering、主题和 modal 的用户。

`/reload` 在 idle 边界切换数据计划；`/mcp reload <server>` 先准备完整候选、预算和 snapshot，再一次提交 registry/owner/metadata。代码、provider、命令、session 等进程级变化，以及 active MCP 上限变化要求重启。启用动态计划但仍选 eager MCP 时，其来源按 cold 合同处理，防止后台 watcher 绕过固定代际。

本轮实际 MCP 是既有 2025-06-18 tools 协议的兼容与渐进装配，未声称实现现代 MCP 的全部 prompts/resources/subscriptions。RPC v1 provider 返回完整响应；WASM 未作为默认依赖。物理边缘板、Rust 全负载比例与真实模型编程成功率没有由固定 loopback 任务证明。

## 复审与保留

[Runtime 复审](review-runtime.md)、[集成复审](review-integration.md) 与 [UI/边缘复审](review-ui-edge.md) 记录真实消费路径和失败修复。构建、下载、缓存、测试 workspace 与原始日志使用任务临时目录；最终保留源代码、设计、票据、测量结论和可复现脚本。配额任一已知窗口剩余低于 25% 时，停止并归档 worktree，清理本轮测试数据。
