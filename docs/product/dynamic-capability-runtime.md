---
title: Go CLI 动态能力运行时
description: 定义标准 Skill、MCP 渐进载入、运行代际、扩展定制与边缘设备的本轮交付合同。
doc_type: product
status: review
owner: go-cli-maintainer
reviewers: [go-cli-maintainer, runtime-reviewer]
version: v0.1
created: 2026-10-22
updated: 2026-10-22
visibility: internal
ai_context: on-demand
tags: [runtime, skills, mcp, extensibility]
related:
  - docs/research/architecture-2026-10-18/architecture.md
  - docs/iterations/2026-10-22-dynamic-runtime/wayfinder.md
---

# Go CLI 动态能力运行时

## Problem Statement

开发者希望 CLI 启动快、占用小，装有更多 Skill/MCP 时仍能在低配机器上交互。他们需要兼容标准 SKILL.md 包、按任务选择能力、在不重启长会话的情况下刷新技能与工具，并能解释某项能力为何尚未加载、被禁用或需重启。现有 deferred 工具推迟的是适配器物化：Skill 正文仍在启动时读取，MCP 服务器仍在启动时全部连接；注册成功的 extension command/provider 尚未全部进入真实消费链。

本轮沿用 [10 月 18 日研究](../research/architecture-2026-10-18/README.md) 中的 typed Go 模块化单体、owner、generation lease 与 prepared dispatch 合同。新增研究补充 PI Coding Agent 与 OpenCode v2 的一手资料；Honeycomb归属未获权威证实，不作为已确认的DeepSeek实现。不把上游实现未经验证地当成本项目安全或性能保证。

## Solution

用户通过能力目录找到 Skill/MCP，按需加载正文或连接所选服务器；系统只向模型投影当前允许且已选择的能力。刷新先构建完整候选，成功后供下一次运行使用；在途运行继续使用其固定版本，资源在最后一次使用后释放。失败保留旧版本并提供结构化诊断。用户通过 CLI 命令查看状态、搜索、加载、刷新和资源档位，通过公开 Go SDK 定制能力；需要新代码的外部扩展可选择有明确版本与预算的子进程协议。

## User Stories

1. 作为终端用户，我希望看到已发现、已加载、失败和需重启的能力，以便知道当前会话能做什么。
2. 作为大型 Skill 仓库使用者，我希望启动只索引 SKILL.md 的元数据，以便正文和附属资料不会撑大默认 prompt。
3. 作为标准 Skill 作者，我希望目录中的 references、scripts、assets 不被识别为独立 Skill，以便直接复用技能包。
4. 作为旧版技能作者，我希望平铺 Markdown/YAML 技能继续可用，以便渐进迁移。
5. 作为用户，我希望搜索名称、描述与触发提示并显式载入 Skill，以便模型获得正确指令。
6. 作为安全敏感用户，我希望项目 Skill 只读取已验证捕获内容，撤销信任后立即拒绝执行，以便刷新不绕过现有信任机制。
7. 作为有多个 MCP 服务器的用户，我希望初始只看到服务器目录，选中后才启动进程或连接网络，以便减少启动成本。
8. 作为用户，我希望按服务器发现和选择工具，而非一次塞入所有 schema，以便节省上下文。
9. 作为 MCP 用户，我希望连接失败、权限失效与工具重名可定位，重试不破坏已可用工具，以便持续工作。
10. 作为长会话用户，我希望更新 Skill/MCP 后下一轮采用新代，而当前轮保持一致，以便不中断任务。
11. 作为用户，我希望刷新失败保留旧代且显示失败原因，以便恢复不依赖猜测。
12. 作为取消任务的用户，我希望后台资源在调用真正退出后才关闭，以便取消等待不损坏仍在运行的工具。
13. 作为用户，我希望撤权后拒绝新的敏感调用和迟到结果，以便历史版本不恢复权限。
14. 作为扩展作者，我希望公开 SDK 无需引用 internal 包，以便独立构建自己的能力。
15. 作为扩展作者，我希望 command/provider/hook/prompt 注册可被真实 CLI 消费，以便定制具有实际效果。
16. 作为用户，我希望重复名称、依赖循环、缺失依赖与版本不兼容在初始化和注册生效前被拒绝，以便配置错误不会部分生效；为读取manifest已启动的握手资源也必须关闭。
17. 作为第三方扩展用户，我希望子进程崩溃、超大帧、请求超时与退出可控，以便隔离扩展故障。
18. 作为边缘设备用户，我希望选择资源档位并设置索引、并发、连接、prompt 与帧字节预算，以便负载有界。
19. 作为维护者，我希望相同输入产生相同 generation digest、顺序和投影，以便复现问题。
20. 作为维护者，我希望日志保留 run/generation/owner 与固定错误类别而不记录秘密正文，以便审计生命周期。
21. 作为用户，我希望能力状态、加载、刷新与失败在 slash 交互中可见，以便无需理解内部实现。
22. 作为维护者，我希望最终统一验证真实入口、多轮、并发、关闭与性能，以便性能结论来自可复现数据。

## Implementation Decisions

- **Deep Module / Interface / Seam**：组合根保留在 CLI；以窄的运行代际 provider、能力目录与公开扩展契约作为 Seam。复杂的候选构建、排序、资源 owner 和失败回退留在 Module 内部，不新增平行 Agent/tool/session 系统。
- **确定性计划**：稳定 ID、requires/provides、scope、priority、版本与权限声明编译为不可变计划。摘要覆盖已捕获内容与元数据、依赖、来源和相关配置；未物化技能正文以加载结果摘要另记，mtime 仅作发现优化。外部manifest可在Load/握手后取得；循环、冲突、缺失依赖及不兼容版本在Init/注册生效前拒绝，已拥有的握手资源在失败时清理。
- **代际与 owner**：候选单飞，revision 防陈旧发布；最多 current+candidate 或 current+retiring 两代占资源，retiring 未排空合并待处理请求。失败清理候选保留 current；逆序清理、panic 隔离、重入关闭与超时均可诊断。不可双开的独占资源返回 restart-required。
- **run pin**：准入之后、首次模型或工具副作用之前固定来源配置与资源代际，覆盖 hooks、prompt 计划、Skill 目录与 MCP server 配置。工具可见性使用独立 catalogRevision；mcp_discover 只追加本代允许的服务器，下一模型请求取得新schema，同一response不得调用本request未声明的新工具。lease 延续至执行、并行调用、producer 和持久提交真正退出；取消等待不提前释放。策略/信任 revision 独立于代际，在调用与完成处复核。合法旧代在 drain 中仍可完成；不得要求等于全局 current。
- **标准 Skill 渐进载入**：支持目录 SKILL.md 和既有平铺格式；标准包附属文件不进入能力索引。元数据索引与正文载入分离，稳定排序与名称冲突诊断；目录只向 prompt 提供有界摘要，所选 Skill 正文按需进入模型上下文。附属脚本不自动执行，tools/allowed-tools 提示不当作权限提升。
- **来源**：用户目录可有界读取；项目内容沿用已验证捕获字节，不能因刷新重新打开未经批准的路径。大小、文件数、目录深度、单条描述、索引总量与 prompt 字节均有预算。
- **MCP 渐进载入**：保留现有 eager 兼容路径；按配置启用 lazy 模式，由发现/加载操作选择服务器后连接与 ListTools。工具 schema 的投影有界；调用仍进入已有 prepared dispatch 与 MCP generation leases，不能通过万能任意调用工具绕过审批。刷新失败保留可用 generation；加载/刷新错误可重试且不重复并发连接。
- **真实消费**：扩展工具、commands、providers、hooks 与 prompt 有真实默认 CLI 消费点；内建命令优先且冲突明确失败；provider 注册发生在模型选择之前。公开 SDK 不暴露 internal 类型；typed native 是默认低开销路径。
- **子进程**：版本化握手、请求 ID、取消、frame/queue/inflight/process/deadline 预算、受控 cwd/env 与有界退出。故障隔离不声称 OS 文件/网络/CPU 沙箱。Go .so 不承诺热卸载。
- **资源档位**：默认兼容档位和 opt-in edge 档位以配置表达；不在启动时连接未选 MCP，不创建无用途 watcher。资源预算失败返回固定类别；性能使用真实 Assembly 与固定无网络工作量对照。WASM 本轮完成 adoption 判断，默认不新增运行时依赖。

## Testing Decisions

最高 Seam 是配置 → 真实 CLI/Assembly → 模型可见目录/工具调用 → 完成/关闭。复用现有 mock provider、MCP server、prepared tool 与 session 测试合同，新增内存 Adapter 只用于需要确定交错的状态机验证。

本轮用户明确要求所有功能完成后统一运行验证，覆盖旧 TDD 的执行顺序；实现期间可以写验收测试但不提前执行。最终包含：标准 Skill/legacy 和安全路径；懒加载前零连接/所选服务器单飞；候选失败回退；old run drain、撤权、迟到结果；真实 command/provider 消费；子进程 framing/cancel/exit；多轮与 race/leak；Linux/Windows/arm64 构建可行性；冷/暖启动与内存、prompt/schema 字节和二进制体积。

目标命令见各 [本地执行票据](../../.scratch/dynamic-runtime/issues/)；最终运行 `make verify`，以及有适用 build tag 的集成/E2E 测试。所有测试日志/下载/临时数据进入本轮临时目录；任务结束由主代理保留验收摘要与可复现结果并清理原始临时日志、下载和测试资源，临时路径不承诺长期可读。未运行、跳过或宿主不能验证的平台分别记录，不以文档 AC 代替执行证据。

当前验收阶段：真实CLI SDK command/provider补证发现并修复extension-process/extension-rpc成本归属校验，两种tag的定向race与lint通过后，最后生产源完整make verify final3已exit 0（8962 started/8956 passed/6 skipped/0 failed）。默认并行E2E发生一次既有GitDiff fixture启动标记超时；原fixture/生产文件未改，current与baseline安静复验各5/5通过；随后原完整target以 `GOFLAGS=-p=1 make test-e2e` 运行final4 exit 0（4877/4871/6/0）。负载敏感限制与失败记录保留，详见review-integration。六build全部exit 0，正式五arm/两场景各5 warmup+30 measured共350/350成功，数据与参数见 [实测报告](../research/iteration-2026-10-22/results.md)；最终清理/交付尚待完成。

这些数据不包含冷OS cache、实物edge、edge profile性能、Rust、千能力规模或真实模型质量/网络测量。Go性能arm为default compatibility profile；headless是build tags。OS cache未flush，UI两实现功能不同，不计算精确等价倍率；交叉编译不表示目标设备运行已验证。

## Out of Scope

本轮不修改保护文件、CI gate、infra tasks/milestone，子代理不提交，是否保存本地commit由主代理统一决定；不推送远端；不向另一个项目的 TFS 发布。无真实需求时不引入 WASM。无需真实付费 LLM 或不受控公网 benchmark，不承诺 Go/Rust 性能比例，不承诺强杀非合作 Go goroutine、自动回滚已发生外部副作用或通用无缝独占资源迁移。

## Further Notes

本文件为 review v0.1，owner/reviewers 是待人类审核的项目角色，未宣称已审核或 active。I_WORK_MATE 的 Skill 提供结构与术语；其必须 active 才实施、逐项访谈与 TFS 审批是该仓库流程，用户已明确授权本轮 go-cli 本地开发并要求自主推进，因此不移植这些前置门。go-cli 未配置对应 TFS adapter；票据采用 to-tickets 明示的本地回退，全部 unpublished-to-tfs，文本依赖不是 TFS 原生关系。

任一 quota 窗口剩余低于 25% 时停止开发和验证，先保存当前成果/WIP、未完成清单与证据，再由主代理按用户要求归档并清理 worktree/测试数据。不得在停工后继续消耗配额实施；不得由子代理擅自删除 worktree 或提交代码。


## 本轮刷新合同

- 默认兼容 eager MCP 和旧 Skill 装配；`skill.progressive`、`mcp.loading: lazy` 或 `runtime.profile: edge` 显式启用。edge 采用 128 项 Skill、2 个活跃 MCP 服务器、131072 字节 prompt/schema 预算，配置可调整且受 validator 上限约束。
- `/reload` 调用主入口绑定的同源 trusted config loader；在 idle 边界编译新的 Skill catalog、PromptPlan、HookChain 与 pre-tool interceptor。未批准的项目新内容返回 needs-trust，非法候选保留旧计划；新轮次采用新计划。用户目录只读前言与有界预读，正文第一次按需读取后缓存至下一次刷新。项目目录必须使用已经捕获且验证的字节。技能正文的实际内容摘要随 skill_load 结果记录。
- MCP 目录中新出现/尚未激活的服务器可重新索引；活跃服务器配置或 schema 的替换使用 `/mcp reload <server>`，由 idle cutover 保留现有 run 的 executor。`mcp_discover` 支持同一 Agent run 的发现→下一request schema→普通prepared MCP调用；全新服务器 append publication 只增加 catalogRevision，不废弃旧request的既有权限快照。
- stdio 的 tools/list_changed 标记 refresh-required，并保留当前请求/执行器直到用户显式刷新。HTTP 当前无通知订阅，显式刷新仍可重新发现。tools/list 分页对两种传输共用 cursor 循环、页数、工具数与总字节预算。协议协商仍明确支持到现有 2025-06-18，不宣称完整实现 2026 协议；server/discover 不等于工具搜索。
- session store、provider、命令代码、扩展二进制/.so 与其他进程级配置不做热卸载。其配置变化返回 restart-required；公开 SDK 的工具、hooks、prompts、commands 与 credential-free provider 均有真实消费者。RPC 是有版本与预算的进程故障隔离，不是 OS 沙箱。
- 数据驱动 hook 仍只支持现有事件，prepared 路径拒绝 post_tool_use；模型/目录元数据与日志不包含原始秘密。Skill 的 allowed-tools 不是授权，附属脚本仍通过单独审批工具调用。

MCP Env 的私有内容 fingerprint 只在进程内用于比较；公开快照记录随机进程 namespace 与单调 opaque revision。secret revision 跨重启不要求内容复放，也不把秘密值或单项秘密摘要输出到日志。


扩展 manifest 的可选 Requires/Provides 现进入真实 CLI 计划：Load/握手后、Init前统一验证缺失依赖、循环与重复 provider；无论 dynamic 开关如何均拒绝无效图并清理已拥有 worker。PluginManager 按拓扑次序 Init，逆初始化次序 Shutdown；动态 snapshot 同时记录 manifest 来源和节点。这些声明表达能力可用性与次序，不提供任意 service invocation 接口。

progressive/lazy 模式即使初始目录或 MCP source 为空也保留发现工具和空 owner，后续 `/reload` 可以加入首个 Skill/服务器。`/capabilities` 输出实际 profile、预算、来源类别和生效时点；dynamic/progressive/lazy 模式开关、profile与MaxMCPServers变更需要重启，prompt 字节预算则随新的 pinned plan 生效。`/mcp reload <server>` 同样拒绝跨项目 trust grant 替换旧 owner，并返回 restart-required。

MCP首次激活与指定刷新在发布前合并完整候选工具集合并校验最终prompt/schema预算；候选失败关闭新连接且保留旧registry、owner和metadata。交互发布一次写入真实schema与执行器，不依赖提交后的额外加载；指定刷新同时切换runtime snapshot，避免资源已替换而元数据仍旧的中间失败结果。

已有活跃MCP owner时，配置来源身份的切换（例如独立用户MCP文件改为inline配置）要求重启，即使服务器字段值相同也不复用旧source owner；同来源的正常显式刷新仍可用。

非lazy MCP有单独的保守合同：默认dynamic=false的eager行为保持兼容；一旦启用dynamic或progressive Skill但仍使用eager MCP，不启动后台自动重载。此组合只要有当前/新MCP来源或已装MCP owner，`/reload`就返回restart-required，覆盖同路径文件改动与文件删除，不能把新元数据发布成旧执行器。若无MCP来源也未安装MCP，Skill/prompt刷新继续可用。需要可刷新MCP及其他能力时，应在启动时选择lazy模式；模式开关本身需要重启。
