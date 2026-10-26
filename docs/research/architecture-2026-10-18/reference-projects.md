# 动态扩展与运行时生命周期：参考项目证据

日期：2026-10-18。本文为 go-cli 动态 skill、MCP、hooks 与 prompt 的架构设计提供一手源码依据。固定版本均从上游官方仓库检出后阅读；记录的是源码行为，不是对其性能、隔离强度或产品稳定性的背书。本文没有运行跨项目性能测试，任何速度结论都必须通过 go-cli 自己的基准验证。

## 结论

建议把动态性分成配置变化和代码变化两类。配置、技能正文、hook 清单、prompt 片段与 MCP schema 变化时，在控制面校验内容和依赖、构建完整候选代，再原子发布不可变快照；调用面用 typed Go 接口派发，不重复解析或装配。静态编入 go-cli 的 Go 扩展可以走同进程 typed API。真正第三方或不同语言的可执行扩展应有版本化公开 SDK；根据调用频率和信任要求选择进程 RPC 或 WASM。现有 `internal` 包不可成为外部 SDK 的依赖合同。

每代能力有独立 owner/scope。每个 turn/run 在准入时获取 generation lease，固定本次配置与能力视图；长寿命 session 不永久 pin 资源代。新 run 可选择新代，活跃 run 在所有子任务、stream 与 commit quiescent 前持有旧代所需资源。旧代停止接新 lease 后排空，按依赖逆序关闭 owner。权限授权单独检查最新 policy epoch，不能因为旧 lease 还活着而保留已撤销的权限。取消、迟到写入隔离和资源预算也必须有独立合同。

性能上不按语言或一次 atomic load 估计端到端能力。受控热路径可按真实 profile 评估分配优化，但不预设 0 alloc/op；须实测相同功能、相同输入下的 allocations、CPU、延迟、吞吐、RSS、GC、重载代价与尾延迟。模型网络和 token 生成应从框架微基准中剥离。

## 证据版本

| 项目 | 固定版本 | 选择理由 |
|---|---|---|
| Magpie | [`d783df46cbac9021e26a6bd71ca0b55544552eb2`](https://github.com/yetone/magpie/tree/d783df46cbac9021e26a6bd71ca0b55544552eb2) | Go 宿主按工作负载划分内建能力、进程内 JS hook、外部插件进程；与用户提供的比较文档版本一致。 |
| DeepSeek Harness（Cordis） | [`687f282e8625c6b8d11ff58211a90524be2e4317`](https://github.com/deepseek-ai/deepseek-harness/tree/687f282e8625c6b8d11ff58211a90524be2e4317) | 本文只提炼服务依赖、Fiber/effect 的所有权与清理，不把 Cordis 的 TS 运行时直接移植到 Go。 |
| HashiCorp go-plugin | [`fa60f8a91608db94647a9dc739b3a33634d35163`](https://github.com/hashicorp/go-plugin/tree/fa60f8a91608db94647a9dc739b3a33634d35163) | Go typed API 通过 subprocess RPC 暴露的生产级样例，包含启动握手、协议版本、checksum、进程管理与自定义 Runner 扩展点。 |
| wazero | [`e234f6fe6ecd4589dd0c643b641bd38f6d826ee5`](https://github.com/tetratelabs/wazero/tree/e234f6fe6ecd4589dd0c643b641bd38f6d826ee5) | Go 内嵌 WASM 运行时的底层控制面、编译缓存、模块关闭、内存和取消配置。 |
| Extism Go SDK | [`de6f8ac95721dff6cf8f5fdca26887699a53b3e9`](https://github.com/extism/go-sdk/tree/de6f8ac95721dff6cf8f5fdca26887699a53b3e9) | 作为 WASM 外部扩展公开 ABI 的候选：跨语言 PDK、manifest、宿主函数与 wazero 封装。只建议进入后续 ADR 与 adoption gate，不把它当本次架构的必选依赖。 |

## Magpie：按扩展工作负载选择运行位置

Magpie 的静态插件运行时不是 Go `.so` loader。内建插件通过 `Manifest` 声明 API 版本、`Requires`、`Provides` 和 `Apply` 回调，runtime 不 import 插件；插件只通过服务相互连接。Apply 注册的 disposer 归调用它的 Ctx 所有，关闭时按逆序执行；runtime 处理 Apply 的 panic/错误并撤销已经登记的内容。[Manifest 与 API 版本](https://github.com/yetone/magpie/blob/d783df46cbac9021e26a6bd71ca0b55544552eb2/internal/pluginrt/pluginrt.go#L24-L71) · [服务、owner 与 disposer](https://github.com/yetone/magpie/blob/d783df46cbac9021e26a6bd71ca0b55544552eb2/internal/pluginrt/pluginrt.go#L91-L119) · [依赖协调、失败回滚与逆序关闭](https://github.com/yetone/magpie/blob/d783df46cbac9021e26a6bd71ca0b55544552eb2/internal/pluginrt/pluginrt.go#L206-L363)。

这与 Go typed in-process 的目标相符：能力在编译时加入，配置决定启用状态，热路径调用已解析的函数和索引。它不能让任意新 Go 机器码在运行中无缝替换。

Magpie 的网关 middleware 是不同的路径：加载时编译 JS 模块，每个 middleware 维护独立 moejs 实例池；请求处理取当前 `set`，调用 hook 有时限；失败和超时会让输入保持原样。当前集合通过 `atomic.Pointer` 发布。[模块编译和实例池](https://github.com/yetone/magpie/blob/d783df46cbac9021e26a6bd71ca0b55544552eb2/internal/middleware/middleware.go#L150-L249) · [hook 时限与错误行为](https://github.com/yetone/magpie/blob/d783df46cbac9021e26a6bd71ca0b55544552eb2/internal/middleware/middleware.go#L277-L299) · [原子发布](https://github.com/yetone/magpie/blob/d783df46cbac9021e26a6bd71ca0b55544552eb2/internal/middleware/middleware.go#L83-L117)。但其变化指纹使用文件名、mtime 和 size，而非完整内容摘要；reload 的 `load` 会记录失败项并继续产出新 set，因而不是整体事务，也没有 generation lease。go-cli 可以借用“先编译后发布”和按请求池化，不应把它当成候选完整、内容寻址和失败保旧的证据。[stamp 与部分加载](https://github.com/yetone/magpie/blob/d783df46cbac9021e26a6bd71ca0b55544552eb2/internal/middleware/middleware.go#L137-L162)

Magpie 子进程插件对活跃 host 做 retire，允许在途请求结束，配置了最长等待时间；流调用 credit window 和全局 budget 限制队列及调用量。budget 统计的是 charged bytes，并明确不包含 wire base64 等额外内存，所以不能将 64 MiB 误读成整个插件 host 的 RSS 硬上限。[流量与内存记账边界](https://github.com/yetone/magpie/blob/d783df46cbac9021e26a6bd71ca0b55544552eb2/internal/plugin/host.go#L129-L178) · [旧 host 排空与超时退役](https://github.com/yetone/magpie/blob/d783df46cbac9021e26a6bd71ca0b55544552eb2/internal/plugin/host.go#L674-L725)。

Magpie 文档自述了其 in-process JS hook 的微秒量级以及 RPC 路径的大致开销，但这不是固定输入下的独立基准，也不可移植成 go-cli 的性能结果。本文不采用该数字做语言或架构排名。

## DeepSeek Harness：依赖服务与清理绑定到 Fiber

Cordis 插件声明所需服务 `inject`，provider 调用 `provide`。服务提供或撤回时，registry 按隔离上下文检查依赖并触发 Fiber 更新；同一服务不允许多个 Fiber 无规则覆盖，撤销 provider 时会等待受影响 Fiber 完成当前生命周期处理。[provider 注册、撤销与依赖通知](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/vendor/cordis/src/reflect.ts#L277-L335)

Fiber 的 `effect` 在 setup 之前就登记为 owner 可见的 disposer；同步 setup 失败会清理已登记效果；异步清理会被 owner 等待；逆序清理、幂等处置、setup barrier 和 reentrant teardown 都有实现。它具体说明了 lifecycle scope 不只是一个 cleanup slice：setup/卸载竞态也须受状态机保护。[Fiber effect 生命周期](https://github.com/deepseek-ai/deepseek-harness/blob/687f282e8625c6b8d11ff58211a90524be2e4317/vendor/cordis/src/fiber.ts#L402-L560)

对 go-cli 的价值是让“提供什么能力、依赖什么、由谁清理”成为可执行的 owner 约束，并把取消传到各个长期操作。需要额外加上的约束是 generation 全图先构建再切换：Cordis 的 provide/notify 面向服务依赖反应，不应被误读为跨所有插件的单点原子发布。

## HashiCorp go-plugin：RPC 是进程边界，不等同 OS 沙箱

go-plugin 启动子进程并经 `net/rpc` 或 gRPC 通信；握手传输 core/app protocol version、socket 类型/地址和协议名。宿主通过 `Client` 管理进程、启动超时、checksum 和关闭；可使用 `SkipHostEnv` 避免继承宿主环境，也可用 `RunnerFunc` 接入自定义运行器。接口调用感觉像 Go 接口，但边界两边仍需实现 RPC 序列化适配。[RPC 架构、协议和限制](https://github.com/hashicorp/go-plugin/blob/fa60f8a91608db94647a9dc739b3a33634d35163/README.md#architecture) · [握手协议](https://github.com/hashicorp/go-plugin/blob/fa60f8a91608db94647a9dc739b3a33634d35163/docs/internals.md#handshake) · [ClientConfig 与 Runner](https://github.com/hashicorp/go-plugin/blob/fa60f8a91608db94647a9dc739b3a33634d35163/client.go#L142-L276) · [checksum、环境和启动路径](https://github.com/hashicorp/go-plugin/blob/fa60f8a91608db94647a9dc739b3a33634d35163/client.go#L638-L747) · [幂等 Kill 与等待](https://github.com/hashicorp/go-plugin/blob/fa60f8a91608db94647a9dc739b3a33634d35163/client.go#L492-L540)。

该库说明了可复用的生命周期/协议机制，但其 README 明确限定在本机可靠网络场景，并未声称适合任意网络服务。子进程崩溃不会直接成为宿主内存里的 panic；然而 RPC 进程本身不会自动提供用户隔离、系统调用限制、CPU/RSS 配额或文件系统隔离。默认启动仍可继承环境；`SkipHostEnv` 可缩小环境暴露，OS 级限制需另由 `RunnerFunc`/平台配置实现。应把“宿主只传 capability”与“插件进程被沙箱限制”作为两个独立合同。MCP 是工具交互协议，不能代替插件版本协商、owner 管理、启动、重载与退役协议。

`SecureConfig` 校验插件可执行文件 checksum，AutoMTLS 认证该 RPC 连接。这类 hash pin 只能证明字节匹配预期 digest；它本身不证明谁发布了该 digest，也不证明 artifact 来源可信。要建立来源信任仍需签名验证、信任根/发布者策略和供应链审计。 [go-plugin 官方 README：安全、local-only 与 shared library 权衡](https://github.com/hashicorp/go-plugin/blob/fa60f8a91608db94647a9dc739b3a33634d35163/README.md#what-about-shared-libraries)

## Go `plugin` `.so`：高性能选项不满足卸载型热替换

Go 官方 `plugin` 包文档说明：动态库以 `-buildmode=plugin` 构建；插件只初始化一次，不能关闭；目前只支持 Linux、FreeBSD 与 macOS。文档警告 host 与 plugin 必须使用完全相同的 Go 工具链版本、构建标记和公共依赖版本，否则会失败或产生难以诊断的问题。[Go 官方 plugin package warnings](https://pkg.go.dev/plugin#hdr-Warnings)

这类加载方式可以共享 Go 数据结构和函数调用，但 `plugin.Open` 没有卸载 API、已运行的包全局状态不会退回；不能以原子替换 registry 指针冒充代码卸载或资源回收。若当前实现使用 native `.so`，generation 只能保证新旧能力视图和请求 pin 的一致性，无法让旧动态库在进程仍存活时真正释放。它不应作为需要无缝替换、失败回退或低资源动态装卸的唯一机制。若继续支持 `.so`，应明确适用平台、构建 ABI 和生命周期限制；更适合作为部署级升级或受控 trusted module 接入点。

## WASM 候选：wazero 底层与 Extism 公开 ABI

wazero 在 Go 宿主内执行 WASM；runtime 提供单独编译和实例化，重复实例化可重用已编译模块。默认 WASI module 不会获得宿主环境变量或文件系统；文件系统需显式提供。runtime 可设置 memory pages 上限，也可配置 `WithCloseOnContextDone` 在 context 取消/到期时终止 guest 调用；该选项默认关闭，并且官方文档明确它有额外成本。运行时和模块有显式 Close 生命周期。[Runtime 编译/实例化](https://github.com/tetratelabs/wazero/blob/e234f6fe6ecd4589dd0c643b641bd38f6d826ee5/runtime.go#L30-L143) · [内存上限与取消语义](https://github.com/tetratelabs/wazero/blob/e234f6fe6ecd4589dd0c643b641bd38f6d826ee5/config.go#L35-L170) · [默认无 guest filesystem/environment](https://github.com/tetratelabs/wazero/blob/e234f6fe6ecd4589dd0c643b641bd38f6d826ee5/config.go#L454-L494)。

Extism Go SDK 建在 wazero 之上，给宿主与多语言 PDK 提供统一的 WASM 插件合同。对 go-cli，主要优点是更容易形成对外稳定 ABI：名字化导出函数以 bytes 输入/输出，config 通过 manifest 传入，宿主函数显式暴露 capability；manifest 可约束 WASM digest、WASM memory pages、超时、HTTP host 与 guest 可见路径。[Extism Go SDK 的 bytes ABI 与 context 调用](https://github.com/extism/go-sdk/blob/de6f8ac95721dff6cf8f5fdca26887699a53b3e9/README.md#calling-a-plug-ins-exports) · [host functions 授权能力](https://github.com/extism/go-sdk/blob/de6f8ac95721dff6cf8f5fdca26887699a53b3e9/README.md#host-functions) · [manifest 类型](https://github.com/extism/go-sdk/blob/de6f8ac95721dff6cf8f5fdca26887699a53b3e9/extism.go#L259-L286) · [memory、timeout 和 runtime 初始化](https://github.com/extism/go-sdk/blob/de6f8ac95721dff6cf8f5fdca26887699a53b3e9/plugin.go#L71-L150) · [manifest wasm digest 校验](https://github.com/extism/go-sdk/blob/de6f8ac95721dff6cf8f5fdca26887699a53b3e9/plugin.go#L190-L220)。

需要加到设计风险里的边界：

- Extism 的 `Plugin` 实例不是线程安全的；跨 goroutine 调用应创建多实例或以池化/串行策略管理。共享编译产物时，编译缓存的寿命必须覆盖所有从它实例化的 plugin。[实例线程安全约束](https://github.com/extism/go-sdk/blob/de6f8ac95721dff6cf8f5fdca26887699a53b3e9/plugin.go#L43-L110) · [go-sdk 缓存寿命说明](https://github.com/extism/go-sdk/blob/de6f8ac95721dff6cf8f5fdca26887699a53b3e9/README.md#enabling-compilation-cache)
- WASM 隔离是线性内存和显式 host capability 边界，不是独立 OS 进程。宿主函数、启用的 WASI、网络 host allowlist 和文件系统 mount 会扩大能力面，必须逐个审计、按 owner 撤权。
- 页面数上限不是进程 RSS 总额度；输入输出缓冲、host 侧分配、WASM runtime、编译缓存和并发实例还要单独预算。超时只在 runtime 开启取消检查且 host function 本身可及时返回时有意义。
- digest pin 同样不是签名或来源可信证明。发布者身份验证应由下载/安装供应链完成，manifest 内容校验只是完整性检查。

因此，WASM 在本次架构中应保持可选，并以独立 adoption gate 验证至少一个真实 use case：外部跨语言 hook/skill 转换；或与其公开 ABI 明确不兼容的能力。若 gate 通过，Extism 是公开 ABI 候选，wazero 是其运行时底座；若只需宿主自用 wasm，直接用 wazero 可减少 ABI 层。两者都不能成为高频内层默认路径。测试需量化冷加载、热调用、实例池、取消、内存/并发配额和完整 prompt/tool 往返开销。

## 对 go-cli 的架构映射

| 能力 | 建议执行位置 | 动态变化点 | 引用依据与边界 |
|---|---|---|---|
| 内建工具、hook、配置化 prompt builder | 同进程 typed Go | 静态代码在构建期接入；运行时仅编译配置/索引；请求持有 generation lease | Magpie Manifest/Apply 给出类型注册和 owner 的样例；配置代的原子切换与请求 lease 是本设计增加的强保证。 |
| Skills | 内容寻址文档与声明式索引；运行期提示词投影走 Go | skill 正文、metadata、权限声明纳入 digest；prompt builder 不在每次请求扫描目录 | 参考 Magpie 的预编译 set；其 mtime/size stamp 不够稳健。 |
| MCP tools | 现有 MCP 协议 adapter + 明确的连接 owner/process 生命周期 | MCP server/schema/content hash 与 tool registry revision 进入候选代；old connection 跟 lease/drain 管理 | MCP 处理工具消息协议；生命周期、版本兼容、启动重试与撤销由宿主单独定义。 |
| 外部高风险/多语言扩展 | subprocess RPC；OS 限制按平台另配 | handshake/API version、固定二进制摘要、启动/健康/退役、调用 quota | HashiCorp 样例；RPC 有隔离作用但不等于系统沙箱。 |
| 外部低能力 WASM 扩展 | 可选 Extism/wazero，经 adoption gate | hash 固定制品、编译缓存分层、实例归 scope 管理、memory/timeout/capability 限定 | Go SDK 可提供通用 ABI；模块/宿主函数权限需独立审计，未选择为当前必需依赖。 |
| Go `.so` | 仅作为受限兼容路径；不作为热替换承诺 | 新版本通过进程部署升级；旧 plugin 在进程退出前留驻 | Go plugin package：不能 close/unload 且构建 ABI 严格。 |

由这些证据推导的候选发布过程为：读取并规范化 config 与所有内容 → 计算包含 skill/prompt/MCP schema/依赖/API 版本/permission revision 的稳定 digest → 校验 DAG 与 capability → 在候选 scope 准备资源及 typed indexes → 全部成功后原子发布 → 新 run 取得新 lease → 旧 generation 拒绝新 run lease 并排空 → 逆依赖清理。任何校验或 setup 失败均清理候选并保留旧代。权限授权不随 generation 固定；撤权线性化后拒绝新 invocation，已取得 token 的 callback 只保证合作式取消与 host completion fence，任意外部副作用仍可能是 unknown/partial。

这是架构建议，不是上述参考项目对 go-cli 能力的现状描述。细化接口、session/turn pin、权限撤销边界、迁移顺序和资源验收应在正式架构与 iteration plan 中规定。

## 性能验证合同

本轮没有生成 Magpie、Harness、go-plugin、wazero 或 Extism 的性能排名。后续公平对照应固定 Go/Rust 工具链、CPU/OS、功能语义、输入字节数、并发度、协议转换与冷/热状态；分别测：

1. 静态 typed dispatch、空 hook 和单 hook 的 `ns/op`、`B/op`、`allocs/op`；只有 profile 证明确有热点后，才为固定小基准另设分配优化门，不预设 0 alloc/op。
2. prompt build、MCP tool JSON 编解码和结果归并的大小分档及 allocs。
3. RPC/WASM/typed handler 调用的 p50/p95/p99、吞吐、CPU 和 RSS；对长流另报告 wire bytes、排队内存、背压等待。
4. 首次构建与单节点/全量 reload 的耗时、峰值 RSS、旧新代并存时间；重载期间正常请求的尾延迟。
5. 被取消调用、策略撤权、provider 崩溃、初始化失败及最终清理时间；确认未产生越过 generation fence 的迟到写入。
6. go-cli 当前实现和候选实现用同一 fixture、真实配置和同一 fake LLM/MCP 服务进行 A/B；外部模型网络时延不纳入框架微基准。

数据至少报告 `go test -bench ... -benchmem`、`go test -race`、Go/Rust 版本、平台、CPU、输入规模和运行命令。若“接近 Rust”成为项目目标，分别报告 `CPU_Go/CPU_Rust`、`throughput_Rust/throughput_Go`、`p95_Go/p95_Rust`、`RSS_Go/RSS_Rust`；先完成代表性 go-cli workload 基线，再由维护者在正式对照前设定每项目标区间。只有完整 agent 回合另设端到端测量；不得从单次 atomic load 或语言声誉推断接近 Rust。
