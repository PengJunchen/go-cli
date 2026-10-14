# R7 Skill 加载汇总报告设计

状态：Designer 提案，待独立 Reviewer 验收；不是实现、运行结果或 R7 阶段完成证明。实现基线固定为 `develop` `c02a1604cbc61e545028e9ead068ce4db262c22d`。本提案承接 [R7 lifecycle design](../2026-09-25-runtime/r7-lifecycle-design.md)、[R7 Skill schema handoff](r7-skill-schema-handoff.md) 与 [R7 acceptance audit](r7-acceptance-audit.md)，只处理 Skill loader 对可跳过文件错误的可观察汇总，不改 parser 接受语法、项目源信任、R6 authority、受保护台账或远端状态。

## 当前行为与问题

在基线上，[`SkillLoader.LoadDir` 和可选的 `SkillSourceLoader.LoadSources`](../../../internal/skill/skill_loader.go) 都返回 `[]*SkillDefinition, error`；后者消费调用方传入的 captured `SkillSourceFile`。两种批量路径都跳过非致命的单文件失败并继续加载：`LoadDir` 记录 `skill.load_dir.skipped`，`LoadSources` 记录 `skill.load_sources.skipped`；调用方只拿到成功定义，没有结构化的跳过数量与错误类别。[`registerSkillTools`](../../../internal/cli/assemble.go) 最后只在有工具成功注册时记 `assemble_skills_registered`，因此混合成功与坏文件时难以看出加载面是否不完整。

[`isFatalSkillLoadError`](../../../internal/skill/skill_loader.go) 将取消、deadline、资源上限和不安全源列为批量加载失败；这些错误与可跳过的解析错误有不同控制流，不能降格为 skip。当前前置/资源/信任行为继续由 loader 与 R6 负责。本切片提供一次加载调用的 aggregate，供 Assembly 对坏文件跳过给出可读警告。

## 决策

保留 `SkillLoader` 和 `SkillSourceLoader` 的现有方法集及返回值；另外定义两个可选、无状态、按调用返回结果的 capability：`SkillLoadReporter` 为目录加载提供 `LoadDirWithReport`，`SkillSourceLoadReporter` 为 captured bytes 提供 `LoadSourcesWithReport`。内建 `YAMLSkillLoader` 实现两个 capability。现有 `LoadDir`、`LoadSources` 保持原行为和签名，转调同一加载实现并丢弃新报告字段。不得用 loader 上的 `LastReport` 或可变共享状态，因为同一 loader 可以并发调用。

建议的窄可选接口与数据类型如下，名称可按现有代码风格微调：

```go
type SkillLoadReporter interface {
    LoadDirWithReport(ctx context.Context, dirPath string) (SkillLoadResult, error)
}

type SkillSourceLoadReporter interface {
    LoadSourcesWithReport(ctx context.Context, sources []SkillSourceFile) (SkillLoadResult, error)
}

type SkillLoadResult struct {
    Definitions []*SkillDefinition
    Report      SkillLoadReport
}

type SkillLoadReport struct {
    Summary     SkillLoadSummary
    Diagnostics []SkillLoadDiagnostic
}

type SkillLoadSummary struct {
    Loaded      int
    Skipped     int
    ErrorCounts []SkillLoadErrorCount
}

type SkillLoadErrorClass string

type SkillLoadErrorCount struct {
    Class SkillLoadErrorClass
    Count int
}

type SkillLoadDiagnostic struct {
    SourceID string
    Class    SkillLoadErrorClass
}

const (
    SkillLoadErrorClassFrontmatter     SkillLoadErrorClass = "frontmatter"
    SkillLoadErrorClassParameterSchema SkillLoadErrorClass = "parameter_schema"
    SkillLoadErrorClassOther           SkillLoadErrorClass = "other"
)
```

在正常完成的结果中，`Summary.Loaded == len(Definitions)`；它表示成功解析出的定义数量，不代表 deferred tool 已成功注册。`Summary.Skipped` 表示批量方法按现有规则跳过的候选数，且等于 `Summary.ErrorCounts` 各项的和。`ErrorCounts` 使用固定类别并按类别标识的字典序排序；无类别时为空切片，不输出随机 map 顺序。聚合 `Summary` 只含计数/类别、不含路径；`Diagnostics` 是另一条有界逐文件信息，用来满足下文已接受的相对路径 CLI 合同。报告和新事件不包含底层 error 文本、Skill metadata、prompt/body、参数、密钥或凭据；调用方不能为打印汇总而序列化整个 `SkillLoadResult`，其 `Definitions` 是已有加载返回值。

类别使用固定、可断言的标识：

| 类别 | 分类条件 |
| --- | --- |
| `frontmatter` | `errors.Is(err, errParse)`，例如缺少 delimiter 或字段块不能形成既有 Skill 定义 |
| `parameter_schema` | `errors.Is(err, ErrSkillParameterSchemaInvalid)`，包括 R4 schema validator 链错误 |
| `other` | 当前 skip-with-warning 路径中其余非致命的单文件错误 |

该分类只描述批量 loader 已决定跳过的项目。任何满足 `isFatalSkillLoadError` 的错误继续作为整个操作的 error 返回，不增加 `Skipped`，不生成一个看似成功的部分报告；原有 all-or-nothing 定义返回行为保持。未来若增加错误类别，须更新分类函数、排序与测试，不把任意 error 文本直接变成公开 class。

## Assembly 可见性与兼容

基线 Assembly 在 [`registerSkillTools`](../../../internal/cli/assemble.go#L2332) 与 [`loadVerifiedProjectSkills`](../../../internal/cli/assemble.go#L2525) 中分别创建 `YAMLSkillLoader`，不存在 custom loader 的 Assembly 选择/注入点。实现沿用这两个既有内建构造点：用户 Skill 目录路径用 `registerSkillTools` 创建的 loader 消费 `SkillLoadReporter`；项目 Skill 路径则由 `loadVerifiedProjectSkills` 创建 loader，在完成 manifest membership/bytes 校验后消费 `SkillSourceLoadReporter`。不得新增 loader registry、配置选择、DI seam 或不可达 fallback policy。旧 `SkillLoader` 与 `SkillSourceLoader` 方法集不增加必需方法；第三方/测试 legacy loader 的兼容性是 source/API-level：它仍通过旧方法工作，无需实现 report capability。直接测试旧接口编译/调用和 builtin 旧方法包装，不虚构 Assembly 中当前不存在的 legacy-loader 运行路径。

把 Assembly 汇总与逐文件提示接入现有 CLI `slog` logger，不新增 global diagnostics framework、stdout 格式、flag 或用户交互路由。报告 `Summary.Skipped > 0` 时发出一个 path-free 汇总 `WARN`，例如 `assemble_skills_load_report`，含解析 `loaded` 数、`skipped` 数及稳定次序编码的 `error_classes`（如 `frontmatter=2,parameter_schema=1`）；另外对 `Diagnostics` 中每项发一个有界的 `assemble_skill_load_skipped` `WARN`，只包含安全 `source_id` 与固定 `error_class`。报告 `Summary` 不带路径；逐文件 diagnostics 满足已接受的相对源定位要求。不得把 parser `Loaded` 与 deferred tool 注册成功数混为一谈；现有 `assemble_skills_registered` 继续表示注册成功数。没有 skip 时不增加新的常规 summary 日志；取消/资源/不安全源仍走现有整体加载错误路径。

每个 skip 恰产生一个 diagnostic，最多 128 项（与当前每个 root/source set 的候选上限相同），每个 `source_id`（含 `skill:` 前缀）最多 256 个编码后 ASCII 字节。目录路径身份取自 [`walkSkillDirectory` callback](../../../internal/skill/skill_loader.go#L268) 提供的 root-relative `relative`（递归遍历按名称排序，实际 `visit` 收到的正是此值）；绝不取同回调的绝对 `path`。Project `LoadSources` 诊断取 [`validateSkillSourcePath`](../../../internal/skill/skill_loader.go#L650) 校验后的 `SkillSourceFile.RelativePath`；这是 [`loadVerifiedProjectSkills`](../../../internal/cli/assemble.go#L2472) 在匹配 R6 `DirectoryMembers`/`ProjectSourceRecord.RelativePath` 后生成的、相对本次 loader skill-directory root 的逻辑 source ID，而 bytes 来自 `SourceBytes(record.ID)`。换言之 label 与 captured byte 的来源锚点不同且不可互换：仅用于显示的相对 ID 不承担读取或信任判断。

路径转换应在输出前再次拒绝 absolute、卷名、空/`.`/`..` 与越根身份，再转为 slash 形式，使用 `strconv.QuoteToASCII` 转义控制符。短标签完整显示相对身份；超长编码标签保留稳定头尾片段并附完整 64 字符小写 SHA-256 摘要，在 256 字节总上限内减少截断导致的 label 碰撞。Diagnostics 按规范化/编码后的 `source_id` 排序；summary classes 也排序。Captured-source 标签只从传入相对身份生成，不按该路径读盘、不使用 workspace canonical root、不加入原始 parser error、Skill bytes 或环境 credential 值。

报告、新汇总事件和新逐文件事件分别只携带规定字段：汇总只有 counts/classes；逐文件只有 bounded relative/logical source id 与 class。两类新事件都不携带底层 error 文本、prompt/body、schema/argument、凭据或 `SkillDefinition`；测试分别检查 aggregate 与 per-file event。这个边界不声称全局 path-free：基线既有 `skill.load` span 的 `source_path` 和逐文件内部日志保持原样，不删除其 tracing spans，也不改其既有合同。

## R6 与 parser 边界

`LoadSourcesWithReport` 只读取收到的 `SkillSourceFile.Content`，并沿用传入的相对路径顺序；它不得按 `RelativePath` 重开文件、重建 manifest、复制/弱化 trust decision，或从低优先级来源回退。Assembly 仍先要求当前有效的 R6 `VerifiedProjectSources`，验证 captured membership，再从每条 record 取得同一次冻结的 `SourceBytes`。撤销、缺失、身份或 membership 不匹配必须在 report 方法之前按现有 R6 路径拒绝；报告只归纳已获准输入集里的解析结果，不形成新的信任能力。

一般 frontmatter 继续使用 c02a160 已接受的宽容子集行为。未知或当前忽略的语法不能因为增加 report 而突然拒绝；非 `parameter_schema` YAML 不切换成完整 YAML 解析器、不新增 strict lint、不改变 legacy `parameters` 意义。`parameter_schema` 的 JSON-object 子合同和 R4 validator 保持现有边界；其具体格式错误可被归入 `parameter_schema` 类，但 schema 支持集不扩张。

## TDD 验收合同

先加红测，再实现。建议至少包含以下可执行覆盖：

1. `TestSkillLoadReportMixedDirectoryIsStable`：在真实临时目录放入有效 Skill、缺失/损坏 frontmatter、损坏 `parameter_schema` 和含既有容忍未知字段的 Skill；断言 definitions、summary 总数/分类计数，以及每个 skip 一条按 `source_id` 排序的 relative diagnostic。重复执行得到相同 report；未知但原本容忍的字段仍正常加载。
2. `TestSkillLoadReportMixedCapturedSourcesUsesSnapshotBytes`：用现有 R6 验证 fixture 捕获有效及格式错误的 Skill bytes，之后更改/删除工作树文件，再调用 report 路径；summary 与 diagnostics 必须对应传入的精确 captured bytes/source ID。通过真实 `VerifiedProjectSources`/Assembly 路径证明 report 只发生在现有 capture/membership 检查之后；撤销或下一次 membership 不匹配仍按 R6 原合同 fail closed，报告不重开 source path。
3. `TestSkillLoadReporterPreservesLegacyMethodSets`：自定义旧 loader 只实现现有 `SkillLoader` 或 `SkillSourceLoader` 方法，不实现 reporting capability，仍通过原接口直接编译/调用；不为当前 Assembly 添加 fallback 或 loader injection 测试。内建 loader 的旧方法与对应 `WithReport` 方法给出相同 definitions/error，source/API method set 未扩张。
4. `TestSkillLoadReportDoesNotFoldCancellationOrFatalErrorsIntoSkips`：覆盖预取消 context、遍历中取消、资源上限和 unsafe source。断言原 context/resource/unsafe error 可通过 `errors.Is` 识别，返回 nil definitions 与零值 `SkillLoadResult.Report`（计数为 0，`ErrorCounts`/`Diagnostics` 为空），不计为 malformed skip，Assembly 不发新的 skip summary/diagnostic。混合项中先遇到 malformed、后遇到 fatal 的目录仍按现有 all-or-nothing 语义失败。
5. `TestSkillLoadReportSummaryAndAggregateEventArePathFree`：检查 `Summary` 序列化及 `assemble_skills_load_report` 事件只含固定类别、数量，不含相异的 prompt/body、绝对根路径、relative path、credential canary 或原始 error canary。断言零 skip 不新增 summary，成功注册数量仍单独记录。
6. `TestSkillLoadReportDiagnosticsAreBoundedAndContentFree`：从目录及 captured-source paths 覆盖 newline/control 字符与超长 relative label；diagnostic 每项只有编码/有界 `source_id` 和固定 class，不含绝对 root、control 字符、raw error、body 或 credential canary；总项数不超过 128、每个 label 不超过 256 ASCII bytes、按 source ID 稳定排序，长标签包含完整 64-hex path digest 与首尾片段。CLI per-file event 只使用这两个字段。

`go test ./internal/skill ./internal/cli` 可验证该定向切片；任何实现后是否需要更广 race/full-suite gate，由 parent 按集成状态决定。本交付仅是可审核的设计输入，不是这些测试或实现已经存在的声明。

## Builder 文件边界与明确不做

预计只需 `internal/skill/skill_loader.go` 增加报告类型/可选接口与共享计数路径，`internal/cli/assemble.go` 消费可选 capability 并输出聚合及有界逐文件 warning，再加 `internal/skill` 与 `internal/cli` 的针对性测试。不要编辑 `docs/iterations/**/execution-tasks.json`、rules/、Makefile、CI、R6 trust/snapshot 源码、R4 prepared gate、schema handoff 或非 Skill R7 owner 文件；若 Assembly 与并行任务冲突，等待文件 owner 的串行集成安排。

本报告不升级 parser、不改变 skip/fatal 策略、不提供工具授权或 readiness 状态、不重构 YAML parser、不改 R6。历史 lifecycle 设计中的逐文件相对路径诊断由独立、有界的 `Diagnostics`/CLI warning 满足，聚合 summary 仍保持 path-free；不能把路径塞进汇总 counts/classes event。

通过上述验收只证明加载汇总的窄合同；不能据此宣称 R7 Skill、R7-DES-01 或原始 47 项验收关闭。该文档须经独立 Reviewer 对齐已接受的 Skill schema、R6 frozen-source 和 CLI privacy 边界后，才交给 Builder。
