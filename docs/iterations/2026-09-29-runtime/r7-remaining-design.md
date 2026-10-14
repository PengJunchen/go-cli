# R7 剩余 Hook / Skill 接线合同

状态：builder handoff，基于冻结源码基线 `090303945508076bd2f4193d01305135e2e4dd6a`。这是设计，不是实现、测试结果或 R7 phase closure。范围为 Hook bridge、Skill 默认 CLI 路径，以及它们必须消费的 R6 项目可执行源信任快照。R1/R2/R4 已接受合同不在此重做。

## R7 trace / audit carry-forward（本 handoff 不关闭）

本文件只细化 R6 source trust、Hook 和 Skill 切片；R7-DES-01 与 R7-BLD-02 对应的 `TRACE-01..06`、`APR-007`、`RT-09` 仍须完成实施与复审，不能由本 handoff 或其测试清单视为关闭。它们的原始设计、测试名和边界见 [`r7-lifecycle-design.md`](../2026-09-25-runtime/r7-lifecycle-design.md#L9) 及 [`execution-tasks.json`](../2026-09-25-runtime/execution-tasks.json#L1543)。

- **生产 trace 链**：真实 `cmd/cli` invocation 持有唯一 root Tracer/exporter，并把同一 Tracer 传入 Assembly；CLI root span 覆盖命令，Harness/Run span 覆盖异步 Run 完成（[`execution-tasks.json:593-605`](../2026-09-25-runtime/execution-tasks.json#L593)）。Assembly 先停 span producers 并关闭，再结束 root span，最后由 root finalizer 在独立有界 cleanup context 下关闭唯一 exporter（[`r7-assembly-close-handoff.md:17,34,55`](../2026-09-25-runtime/r7-assembly-close-handoff.md#L17)）。
- **隐私、递送与关闭**：默认 trace 不含 prompt/secret，失败 span 为 ERROR；`none`/disabled 不创建 sink、输出或 async worker，unknown exporter 启动失败。队列满、sink/batch 失败、shutdown abandon、pending 与 close errors 由同一 finalizer 的 bounded `ShutdownReport` 区分并影响命令退出，报告不含原始 payload；非合作 callback 到期只能报告 pending/quarantined 并保留 owner/sink，不能伪称 drained/closed。隐私与生命周期细节见 [`r0-contract.md`](../2026-09-25-runtime/r0-contract.md#L14) 和 [`r7-lifecycle-design.md:129-161`](../2026-09-25-runtime/r7-lifecycle-design.md#L129)。
- **审计、成本与 stats**：approval audit 分开记录 classifier 结果、用户最终选择和工具结果，hash 只在 durable append 成功后前进；usage 或价格未知时保留已知部分并显示 unknown/unavailable，不能伪装成零。R3 成本只作报告、不引入硬预算；任何未真实 enforce 的 hard-budget 声明必须拒绝或降级，不能把未知 usage/price 当零来宣称金额门限有效（[`r3-provider-design.md:150`](../2026-09-25-runtime/r3-provider-design.md#L150)）。对应 audit/stats acceptance 在 [`r7-lifecycle-design.md:221-226`](../2026-09-25-runtime/r7-lifecycle-design.md#L221) 与 R7-BLD-02 task (`execution-tasks.json:1634-1673`)。
- **状态边界**：ledger 中 R7-DES-01 仍为 `pending`；R7-BLD-02 为 `in_progress`，仅接受 partial telemetry 与 bounded async-exporter/CLI-finalizer slices。Assembly/REPL/serve 生命周期、完整 close-error 传播、剩余 audit/privacy 和 R7-REV-01 仍开放；本 handoff 只固定跨切片依赖，不扩大源码授权或声称测试通过（[`execution-tasks.json:1543-1591,1634-1673`](../2026-09-25-runtime/execution-tasks.json#L1543)）。

## 基线事实与 R6 信任前置条件

R6 原始选择要求持久化 manifest 绑定 canonical workspace root 和完整 executable-source 集：六个根配置候选的存在/缺失及现存字节，hooks、MCP、skills 与项目配置引用的可执行来源的字节及排序成员关系。相同冻结字节必须用于摘要、解析和注册；候选新增、删除、重命名、内容变化、不可读或 canonicalization 失败都 fail closed 并要求显式重新授权。路径许可或仅限本次进程的摘要不满足此合同（[`r6-entrypoint-design.md:40-44`](../2026-09-25-runtime/r6-entrypoint-design.md#L40)、[`r7-lifecycle-design.md:96`](../2026-09-25-runtime/r7-lifecycle-design.md#L96)）。

当前源码尚未实现该 handoff，具体差距如下：

- `approval.TrustManager` 是必需的现有接口；实际实现为 `DefaultTrustManager` 包装 `TrustStore`，没有 `FileTrustManager` 类型。`TrustEntry` 目前只有 path、单一 fingerprint、时间和过期字段；`FileTrustStore` 持久化 JSON，但 `main` 使用 `NewDefaultTrustManager(nil)`，所以生产 CLI 现在是内存存储。
- `DefaultTrustManager.IsTrusted` 只按传入 path 查找并检查过期；它不 canonicalize 路径、不比较源内容。`TrustProject` 只 fingerprint `.go-cli/mcp.json`；读取失败时 `contentFingerprint` 退回 hash 路径字符串。该 fingerprint 不能证明 MCP、hook、config 或 skill 内容曾获授权。
- `config.LoadTrusted` 的 callback 只返回路径级 bool；批准后 `NewLoader().WithFile(projectPath)` 再按路径读项目配置。`cmd/cli/main.go` 创建的局部 manager 与 `registerSkillTools`、MCP/custom-tool assembly 通过 `approval.GetTrustManager()` 取得的进程级 manager 是不同实例。
- `config.LoadTrusted` 把 project config merge 到同一个 `Config`，不保留 per-field source provenance；MCP discovery 在 `rc.MCP.Servers` 非空时直接将其视为静态配置并提前返回。因此 project-derived servers 可伪装成 user/global static servers，绕开 source-manifest/revoke 检查。后续 MCP-generation CLI assembly 必须依赖本 handoff 的 verified capability/source provenance；在接线完成前，来源不明的 merged servers fail closed。
- `internal/cli/cli.go` 已有 `CommandRegistry` 和内建命令注册点，但当前没有 production `trust` 命令；`internal/cli`/`cmd/cli` 中没有生产 `TrustProject` 或 `RevokeTrust` 调用点。当前 `FileTrustStore` 和默认 trust manager 不能因此被称为可用的持久授权流程。
- `cmd/cli/runWithInvocationWaitBudget` 目前先调用 `loadConfig`，之后才进入 `cli.Run`；配置损坏会在 help/version dispatch 前直接退出，因此 `--help` / `--version` 不能遵守 R6 的 config-independent root contract。
- `registerSkillTools` 对 explicit `Skill.Dir` 不做同等 trust 检查，并按路径调用 `LoadDir`。`filepath.IsAbs` 与“自动发现/显式配置”都不是来源授权。配置合并后 `SkillConfig` 只保留 `Dir`，没有来源 provenance。

### 唯一共享快照与持久授权设计

在现有 `approval.TrustEntry` / `TrustStore` / `TrustManager` 上扩展，不建立第二个 trust store、全局 manager 或 capability registry。保留现有 `TrustManager` 接口供既有调用者使用；新增可选能力接口，签名固定为：

```go
type FrozenProjectSourceTrust interface {
    CheckProjectSourceSnapshot(context.Context, *ProjectSourceSnapshot) (*VerifiedProjectSources, error)
    TrustProjectSourceSnapshot(context.Context, *ProjectSourceSnapshot) error
}
```

只有 `CheckProjectSourceSnapshot` 在现有 store 中成功核对 manifest version、entry expiry、canonical root、完整 source set、presence、directory membership 和精确 bytes digest 后才能返回 `*VerifiedProjectSources`。该类型字段和可用 constructor 均为 `approval` 包私有；零值无效，且无快照访问能力。它是唯一能交给 runtime/assembly 的 authority proof；`ProjectSourceSnapshot` 与 presentation status 都不是 proof。所有加载 project executable sources 的路径都必须要求非零、由此 manager mint 的 capability；只有旧路径接口的注入 manager 一律视为“不支持冻结源信任”，并 fail closed。

定义一个不可变、opaque 的 `approval.ProjectSourceSnapshot` 与单次 capture builder。`approval.NewProjectSourceSnapshot(root, fileInputs, directoryInputs)` 是 public capture-input constructor，不是授权入口；它只建立未验证的 raw snapshot，私有保存 canonical root、manifest version、按稳定 source ID 排序的 source 记录、排序 directory-member 记录、每个 present 文件的精确 bytes，以及基于它们生成的 manifest digest。source ID 使用 canonical root 内的相对路径和来源类别；记录含 `Present`、relative path、SHA-256。directory 记录含 present/absent 与完整的排序成员路径清单。构造时复制输入 bytes；快照访问器只能返回副本。禁止把可变 map/slice 或绝对原始输入路径作为身份。raw snapshot 仅用于 manifest 比较、TTY grant 展示/持久化和无副作用的 source discovery；不得用于 Config merge、MCP/hook/skill/command/custom-tool 注册或 assembly。

`VerifiedProjectSources` 仅暴露 defensive-copy 只读 accessor；这些 accessor 从其私有 frozen snapshot 返回 root-relative source bytes/member lists。不能从 raw snapshot、exported `Config` 字段、JSON decode、普通 constructor 或 `ProjectSourceTrustStatus` 转换/构造 verified capability。即使 caller 自行建立 `Config{ProjectSourceTrustStatus: trusted}`，没有 manager-minted verified capability 也不得加载或 publish 任何 project executable source。

快照 digest 使用 SHA-256，采用版本化 domain tag 和明确长度前缀编码：canonical root、每条按 source ID 排序的 source identity、relative path、presence marker、现存 bytes 长度与 bytes、每个 directory identity 及其排序成员数和每个成员的长度与路径。逐文件摘要也从同一 bytes 计算。manifest 比较必须核对版本、root、所有 presence/member/bytes digest 记录和总 digest；不能只比较目录名或旧 `Fingerprint`。

持久 `TrustEntry` 增加 manifest version、canonical root、逐 source presence/path/digest、逐 directory presence/排序 membership 和总 digest。旧 entry 缺 manifest version 或只含 MCP fingerprint 时保留为 legacy 记录供 `trust list` 显示，但绝不授予新 project config、MCP、hooks、custom tools 或 project skills；不得在启动时静默迁移或重算其授权。`TrustProject(ctx, path)` 的 path-only 形式不能授权 executable sources；新 CLI grant 使用 `TrustProjectSourceSnapshot`。

`config` 是 source inventory 与配置解析的 owner：新增窄的 `CaptureProjectSources(ctx, canonicalRoot, globalConfig)`，在一个 capture builder 中完成：

1. Canonicalize workspace root（绝对、解析 symlink 后的真实目录）；失败即拒绝 project sources。逐项 capture 当前 config loader 的六个根候选，顺序固定为 `.go-cli.yaml`、`.go-cli.yml`、`.go-cli.json`、`go-cli.yaml`、`go-cli.yml`、`go-cli.json`，缺失项也写入 manifest。每个 present candidate——包含被高优先级文件遮蔽的 lower-priority candidate——都必须只打开/读取一次，其精确 bytes 和 digest 都进入 manifest；只有最高优先级 present candidate 会被选中解析，且解析使用同一 capture 中已保存的 bytes。candidate 格式错误/非 regular/symlink/读取失败都报错，不回退到较低候选。
2. 把选中配置从 builder 中已有的 bytes 解析成 discovery config（只解析数据，不执行/注册）。从该 config、global config 中指向 workspace 的 executable refs 与固定默认路径收集 `.go-cli/hooks.yaml`、项目 `.go-cli/mcp.json`、默认或配置指定的 skill/command/extension 目录，以及所有其他 workspace 内 config-referenced executable source。用户 global config 本身不进入项目 manifest；但它引用的 workspace 内 executable file/tree 作为 project source 纳入。项目 config 引用 canonical workspace root 外的 hooks、MCP、skills、commands、extensions 或其他 executable source 一律 fail closed；不能以“可能有 independent authority”作为放行条件。仅 canonical user-skill root 可由 user global config 通过独立 bounded user loader 使用，且绝不继承 project trust。路径先 canonicalize，再以 `filepath.Rel(canonicalRoot, resolvedPath)` 判定 containment；`filepath.IsAbs` 不构成 authority。
3. 对所有自动发现或 config 引用的目录记录存在状态和完整候选 membership；每个候选源只读取一次。文件 bytes 同时进入 snapshot digest、project config parser 和后续 consumer。物理 capture 完成后再核对现存 manifest；任何 source-set / membership / digest 改变时拒绝整组 project executable config，不做部分应用或同次启动 fallback。

manifest capture 的固定资源界限同样约束 `skill.SourceSnapshot`：每个扫描 root 最多 4,096 个 entry、DFS 深度 128、每 root 最多 128 个 skill candidate；每个可执行来源文件最多 1 MiB，整次项目 snapshot 累计最多 16 MiB。用 `os.File.ReadDir(1)` 做 DFS，逐 entry 计数（含忽略名称），读取固定 32 KiB chunk 并逐 chunk 检查 context；排序只在 bounded candidate 集收集后按 canonical-root 相对路径进行。资源超限、取消、symlink、候选 non-regular file、任何被跟踪目录不可读都拒绝整次项目快照；不得使用 `filepath.WalkDir` 或 `os.ReadDir` 先展开无限目录。候选 symlink 和 symlink directory 不跟随；所有用于解析的 bytes 必须来自 capture，之后不按源 path 重开。

`config.LoadTrustedWithManager(ctx, tm)` 是生产入口：先以现有 `Loader` 读取 global config；capture 全部六个根 config candidates 和完整项目 executable source set；要求 `tm` 实现 `FrozenProjectSourceTrust` 并用同一 raw snapshot 校验；只有 `CheckProjectSourceSnapshot` 返回有效 capability 后，才从 capability 暴露的冻结 bytes 加载/merge 项目 config，并将该 capability 放入 `Config` 的 private `json:"-"` 字段。加载器同时在 private fields 中保留清晰分离的 user/global config 与 project-only overlay（两者都由正确 provenance 的 loader 构造，不从 merge 后字段反推）；project overlay 只在 manager-minted capability 存在时可供 assembly 读取。`Config` 可另带公开或可序列化的 `ProjectSourceTrustStatus` 作 presentation/diagnostic，也可私有保留 raw snapshot 供显式 `trust grant`；这两者绝不是 assembly authority。为跨 `config` 与 `internal/cli` 窄接线，提供 `(*Config).VerifiedProjectSources() *approval.VerifiedProjectSources`，只返回内部 capability（无 capability 返回 nil），`(*Config).ProjectOverlayForVerifiedSources() *Config` 仅在同一 private capability 存在时返回 project-only overlay 的 defensive copy（否则 nil），以及仅 grant command 使用的 `CapturedProjectSourcesForGrant() *approval.ProjectSourceSnapshot`；raw getter 从不被 assembly 接受。只有 grant 可读取当前 raw snapshot 并在 TTY 确认后持久化；grant 本身不在当前命令内 mint 或使用 assembly capability，授权要到后续启动时再次通过 store check 才生效。这样 grant 复用本次 capture，不按路径重读。任何手工构造、JSON decode、仅 status=trusted、raw snapshot 或未验证 manager 都无法填充有效 private capability；assembly 未取得 capability 时不 merge project config、不注册任何 project-sourced executable feature。为实现 byte-only 解析，给 config loader 增加 `WithFileBytes(originPath, bytes)` 等窄入口，使 format 按 origin path 判定但解析不触盘。未信任、manifest 变更、旧 entry、manager 能力缺失或 capture 失败时返回可呈现的 typed denial/status；global-only CLI 可继续运行。取消必须传播为取消错误。现有 `LoadTrusted(callback)` 若为 API 兼容保留，callback 的 path bool 不能再授权生产 project executable sources；生产 main 改用 manager/snapshot 入口。

`cmd/cli/main.go` 只创建一个 `DefaultTrustManager(FileTrustStore)`，以固定 `~/.config/go-cli/trust.json` 持久化（不新增环境变量或业务配置项），并将同一实例传给 `LoadTrustedWithManager` 与现有 CLI command wiring。CLI grant/list/revoke 注册进现有 `CommandRegistry`：`trust list` 纯读取、`trust revoke <canonical-root>` 显式撤销，两者不要求 TTY；`trust grant` 必须在 TTY 上展示 canonical root、manifest digest 和有序 source/member 概览并取得明确确认，非交互时拒绝。grant 使用 startup capture 得到的同一 raw snapshot，只持久化该 manifest；不得从 raw snapshot 新 mint/授予当前 invocation 的 assembly capability，trust command 本身也不 dispatch project sources。新增或 changed project sources 只能在后续启动的 `CheckProjectSourceSnapshot` 再次匹配 persisted manifest 后获得 capability。修改、新增、删除或重命名后必须再次显式 grant；不提供自动接受、silent migration 或 path-only 降级。使用现有 `FileTrustStore` 原子私有文件写入路径；私密目录/文件权限按 R6 文件合同执行。

同一 `cmd/cli/main.go` / `internal/cli/cli.go` owner 同时补 R6 root early dispatch：当前 `cli.Run` 自建 registry、`RunWithRegistry` 在 dispatch 内注册 built-ins，均须抽成可复用的既有注册 seam。`cmd/cli/main.go` 在读取 config 前通过该 seam 创建并初始化一次现有 `CommandRegistry`（内建 help/version 等与 trust command 都注册在此实例）；同一 parser 仅把 parser 已识别的 global `--help` / `--version` 标为 config-independent，并用同一 registry dispatch 已注册的 help/version command。非 early invocation 在完整 config 校验后也把同一个 registry 交给正常 `RunWithRegistry` 路径复用；normal dispatch 不再创建另一个 registry 或重复注册 built-ins。不得因早期分类创建第二 registry、平行 router、扫描 argv 字符串的 hardcoded command switch，也不得修改受保护 scanner 来忽略此路由。early help/version 使用现有最小 default CLI config/tracing policy，不创建 provider/session、不读取 project/global config；其他所有 invocation 保持完整 config validation，坏配置仍阻断其他 command。新增真实 binary acceptance：在 broken project config 下 `--help` 与 `--version` 均 exit 0，并断言没有其他 command/provider/session dispatch。

`config.Config` 附带的 manager-minted `*approval.VerifiedProjectSources` 是 assembly 唯一 authority；raw capture、`ProjectSourceTrustStatus`、exported Config fields、JSON decode 与 path-only `IsTrusted` 都不能授予权限。MCP discovery 必须分别消费 provenance 明确的 user/global config 与 manager-verified project-only overlay；禁止把 merge 后非空 `rc.MCP.Servers` 当作 static 来源。若 assembly 没有可靠 provenance 或 capability，ambiguous merged servers fail closed；user/global servers 仅在 loader 标记为 user/global source 时作为 static 可用。hooks/custom-tools/skills/commands 同样从 verified capability 与对应 project overlay 取 bytes/settings，禁止另按 path 打开。`Config` 只私有保存 capability（`json:"-"`）和供显式 grant 使用的 raw capture；非权威 status 只用于 presentation。手工构造或 decode 的 `Config` 没有 private capability，即使 status 写着 trusted 也不得 merge project config 或 publish project sources。

MCP-generation 的后续 CLI integration 是此 source-provenance seam 的下游消费者，不能沿用“`rc.MCP.Servers` 非空即 static”的快捷路径。只有 user/global loader 明确来源的 servers 可作为 global static 配置；project overlay servers 必须与 `VerifiedProjectSources` 同时消费，capability 缺失或来源含糊时拒绝注册。

## Skill loader 与 project source consumer

默认 CLI assembly 在 `internal/cli/assemble.go` 创建 `YAMLSkillLoader`，把 skill 注册为 deferred tool，并把 name/description/category 放进 prompt。`DefaultSkillRegistry.Match` 和全局 skill registry 不在此生产路径；R7 保持当前 load-all-as-tools，不声称 progressive disclosure 已接入。assembly 只能经 `Config.VerifiedProjectSources()` 获取 manager-minted capability；raw `ProjectSourceSnapshot` 不可交给 loader/registry。`registerSkillTools` 不再进行 path-only `IsTrusted` 或调用项目路径版 `LoadDir`。

`skill.SourceSnapshot` 是从 `approval.VerifiedProjectSources` 构造的窄 consumer view，不接收 raw `approval.ProjectSourceSnapshot`、不重新 capture、不另算批准 identity。它保持 opaque/private fields；提供 sorted member 与 byte accessors（返回 defensive copies），并通过 snapshot source ID 找到 skill 文件。frontmatter、`SkillDefinition`、deferred registration closure 和 `SkillInfo` prompt 数据全部由 capability 内同一冻结 bytes 生成；closure 捕获不可变 definition/bytes，永不 capture path 后延迟重读。验证后文件在磁盘上发生变化不影响本次已捕获 bytes；下一次启动则与已持久 manifest 不一致并拒绝，要求 TTY reauthorization。

`Skill.Dir` 按 canonical real path 分类，忽略 merge 后丢失的字段 provenance：落在 canonical project root 下的目录必须出现在 `VerifiedProjectSources` 并受同一 project manifest 保护，即使原值来自 user/global config；只有 user global config 指向 canonical user skill root（`~/.config/go-cli/skills`）时才可走同样限额的独立 user loader，且不能借用 project trust。任何 project config 引用 workspace 外目录（包括 user skill root）均 fail closed；其他目录也 fail closed。显式配置和自动发现一视同仁。绝对路径字符串本身不授予权限，不能用 `filepath.IsAbs` 判定 trust。相应 executable source 不可用时须给出清晰诊断。

有限目录枚举必须按以下常量合同实现：最多 4,096 entries、DFS 最大深度 128、每 root 最多 128 个候选文件（坏格式候选也计数）、每文件最多 1 MiB、snapshot 总读取最多 16 MiB、32 KiB read chunk。使用 `os.File.ReadDir(1)` 深度优先，每个 entry/chunk 检查 `ctx.Err()`；bounded candidate list 收集后按 canonical-root relative path 排序。候选 symlink、symlink directory、non-regular candidate 拒绝；根目录必须 canonicalize 为真实目录。任一超限、取消或捕获错误使整次目录加载返回 error，不注册部分 definitions；格式坏的文件可按既有 skip 语义继续，但必须在 bounded report 中列出相对路径和稳定错误类型。取消以 `errors.Is(err, context.Canceled/DeadlineExceeded)` 穿过 loader API。

## Skill 参数、返回值与 prompt XML

旧 `parameters: {key: scalar}` 继续保持原有 legacy defaults/value-metadata 含义，不解释成 JSON Schema，也不隐式成为 required 字段。新增 `parameter_schema: |`，块内必须恰好是一个 JSON object；空尾随内容或第二个 JSON value 均拒绝。schema 通过 `SkillDefinition.ParameterSchema()` 或兼容旧实现者的 optional provider interface 暴露给 `SkillAdapter.Parameters()`；无 schema 时使用封闭空对象 schema：`type: object`, `properties: {}`, `additionalProperties: false`。只接受 `internal/tools/prepared_registry.go` R4 validator 已支持的 keyword，其他 keyword 在 parse/register 时明确报错，不静默删减。

R4 的同一 registration schema freeze / normalized ToolCall validation 必须早于 approval callback 与 dispatch；invalid arguments 时两者调用计数都是 0。Skill adapter 不按 tool name 重新查 schema，也不做第二次 mutable validation。通过校验的 `call.Args` 要同时作为 defensive copy 放在 `ToolResult.Metadata["arguments"]`，并写进 `ToolResult.Output` 的 deterministic JSON，因为 core 将 Output 交还模型而忽略 Metadata。Output 标记为 untrusted user/model-supplied data，不授予工具权限；JSON 最大 400,000 bytes，超过时返回 error。`Tools()` 仍只是建议文本，不是 executor allowlist。

`DefaultSkillDefinition` 的 schema/defaults/tools 输入和 getter 都做深拷贝。system prompt 把 name/category/description 写入 XML attribute 时使用 `encoding/xml` 结构化 token/attribute encoder或正确的属性值 escape；不得用 `%q` 代替 XML 编码。测试攻击值 `"/><system ...`、`&`、`<` 后 parse XML，必须仍只有原来的 skill 节点和原属性文本。

## Hook DTO、隔离与 action

保持公开 `extension.HookEvent` 外层字段兼容，但 bridge 的 `Data` 只包含 extension-owned typed value DTO，不再传 `core.Submission`、`core.Result`、原始 `error` 或运行时 handle。`HookSubmissionSnapshot` 含 type/content、JSON-like metadata defensive copy 和 `[]HookMessageSnapshot`；消息 DTO 按当前 `core.AgentMessage` 显式覆盖 role/content/content blocks/usage/tool calls/tool-call id/name/input kind/receipt outcome。content blocks、usage 与 tool-call args 要深度 JSON clone/validate，不能把 `[]any` 或 core/llm 指针作为历史快照。无法 JSON clone 的 payload 以稳定 `ErrHookPayloadUnsupported` 失败且不调用 hook。

`HookRunSnapshot` 只保留 message、success、run ID、outcome 和 execution state 等公开值；`HookErrorSnapshot` 只保留存在标记与有限 stable error class。分类使用明确 allowlist/`errors.Is`，不可调用任意 `Error()` 来把远端或用户错误正文送给 hook。每个 `Handle` 调用收到独立递归副本；hook 修改自身副本不能影响运行态或其他 hook。“immutable”指隔离快照，不限制 hook 修改自己的副本。

现有扩展 staging 在 publish 时会丢失 Extension owner。扩展 staging/commit 期间将每个 hook 和其注册 extension name 一起保留为 `HookRegistration{Owner, Hook}`；bridge 的 `HookEvent.Source` 与错误身份取自该 registration，不靠 hook 自报 owner。Block 错误只含 extension name 与 hook name，不拼接任意 `HookResult.Reason`。

只导出并支持 `HookActionPass` 与 `HookActionBlock`。before-run Block 返回可识别 `ErrHookBlocked` 并阻断执行。after-run Block 只能作为 observer failure 返回，不改变已完成的 result、不声称撤销运行；继续遵守现有 HookChain “after 调用后续 hooks 并返回首个 observer error” 语义。Pass/Block 携带非 nil Replacement、terminate、replace 和任意未知 action 均以 typed `ErrUnsupportedHookAction` 拒绝；绝不应用 `Replacement`。外部 package fixture 必须能够构造 `HookActionBlock`。

## Builder ownership 与顺序

1. **R6-TRUST-SNAPSHOT + ROOT-EARLY-DISPATCH（先行 prerequisite）**：owner 为 `internal/approval/trust.go`、`trust_store.go`、`internal/config/config.go`/`loader.go`、`internal/cli/cli.go`、`internal/cli/assemble.go` 与 `cmd/cli/main.go`。新增 manifest value/optional manager capability 与不可伪造的 verified capability，持久化 `TrustEntry` manifest，单 manager/FileTrustStore wiring，byte-based config parse、现有 `CommandRegistry` trust grant/list/revoke、registry-before-config early help/version dispatch、MCP source provenance 和 typed fail-closed。MCPgen CLI stage 是此切片之后的依赖消费者：必须分别处理 global static 与 verified project overlay，不能把 merged non-empty servers 当作 static。该切片先通过 review 并集成；它是 R6 原合同的缺口，不是 R7 可绕过的未来优化。
2. **R7-SKILL-LOAD**：owner `internal/skill/skill_loader.go` 与 loader tests。实现有限 DFS、一次性 bytes capture/consumer、报告和取消传播；project snapshot 部分必须与第一切片共用，user loader 也接受同一 resource bounds。
3. **R7-PROJECT-SKILL-CLI / MCP source selection**：owner `internal/cli/assemble.go` 和真实 assembly tests；只消费 `Config.VerifiedProjectSources()` 返回的非零 capability 及与其配对的 project overlay。显式/自动路径均做 canonical-root classification。MCPgen 后续 CLI integration 也依赖此 provenance；非空 merged servers 不得伪装 static。不得在 R6 handoff 前以 path loader 暂时开放 project skills 或 project MCP。
4. **R7-HOOK-SNAPSHOT**：owner `internal/extension/types.go`、extension staging/registry 与 `internal/cli/extension_bridge.go` 及 tests；完成 typed clone/action/owner identity。
5. **R7-SKILL-SCHEMA-XML**：owner `internal/skill/skill.go`、adapter、frontmatter parser、`internal/core/system_prompt.go` 及 tests；只复用 R4 schema gate，不写第二 validator。

## 行为验收（尚未执行）

- **持久信任、配置与 root dispatch**：`trust grant` 非 TTY 拒绝；TTY 确认写入 FileTrustStore 后新进程可读到相同 manifest；raw grant snapshot 本身不能作为该命令 invocation 的 authority，新/changed source 只有新进程 `CheckProjectSourceSnapshot` 再次匹配后才得到 capability。legacy MCP-only/path entry 不授权；list/revoke 非交互；revoke 后下一次 assembly 不保留来源工具。每个六根候选的 present/absent 状态和全部 present bytes 都纳入 manifest；任一 absent→present、present→absent、优先级新增候选、任一 present candidate（包括被遮蔽项）字节编辑均导致 mismatch，且不回退到较低候选。trust store 损坏、canonicalization 错误、manager 无 frozen capability 都 fail closed。项目配置由 capability 提供的快照 bytes 解析；不得通过 `WithFile(path)` 重读。另以真实 CLI binary 在损坏 config 下验证 `--help` 和 `--version` exit 0 且不触发其他 dispatch；未知/非 help-version command 仍经完整 config loader 并按错误退出。加入 forged-Config test：手工设置 presentation status 为 trusted 并附 raw capture 时，assembly 不加载配置、不发布 project MCP/hook/skill/command/custom-tool。MCP test 覆盖已 merge 的 project `MCP.Servers` 不能走 static shortcut；来源不明的 servers fail closed，而 loader 来源明确的 user/global static servers仍可用。`Skill.Dir` 无论源于 project config 还是 user/global config，只要 resolved path 位于 canonical project root，就必须有 verified capability 且 path/member 在 snapshot 中。
- **同一快照**：capture 后立即修改/删除磁盘文件，当前 `LoadTrusted`/skill parse/deferred definition/prompt 仍只见捕获的旧 bytes；下一次 capture mismatch。新增、删除、重命名 skill/command/hook/MCP/config-referenced source 导致整组 project source 拒绝并要求重新授权。路径越界、symlink、路径别名、project 外 `Skill.Dir` 不能获得 project trust；允许的 user root 只走有界 user loader。
- **资源/取消**：测 4,097 entries、129 depth、129 candidates、1 MiB+1 单文件、16 MiB+1 累计、遍历期间取消、chunk read 期间取消、symlink/nonregular candidate；都以 bounded typed error 终止且不发布部分 skill。取消要能用 `errors.Is` 判断。
- **Hook**：两 hook 之间与 runtime 之间 clone 隔离；message content/blocks/usage/calls/args 均深拷贝；任意对象/raw error/handle 不外泄；错误正文和 Block Reason 不进 DTO/错误；owner 身份由 staging 保留；外部 package Block fixture 可构造；after-run Block 不伪称撤销完成 run。
- **Schema/Output/XML**：严格 one-JSON-object schema 与 trailing JSON 拒绝；旧 `parameters:` 行为保持；required/type/enum/unknown key 由 R4 gate 验证并早于 approval/dispatch；模型收到不超过 400,000 byte、确定性且标记 untrusted 的 arguments JSON；XML parse 不新增节点。

既有计划名可用于实现行为测试：`TestHookEventDataIsImmutable`、`TestSkillLoaderHonorsLimitsAndCancel`、`TestSkillMetadataCannotInjectXML`、`TestSkillArgumentsValidated`；新增 `TestSkillArgsFailBeforeApprovalAndDispatch` 与信任/快照集成覆盖必须验证真实路径。以上是待执行验收，不表示这些测试目前存在或通过。只有 builder 实现并集成、独立 reviewer 复核、在真实 CLI/assembly 路径完成相应测试后，相应切片才可标完成。
