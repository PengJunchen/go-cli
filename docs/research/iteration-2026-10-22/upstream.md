# 2026-10-22 上游证据与落地选择

本轮使用 research skill，以官方规范、官方仓库固定源码为依据。阅读和下载发生在隔离 worktree；临时原文在 `/tmp/go-cli-iteration-20261022.to3oRg/upstream`。本研究阶段未运行上游/本项目测试或性能测试。本文的设计建议不等同已经实现的功能或性能结论；实现后另填统一验证报告。既有 owner/generation/lease/撤权分析继续引用 [10/18研究](../architecture-2026-10-18/README.md)，这里不重复。

## 决策摘要

1. Skills 按 Agent Skills 标准读 YAML frontmatter，启动只投影 name、description、来源；正文在 activate/read 时加载，references/assets/scripts 继续按需读取。`allowed-tools` 是实验性元数据，不覆盖宿主审批。
2. 将连接懒加载与 schema 渐进暴露分别配置。`mcp.loading=lazy` 使未使用 server 不启动；`tool_search`/activate 使已发现的大工具目录不进入每次模型请求。仅做第二项不能宣称减少 MCP 连接资源。
3. 工具搜索/激活是宿主策略；MCP `tools/list` 仍是分页返回完整 schema，不能假造标准 `tools/search`/`tools/activate`。稳定 server ID、原始工具名、公开别名、内容 revision 必须一起记录。
4. 默认 CLI、SDK、RPC 共用真实 agent/session/工具机制。资源受限构建复用现有入口和装配；`headless,no_plugin` 只排除终端绘制/native `.so`，保留核心 agent、Skills、标准 MCP、审批和会话。
5. 不把 PI 的历史版本描述当作当前事实。当前官方 PI 已内建 MCP、deferred tool search、codemode 和公开 SDK/RPC；其默认延后 schema 不等于按需连接。

## 固定证据版本

| 上游 | 固定版本 | 备注 |
|---|---|---|
| PI | [`earendil-works/pi@bd3c9b823762b7383954aa49e13ee350471015fe`](https://github.com/earendil-works/pi/tree/bd3c9b823762b7383954aa49e13ee350471015fe)；coding-agent `1.3.0` | 原官方 `badlogic/pi-mono` 重定向至该仓库，2026-10-21提交；当前包名 `@earendil-works/pi-coding-agent`。 |
| DeepSeek Harness | [`687f282e8625c6b8d11ff58211a90524be2e4317`](https://github.com/deepseek-ai/deepseek-harness/tree/687f282e8625c6b8d11ff58211a90524be2e4317) | 2026-10-17，与10/18研究固定版本相同。 |
| MCP 官方规范仓库 | [`80041ab0a139a8f4fb3adbc99f7ae9113ea59671`](https://github.com/modelcontextprotocol/modelcontextprotocol/tree/80041ab0a139a8f4fb3adbc99f7ae9113ea59671) | 2026-10-20；已发布 `2026-07-28`，同时保留 `2025-11-25`历史规范。 |
| OpenCode v2 | [`4617210822bdbb31e5749751afbfb4584ccedac8`](https://github.com/anomalyco/opencode/tree/4617210822bdbb31e5749751afbfb4584ccedac8) | 沿用10/18 v2源码研究。当前默认 `dev` 是另一个提交，不能冒充 v2 基线。 |

## DeepSeek Honeycomb 归属核验

截至本次核验，[`deepseek-ai/DeepSeek-V4`网页](https://github.com/deepseek-ai/DeepSeek-V4)与[GitHub官方API](https://api.github.com/repos/deepseek-ai/DeepSeek-V4)均返回404。对官方 `deepseek-ai/deepseek-harness` 上述 SHA 的递归文件树以及下载源码执行不区分大小写 `honeycomb` 文本检索，未发现该名称；官方README使用 DeepSeek Harness/DSH/Cordis。因此**无法确认用户提到的 Honeycomb 是 DeepSeek 官方项目、此仓库别名或具体功能名称**。404不证明历史从未存在/私有仓库不存在；搜索不到也不证明不存在。本文不把任意同名第三方项目归到 DeepSeek 名下。

可以确定的实际 MCP 行为来自官方 [MCP子系统说明](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/docs/subsystems/mcp.md)、[client配置与限制](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/mcp/mcp-client/README.md)：每个 server 是独立连接插件，默认不启用；初始连接和工具发现属于启动工作，慢server可能延迟启动。现代SDK或legacy通知驱动工具更新；refresh失败保留旧工具代；resource discovery/read按需，不在连接时抓全部资源正文。server instructions是有大小上限、带来源的literal prompt内容。该版本明确不支持 MCP prompt templates。

可借鉴的是按server owner、last-known-good、稳定namespace、资源按需与真实状态展示；**不能据此宣称 DSH 已实现server连接懒加载或所有MCP schema渐进激活**。其源码 [connection.ts](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/mcp/mcp-client/src/connection.ts)、[tools.ts](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/packages/mcp/mcp-client/src/tools.ts) 给出了串行刷新、连接代际和工具桥接的具体实现。

## MCP 标准边界与宿主发现机制

### 兼容版本必须如实声明

`2026-07-28` 已是正式规范，不能把 `2025-11-25` 称为最新。现代版本新增 `server/discover`（版本/身份/capabilities，而非tool搜索）、每请求 `_meta`、`subscriptions/listen`、MRTR、`resultType` 与 cache freshness；`initialize` 属于legacy路径。若本迭代仅实现legacy handshake，应继续准确标注所支持版本，不能仅修改version字符串。参考 [新版本变更](https://github.com/modelcontextprotocol/modelcontextprotocol/blob/80041ab0a139a8f4fb3adbc99f7ae9113ea59671/docs/specification/2026-07-28/changelog.mdx)、[server/discover](https://modelcontextprotocol.io/specification/2026-07-28/server/discover)、[deprecated](https://github.com/modelcontextprotocol/modelcontextprotocol/blob/80041ab0a139a8f4fb3adbc99f7ae9113ea59671/docs/specification/2026-07-28/deprecated.mdx)。本轮源码实际声明 `2025-06-18`，不可据下文参考的2025-11-25规范声称已支持该版本。最小交付应优先完善已经声明支持的协议行为，再独立实现modern兼容适配，不能半实现混用。

### 必须贯通的legacy合约

| 能力 | 标准行为 | go-cli选择 |
|---|---|---|
| Tools | capability `tools`；`tools/list(cursor)` → tools + optional nextCursor；`tools/call(name,arguments)`；可选 `notifications/tools/list_changed` | 拉全本次分页再发布目录；限制页数/数量/总字节；重复cursor或重复原名报错，失败保留上次完整目录；模型schema与executor同revision。 |
| Prompts | capability `prompts`；`prompts/list`分页；`prompts/get`按需；可选list_changed | slash/交互显式选择；保留返回messages/role/content，不能把prompt伪装成无参tool。 |
| Resources | capability `resources`；list、templates/list均分页；read按URI；可选list_changed及逐资源updated | 先显示元数据，实际read按需；server+URI配对，保留rich content或明确不支持，不能假成功只丢弃blob。 |
| 分页 | cursor是不透明token；不假设页长；缺nextCursor结束；不跨连接session持久化cursor | 增加有界循环、取消、cursor-cycle检测；不以空页当完成（仍可能有nextCursor）。 |

来源分别为 [tools](https://modelcontextprotocol.io/specification/2025-11-25/server/tools)、[prompts](https://modelcontextprotocol.io/specification/2025-11-25/server/prompts)、[resources](https://modelcontextprotocol.io/specification/2025-11-25/server/resources)、[pagination](https://modelcontextprotocol.io/specification/2025-11-25/server/utilities/pagination)。现代版仍保留这些RPC，但change notification要通过所声明类型的 `subscriptions/listen` 流订阅，并处理 `ttlMs/cacheScope` 等新字段，参考 [modern tools](https://github.com/modelcontextprotocol/modelcontextprotocol/blob/80041ab0a139a8f4fb3adbc99f7ae9113ea59671/docs/specification/2026-07-28/server/tools.mdx)。

### 可执行的渐进目录设计

- 控制面有轻量 `server{name,description,transport,state,revision}` 与 `skill{name,description,path,digest}`。lazy模式初始化只建立descriptor；显式load或首次相关发现连接目标server，single-flight避免并发重复启动；错误保留可诊断state并允许重试。
- `tool_search(query,server?,limit?)` 是宿主工具，只检索获准metadata，结果给稳定ID、名称、摘要、server、revision。若catalog还未加载，按明确server提示连接；泛搜是否预连全部server必须是显式策略，不能暗中抵消lazy收益。
- activate/search命中后将所选完整 inputSchema 注入**下一次模型请求**。普通provider无法在本次正在流式响应的请求中途增加tool declarations。新的请求快照同时固定schema和executor；本次未公布的工具不能被普通直接调用绕过。
- schema激活可按branch/run保存；撤权/删除必须撤可调用性，不因历史激活永久放行。同名跨server稳定namespace；规范化引入碰撞时用稳定hash或拒绝，并保留原名派发。
- 最小初始模型tools应为完成代码任务所需基础工具 + search/skill-read等少数发现工具；不能把工具搜索当作权限检查或模型必定会找到工具的保证。catalog容量与prompt schema字节数分开预算；初版确定性关键词排序即可，不引入embedding服务。
- UI `/capabilities` 显示 discovered/active/deferred/unloaded/error 等准确计数、来源、revision；`/mcp load <server>`、reload、retry可以解释实际状态。工具发现和激活事件进入普通trace，不能把凭证或全环境写日志。

以上是本文设计推导，不是MCP新增RPC。参照PI当前 [MCP exposure](https://github.com/earendil-works/pi/blob/bd3c9b823762b7383954aa49e13ee350471015fe/packages/coding-agent/docs/mcp.md#control-tool-exposure)：`direct`直接声明、`deferred`通过tool_search加载、`codemode`通过程序化发现（默认）、`hidden`隐藏。PI连接所有enabled server的后台启动，与go-cli所追求的真正server lazy连接须分开评价。

## 标准 Skills

[Agent Skills规范](https://agentskills.io/specification)要求目录里的 `SKILL.md` 为 YAML frontmatter + Markdown；必填name/description，name与父目录相符且有字符/长度约束，metadata为string map；正文内容无固定格式。规范推荐启动metadata、激活正文、随后按需resources三级披露。目录下脚本不是宿主可自动执行的初始化钩子。

PI [skills文档](https://github.com/earendil-works/pi/blob/bd3c9b823762b7383954aa49e13ee350471015fe/packages/coding-agent/docs/skills.md)实现相同思想。go-cli应对标准目录兼容，保留旧skill YAML格式为明确legacy adapter；不能为了兼容只接受description字段却默默忽略错误frontmatter。完整读取时校验同一内容摘要、限制正文大小并验证相对资源路径，避免索引和激活正文变化未被察觉。

## PI 当前能力、交互与轻量选择

当前官方 [README](https://github.com/earendil-works/pi/blob/bd3c9b823762b7383954aa49e13ee350471015fe/packages/coding-agent/README.md)、[how it works](https://github.com/earendil-works/pi/blob/bd3c9b823762b7383954aa49e13ee350471015fe/packages/coding-agent/docs/how-pi-works.md)明确所有交互、print/JSON、RPC、SDK共享agent/session机制；会话JSONL树保存原始记录，compaction只改变后续模型上下文。扩展可注册工具/命令/shortcut/provider/hook/UI，skills/prompts/themes/packages可独立分发。当前默认版本并非历史fork文档中的旧包名和纯4工具印象，测量务必固定实际配置与启用功能。

[TUI文档](https://github.com/earendil-works/pi/blob/bd3c9b823762b7383954aa49e13ee350471015fe/packages/coding-agent/docs/tui.md)值得借鉴：简单选择/输入使用统一ctx.ui API；状态/通知不阻塞主操作；复杂custom component才接管绘制。组件只在状态或主题变化时invalidate，render请求合并；以可见终端列宽处理中文/emoji/ANSI；焦点与光标传递保障IME；regular模式交给终端scrollback，fullscreen模式才接管mouse。go-cli先保留一致的run/tool/审批/cancel事件与可搜索slash，不引入第二套renderer。

[package.json](https://github.com/earendil-works/pi/blob/bd3c9b823762b7383954aa49e13ee350471015fe/packages/coding-agent/package.json)声明Node >=22.19.0，同时存在Bun compile构建（另复制theme/assets/docs/examples/WASM）。因此安装footprint、Node/JS运行时、发布archive大小、独立binary大小都必须分别报，不能把完整Node安装和Go单binary归在同一个“二进制大小”数字。PI“minimal”是产品定位，不能由文字推导RSS/启动胜负。

本地 import 审计：`internal/cli/assemble.go` 仅为 `tui.ApprovalRequest` DTO就导入TUI；slash依赖具体ThemeManager；interactive.go混合通用history/token usage helper与Bubbletea；REPL/Todo/Transcript投影也导入TUI。native plugin已有 `no_plugin` build tag但stub的构造器option不完全一致。真正headless方案须抽离审批DTO、主题窄接口、共享helper，再按tag去掉绘制；不能另写删掉Skills/MCP/session的新玩具入口。runtime `edge` profile可减少后台工作，**不会自动减少binary链接体积**。

## OpenCode v2 对照

继续采用 [10/18 v2报告](../architecture-2026-10-18/opencode-v2.md)：显式owner/scope、不可变tool request snapshot、候选配置先校验后发布、权限在执行边界重验。v2已有按需Skill正文、每server独立owner/readiness、异步MCP启动及CodeMode工具目录；不据此承诺通用generation lease、独占资源无停顿切换或所有setup失败原子回滚。

本次查询默认dev HEAD为 `2fde77c9a0a442024469a6e56f5130c993865e5b`；没有重新审阅该HEAD全部代码，不把它等同10/18固定v2结论。10/18记录的官方tag [`v2.0.32`](https://github.com/anomalyco/opencode/releases/tag/v2.0.32) 指向 `843a6c514e606b436a7fe713422ace0a93ad3d61`，与本报告架构源码SHA不同。该release网页只有自动Source code zip/tar.gz；API release/tag返回404不能否认实际tag。发行package已改为 `@opencode/cli`：官方npm [`@opencode/cli@2.0.32`](https://registry.npmjs.org/@opencode%2fcli/2.0.32) 与platform `@opencode/cli-darwin-arm64@2.0.32`可获得。旧 `opencode-ai/2.0.32`返回404且旧 `opencode-ai@next`指2026-06快照，不能拿旧分发当v2。已准备下载该v2预编译包作为可选实际比较，无需构建monorepo；研究阶段仍未运行性能测试，故不填排名。

## 可复现实验协议（开发结束后统一执行）

将功能正确性、框架资源成本和真实模型任务质量分开，所有结果保留raw records及缺失/失败样本。先锁协议、工具链、项目SHA、模型ID/版本、配置/工具集合、主机CPU/RAM/OS/电源状态；再运行，避免看到结果后调目标。

| 场景 | 对等工作与测量 |
|---|---|
| build与footprint | Go默认/headless,no_plugin同版本strip构建；CGO=0 linux/amd64、arm64产物；报告字节、archive、安装总量、动态依赖。PI固定1.3.0官方发布/pack或固定源码bundle，Node另列，不混单位。cross compile成功不等于目标板运行过。 |
| startup | version/help只作为loader成本，另测真实agent-ready、首次model-request、首token；相同空home/新workspace/无network updater策略；冷OS缓存和进程重启warm分开，不能把fake prompt当完整coding benchmark。 |
| agent loop | 两边同一loopback scripted OpenAI-compatible SSE provider、同一工具任务（read→edit→verify→final）、相同输入输出字节；记录正确输出、请求数、工具数、total wall/process CPU/peak RSS/子进程RSS；失败单列。 |
| lazy能力 | 0/10/100 server descriptors，0/100/1000工具与0/100/1000 Skills；启动必须检查真实子进程数和连接数。搜索一次、激活、调用一次；分别记录startup/first-use延迟、schema字节/模型token（有同一tokenizer才报token）、RSS和刷新成本。 |
| hot reload | 有在途调用的单server/tool/skill变化；候选失败、list_changed风暴、分页cursor-cycle、删除/revoke；测p95、旧代存活时间和bounded close，正确性失败时不报为“性能提升”。 |
| interaction | replay相同流式文本/大tool输出/中文emoji/resize/cancel/审批；记录首可见内容、input响应和render工作。人工评价同一任务盲序，维度预先确定。 |
| 真实任务质量 | 独立sandbox、固定真实repo提交+隐藏deterministic grade，同provider/model与工具语义、同预算，多次配对运行AB/BA交替；报pass rate、成本、tokens、tool errors、wall和失败原因，不能用help启动替代。 |

框架重复微基准建议5 warmup+至少30 measured process runs；使用交替/随机AB顺序，报median/p95及样本数，样本足够才报p99。RSS统一OS单位并记录peak是parent还是整个进程树；macOS ru_maxrss与Linux单位不同。真实任务重复≥5并报告置信区间/失败率；该数只能提供初步稳定性，不能证明普遍优越。无API预算/相同模型可用性时真实任务明确未执行。

PI官方 [evals/README](https://github.com/earendil-works/pi/blob/bd3c9b823762b7383954aa49e13ee350471015fe/packages/evals/README.md)提供可借鉴的严谨实践：先规划(case,variant,model,run) cohort、每arm新容器、AB顺序交替、完整expected-runs与observations、精确pair匹配；missing/duplicate/error不按零分吞掉，阻塞pair不发布headline lift。它主要评估文档效果，不是PI对go-cli的性能成绩；本轮只阅读协议未执行。

## 本轮实现采用与后续范围

以上标准表与实验表是研究输入，不能直接计作交付功能。实际消费及完整验证以 [usage](../../iterations/2026-10-22-dynamic-runtime/usage.md)、[verification](../../iterations/2026-10-22-dynamic-runtime/verification.md) 和最终测量报告为准。

| 研究建议 | 本轮采用 | 保留到后续 |
|---|---|---|
| 标准 Skill 三级披露 | edge/progressive 目录/模型先投影 name/description，`skill_search` / `skill_load` 按需materialize正文；用户完整正文lazy读缓存到 `/reload`，verified project 启动沿trust完整捕获有界正文 | 辅助 scripts/resources 不自动执行；实验性 allowed-tools 不替代权限 |
| MCP 连接 lazy 与 schema deferred 分开 | lazy 未选 server 零连接；`mcp_discover` 按 **server** 连接、预算候选与激活 schema，下一模型请求生效 | 未实现逐工具语义 `tool_search` 选择/排名；不冒称标准 search/activate RPC |
| Tools 分页与目录一致性 | 有界分页、cursor/重复项检查、候选 schema/executor 同步发布；失败保旧 | 当前声明2025-06-18；未实现modern2026 handshake/subscription/cache 合同 |
| Prompts / resources 标准合同 | 上表仅供设计参考 | 本轮未实现标准 prompts/list/get、resources/list/read 全链路；不可算作通过验收 |
| 工具变化通知 | stdio tools/list_changed 标记 refresh-required，显式 idle reload；dynamic+eager 保守 cold | HTTP change subscription 尚未实现 |
| 计划更新与权限 | generation/run lease、idle 候选发布、admission/completion guard、可信入口 loader 的 `/reload` | provider/session/扩展代码、profile、加载模式、active MCP 上限等 cold 改动需重启 |
| 可分发外部扩展 | public SDK + JSONL worker 工具/hook/command/prompt/complete-response provider；独立 module 只依赖 public API/stdlib | RPC Skill DTO 和逐 token provider stream 未实现；process/hash 不等于 OS sandbox |
| 边缘运行与二进制 | 同一入口/核心的 `headless,no_plugin`，CGO0 多平台编译、独立行式交互合同 | 目标 Linux 板与 Windows runtime 未实测；headless 无 rich modal/theme/in-flight keyboard steering |
| 科学评价 | 固定 CLI task、官方发行版本、构建来源、完整参数与重复测量；edge 原始入口单独 integration | 未运行真实模型任务质量评价；未实测0/100/1000完整能力规模，不能由固定fixture外推 |

metadata-first不等于所有项目正文启动零磁盘读取：project source capture 为安全合同完整捕获有界正文；目录与模型投影、materialization才按需，用户Skill完整正文另有lazy读缓存。

研究阶段“相同空home”只是可选实验模板。实际 CLI 协议保留宿主 HOME：metadata preflight 拒绝已知 Go 个人配置/能力来源，空 project Skill 目录遮蔽默认发现；PI 使用自身资源目录与禁用个人能力的flags；OpenCode 使用XDG/config与官方 OPENCODE_TEST_HOME资源根，并检查系统/MDM配置不存在。未读取个人内容，也不将该资源根隔离描述为操作系统沙箱。正式结果会记录这些具体参数，不能将旧研究模板当作实际测量步骤。

## 原始 Builder验收输入（设计目标，未实现项见上表）

- 全默认入口与SDK实际使用新目录；元数据可见→搜索→加载/激活→下一LLM请求schema→真实执行→撤销，不能只测registry。
- 未用lazy server启动零连接；并发首次调用只启一次；失败可重试；退出真实process退出。
- Skills标准fixture和MCP分页/通知/prompts/resources全链路在统一验证阶段覆盖；超限/错误保留旧代且暴露诊断。
- headless tag import graph无Bubbletea/Glamour/Chroma，no_plugin构造器API一致；同核心prompt/SDK/MCP/Skills行为，清楚列出缺少的终端绘制功能。
- 先实现所有功能，最后统一跑测试/verify/build/benchmark；任何暂未验证功能与数字保留未验证标签。
