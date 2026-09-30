# R7 Skill 加载汇总报告设计

状态：Designer 提案，待独立 Reviewer 验收；不是实现、运行结果或 R7 阶段完成证明。实现基线固定为 `develop` `c02a1604cbc61e545028e9ead068ce4db262c22d`。本提案承接 [R7 lifecycle design](../2026-09-25-runtime/r7-lifecycle-design.md)、[R7 Skill schema handoff](r7-skill-schema-handoff.md) 与 [R7 acceptance audit](r7-acceptance-audit.md)，只处理 Skill loader 对可跳过文件错误的可观察汇总，不改 parser 接受语法、项目源信任、R6 authority、受保护台账或远端状态。

## 当前行为与问题

在基线上，[`SkillLoader.LoadDir` 和可选的 `SkillSourceLoader.LoadSources`](../../../internal/skill/skill_loader.go) 都返回 `[]*SkillDefinition, error`；后者消费调用方传入的 captured `SkillSourceFile`。两种批量路径都跳过非致命的单文件失败并继续加载：`LoadDir` 记录 `skill.load_dir.skipped`，`LoadSources` 记录 `skill.load_sources.skipped`；调用方只拿到成功定义，没有结构化的跳过数量与错误类别。[`registerSkillTools`](../../../internal/cli/assemble.go) 最后只在有工具成功注册时记 `assemble_skills_registered`，因此混合成功与坏文件时难以看出加载面是否不完整。

[`isFatalSkillLoadError`](../../../internal/skill/skill_loader.go) 将取消、deadline、资源上限和不安全源列为批量加载失败；这些错误与可跳过的解析错误有不同控制流，不能降格为 skip。当前前置/资源/信任行为继续由 loader 与 R6 负责。本切片提供一次加载调用的 aggregate，供 Assembly 对坏文件跳过给出可读警告。

## 决策

保留 `SkillLoader` 和 `SkillSourceLoader` 的现有方法集及返回值，另增一个可选、无状态、按调用返回结果的 `SkillLoadReporter` capability。它提供 `LoadDirWithReport` 和 `LoadSourcesWithReport` 两种批量入口；内建 `YAMLSkillLoader` 实现两者。现有 `LoadDir`、`LoadSources` 保持原行为和签名，转调同一加载实现并丢弃报告。不得用 loader 上的 `LastReport` 或可变共享状态，因为同一 loader 可以并发调用。

建议的窄可选接口与数据类型如下，名称可按现有代码风格微调：

```go
type SkillLoadReporter interface {
    LoadDirWithReport(ctx context.Context, dirPath string) (SkillLoadResult, error)
    LoadSourcesWithReport(ctx context.Context, sources []SkillSourceFile) (SkillLoadResult, error)
}

type SkillLoadResult struct {
    Definitions []*SkillDefinition
    Report      SkillLoadReport
}

type SkillLoadReport struct {
    Loaded      int
    Skipped     int
    ErrorCounts []SkillLoadErrorCount
}

type SkillLoadErrorClass string

type SkillLoadErrorCount struct {
    Class SkillLoadErrorClass
    Count int
}

const (
    SkillLoadErrorClassFrontmatter     SkillLoadErrorClass = "frontmatter"
    SkillLoadErrorClassParameterSchema SkillLoadErrorClass = "parameter_schema"
    SkillLoadErrorClassOther           SkillLoadErrorClass = "other"
)
```

在正常完成的结果中，`Loaded == len(Definitions)`；它表示成功解析出的定义数量，不代表 deferred tool 已成功注册。`Skipped` 表示批量方法按现有规则跳过的候选数，且等于 `ErrorCounts` 各项的和。`ErrorCounts` 使用固定类别并按类别标识的字典序排序；无类别时为空切片，不输出随机 map 顺序。`Report` 本身不包含文件路径、底层错误文本、Skill metadata、prompt/body、参数、密钥或凭据；调用方不能为打印汇总而序列化整个 `SkillLoadResult`，其 `Definitions` 是已有加载返回值。

类别使用固定、可断言的标识：

| 类别 | 分类条件 |
| --- | --- |
| `frontmatter` | `errors.Is(err, errParse)`，例如缺少 delimiter 或字段块不能形成既有 Skill 定义 |
| `parameter_schema` | `errors.Is(err, ErrSkillParameterSchemaInvalid)`，包括 R4 schema validator 链错误 |
| `other` | 当前 skip-with-warning 路径中其余非致命的单文件错误 |

该分类只描述批量 loader 已决定跳过的项目。任何满足 `isFatalSkillLoadError` 的错误继续作为整个操作的 error 返回，不增加 `Skipped`，不生成一个看似成功的部分报告；原有 all-or-nothing 定义返回行为保持。未来若增加错误类别，须更新分类函数、排序与测试，不把任意 error 文本直接变成公开 class。

## Assembly 可见性与兼容

`registerSkillTools` 对内建 loader 使用可选 `SkillLoadReporter`。用户 Skill 目录调用 `LoadDirWithReport`；已验证的项目 Skill 目录在完成现有 manifest membership/bytes 校验后，用同一组 `SkillSourceFile` 调用 `LoadSourcesWithReport`。不支持 report capability 的旧 loader 仍走原接口；此时“报告不可用”与计数为零不同，不构造零计数假象。`SkillLoader` 与 `SkillSourceLoader` 不增加必需方法，已有 custom loader 无需修改即可继续满足旧合同。

仅当报告 `Skipped > 0` 时，Assembly 发出一个汇总 `WARN`，例如事件 `assemble_skills_load_report`，含解析 `loaded` 数、`skipped` 数及按稳定次序编码的 `error_classes`（如 `frontmatter=2,parameter_schema=1`）。不得把这些字段与工具注册数混为一谈；现有 `assemble_skills_registered` 继续表示注册成功数。没有 skip 时不增加新的常规日志，保留当前零损失路径的日志基线；取消/资源/不安全源仍走现有整体加载错误路径，而不是汇总警告。

报告和新汇总事件必须不含 per-file path、原始 error、prompt/body、schema/argument、凭据或可序列化的 `SkillDefinition`。本约束只适用于新增的 report 与 aggregate event。基线既有 `skill.load` span 的 `source_path` 和逐文件日志属于既存行为；本提案不声称消除其他路径记录、不移除这些 tracing spans，也不改变其合同。该隐私边界应在 Reviewer 审查中明确，不能将报告的 payload-free 误述为全局 path-free。

## R6 与 parser 边界

`LoadSourcesWithReport` 只读取收到的 `SkillSourceFile.Content`，并沿用传入的相对路径顺序；它不得按 `RelativePath` 重开文件、重建 manifest、复制/弱化 trust decision，或从低优先级来源回退。Assembly 仍先要求当前有效的 R6 `VerifiedProjectSources`，验证 captured membership，再从每条 record 取得同一次冻结的 `SourceBytes`。撤销、缺失、身份或 membership 不匹配必须在 report 方法之前按现有 R6 路径拒绝；报告只归纳已获准输入集里的解析结果，不形成新的信任能力。

一般 frontmatter 继续使用 c02a160 已接受的宽容子集行为。未知或当前忽略的语法不能因为增加 report 而突然拒绝；非 `parameter_schema` YAML 不切换成完整 YAML 解析器、不新增 strict lint、不改变 legacy `parameters` 意义。`parameter_schema` 的 JSON-object 子合同和 R4 validator 保持现有边界；其具体格式错误可被归入 `parameter_schema` 类，但 schema 支持集不扩张。

## TDD 验收合同

先加红测，再实现。建议至少包含以下可执行覆盖：

1. `TestSkillLoadReportMixedDirectoryIsStable`：在真实临时目录放入有效 Skill、缺失/损坏 frontmatter、损坏 `parameter_schema` 和含既有容忍未知字段的 Skill；断言定义、总数、分类计数及排序。重复执行得到相同报告；未知但原本容忍的字段仍正常加载。
2. `TestSkillLoadReportMixedCapturedSourcesUsesSnapshotBytes`：用现有 R6 验证 fixture 捕获有效及格式错误的 Skill bytes，之后更改/删除工作树文件，再调用 report 路径；结果和错误类别必须对应传入的精确 captured bytes。通过真实 `VerifiedProjectSources`/Assembly 路径证明 report 只发生在当前有效的 capture/membership 检查之后；撤销或下一次 membership 不匹配仍按 R6 原合同 fail closed。
3. `TestSkillLoadReporterPreservesLegacyInterfaces`：未实现 report capability 的旧 `SkillLoader`，以及只实现已有 `SkillSourceLoader` 的 loader，仍可编译并通过原方法调用；Assembly fallback 不声称 report 可用，也不输出零计数汇总。内建 loader 的旧方法与 `WithReport` 方法返回同样定义/error。
4. `TestSkillLoadReportDoesNotFoldCancellationOrFatalErrorsIntoSkips`：覆盖预取消 context、遍历中取消、资源上限和 unsafe source。断言原 context/resource/unsafe error 可通过 `errors.Is` 识别，返回 nil definitions，不计为 malformed skip，Assembly 不发 `assemble_skills_load_report`。混合项中先遇到 malformed、后遇到 fatal 的目录仍按现有 all-or-nothing 语义失败。
5. `TestSkillLoadReportAggregateContainsOnlyCountsAndClasses`：使用相异的 prompt/body、文件名、绝对根路径及 credential canary；检查 report 序列化和 `assemble_skills_load_report` 事件只含固定错误类别、数量，不含这些 canary 或底层 error 文本。断言零 skip 不新增 summary，成功注册数量仍单独记录。

`go test ./internal/skill ./internal/cli` 可验证该定向切片；任何实现后是否需要更广 race/full-suite gate，由 parent 按集成状态决定。本交付仅是可审核的设计输入，不是这些测试或实现已经存在的声明。

## Builder 文件边界与明确不做

预计只需 `internal/skill/skill_loader.go` 增加报告类型/可选接口与共享计数路径，`internal/cli/assemble.go` 消费可选 capability 并输出一个聚合事件，再加 `internal/skill` 与 `internal/cli` 的针对性测试。不要编辑 `docs/iterations/**/execution-tasks.json`、rules/、Makefile、CI、R6 trust/snapshot 源码、R4 prepared gate、schema handoff 或非 Skill R7 owner 文件；若 Assembly 与并行任务冲突，等待文件 owner 的串行集成安排。

本报告不升级 parser、不改变 skip/fatal 策略、不增加每文件 API、不提供工具授权或 readiness 状态、不重构 YAML parser、不改 R6。历史 lifecycle 设计中“逐文件相对路径诊断”尚不由此 aggregate contract 满足；若该逐文件定位仍是强制验收，应在实现前由 parent 明确另批，不能暗中把路径放进本报告。

通过上述验收只证明加载汇总的窄合同；不能据此宣称 R7 Skill、R7-DES-01 或原始 47 项验收关闭。该文档须经独立 Reviewer 对齐已接受的 Skill schema、R6 frozen-source 和 CLI privacy 边界后，才交给 Builder。
