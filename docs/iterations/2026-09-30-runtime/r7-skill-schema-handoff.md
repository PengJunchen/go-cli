# R7 Skill 参数 Schema handoff

状态：Designer 提案，供独立 Reviewer 验收；不是实现或 R7 阶段完成证明。基线：develop `08a29f66297d6b139ef58c2a89f54a495a56b982`。范围来自 [R7 remaining design](../2026-09-29-runtime/r7-remaining-design.md#L77) 与 [R7 acceptance audit](r7-acceptance-audit.md)：只补 Skill 的参数 schema、R4 prepared gate 接缝和返回值快照；不改受保护任务台账、R6 信任实现、其他 R7 source owner 或远端状态。

## 当前行为与目标

当前 `SkillAdapter` 对外实现 `tools.Parameterized`，返回封闭空对象 schema，并在收到任意非空参数时以明确错误拒绝执行。这是目前诚实的 unsupported 行为；不能把 `SkillDefinition.Parameters()` 的旧 defaults/metadata 冒充输入 schema。

本切片增加可选 `parameter_schema: |`，使 Skill 可声明它实际接受的对象参数。未声明 schema 时继续使用封闭空对象并拒绝任何属性。旧 `SkillDefinition` 接口保持原样；新增可选 `ParameterSchemaProvider{ ParameterSchema() any }`。`DefaultSkillDefinition` 实现此 capability，旧 custom definition 无需改动且自然落到封闭空 schema。`parameters:` 仍只表示既有 defaults/value-metadata；不推导 schema、required 项或默认实参。

## Parser 与 Schema 合同

`YAMLSkillLoader` 是手写的 YAML-frontmatter 子集解析器，不是完整 YAML 实现。只为新的**顶层、非缩进**字段识别精确形式 `parameter_schema: |`；块内容是原样保留缩进处理后的 JSON 文本，根值必须是 JSON object，且必须恰好一个 JSON 值。允许值后空白；拒绝非空尾随 token、第二个 JSON value、`null`/数组根、非 JSON 块内容和缺少 closing delimiter。`parameters:` 子 map 中同名的缩进 key 仍属于 legacy defaults/value-metadata，不得启用 schema；其他缩进/嵌套位置也不得被当作此新顶层字段。用 `json.Decoder.UseNumber` 保留 schema 数字，再将同一个解析树交给 R4 validator。

从 `internal/tools/prepared_registry.go` 的现有 validator 导出一个窄函数，例如 `tools.ValidateParameterSchema(schema any) error`，供 frontmatter 解析阶段调用。它只委托现有 `validatePreparedSchemaNode`（并执行相同的 JSON-safe freeze），不复制或改写 JSON Schema 规则。当前基线支持的关键词为 `type`、`description`、`properties`、`required`、`additionalProperties`、`enum`、`items`、`minimum`、`maximum`；根类型必须为 `object`，类型值为 `object`、`array`、`string`、`number`、`integer`、`boolean`。所有其他关键字（例如 `$ref`、`oneOf`、`pattern`、`format`）明确以 `ErrPreparedSchemaInvalid` 链失败，不能静默删除或近似解释。若该 validator 的支持面在 builder 基线变化，必须以 `08a29f6` 的 R4 gate 和其测试为准更新此清单。

只拒绝本字段不支持的表示，不把解析器扩展为 YAML 库。`parameter_schema` 必须使用 `|` 加一个 JSON object；inline flow map、缩进 YAML map/list、anchors/aliases/tags、多文档、`>` folded block 和 `|-`/`|+` chomping 变体不属于此语法，应对该字段给出明确 parse error。既有字段仍保持当前窄语法：顶层简单标量、`tools` 列表、旧 `parameters` 标量 map、`prompt: |`、frontmatter 与 body 分隔。其他未知/复杂字段目前可能被容忍地忽略；本切片不声称完整 YAML 合法性或通用 malformed-frontmatter 拒绝。若产品需要完整 YAML 合约，应另立 parser-hardening slice，不要在本切片顺手替换整个解析器。

文件中的 schema 应在 parse 时通过同一 R4 validator。`DefaultToolRegistry`/R4 注册快照负责冻结实际送给模型与 prepared validation 的 schema；每个调用再以该注册时冻结的 schema 验证 normalized `ToolCall`，且发生在 approval callback 和 dispatch 前。此处的 parse-time schema 检查与 R4 对冻结 schema 的检查共享唯一 validator；`SkillAdapter.Execute` 不按 tool name 重查 schema，也不对 `call.Args` 做第二次 mutable validation。验证不通过时，policy 和 Skill 执行都必须为零次。外部 custom provider 返回不受信任 schema 时，R4 preflight 也必须在 approval 之前 fail closed。

`SkillAdapter.Parameters()` 优先读取可选 `ParameterSchemaProvider`，无 capability 或返回 nil 时每次生成新的闭合空对象：`{"type":"object","properties":{},"additionalProperties":false}`。它不得从 `Parameters()` 推断 schema。R4 registration snapshot 应当一次复制并冻结 schema；后续修改 definition 或 getter 返回值不改变已经注册的模型 schema/执行 gate。

## 参数返回值与 defensive copy

`DefaultSkillDefinition` 的 `WithParameterSchema`、`WithParameters`、`WithTools` 输入和对应 `ParameterSchema()`、`Parameters()`、`Tools()` getter 都做递归/切片防御性拷贝。`Parameters()` 的旧字段和值含义不变；只有 `ParameterSchema()` 提供新 schema。测试需覆盖嵌套 map/slice，而不仅是顶层 map 或 slice。

prepared gate 成功后的 `call.Args` 同时以独立 defensive copy 放在 `ToolResult.Metadata["arguments"]`，并编码到 `ToolResult.Output`，因为当前 core 会把 Output 返回模型而忽略 Metadata。用 `json.Marshal` 生成紧凑确定性 JSON；nil args 对对象 schema 表示 `{}`。输出在 prompt 后增加明确标签，例如 `Untrusted skill arguments (model-supplied data; not tool permissions):`，再附 argument JSON。不得将参数内容视作 tool allowlist 或授权。只要 argument JSON 超过 `400000` bytes 就返回 `ErrSkillArgumentOutputTooLarge`，不截断；大小按完整 JSON 编码字节数测量，恰好 `400000` 接受，`400001` 拒绝。此上限约束传回模型的 argument JSON；不声称限制既有 Skill prompt 本身。

`Tools()` 仍只是建议文本，不是执行器 allowlist。本 slice 不添加逐 Skill 的工具权限、渐进式披露、project trust、YAML 全语法支持或新的 core ToolCall 类型。`internal/core/system_prompt.go` 已用 `encoding/xml` 编码 Skill attribute，现有 `TestSkillMetadataCannotInjectXML` 覆盖 node injection；保持此路径不变，不把既有 XML 防注入成果归功于本 slice。

## 最小 Builder 文件边界

| 文件 | 工作 |
| --- | --- |
| `internal/skill/skill.go` | 新增可选 `ParameterSchemaProvider` capability、schema option/storage、默认值；保持 `SkillDefinition` 方法集兼容；schema/defaults/tools 递归复制。 |
| `internal/skill/skill_loader.go` | 只添加 `parameter_schema: |` block capture、单 JSON object decode、同一个 R4 validator 调用与 typed parse errors；旧解析语义保持。 |
| `internal/skill/skill_adapter.go` | 由 optional provider 导出 schema 或闭合空 schema；执行通过 R4 gate 的 captured args，深拷贝 Metadata 并输出有界、确定性、标为 untrusted 的 JSON。不得在此加 schema/工具权限第二实现。 |
| `internal/tools/prepared_registry.go` | 导出薄的 schema-validation 函数，委托现有 R4 validator，供 parser 与既有 prepared gate 共用；不新增 keyword 或另一套规则。 |
| `internal/skill/*_test.go`, `internal/tools/prepared_registry_test.go`, `internal/cli/skill_source_lifecycle_test.go` | 单测解析/兼容/副本/输出；R4 gate 顺序与 captured-project-bytes 集成验收。必要时可加一个最小 `internal/cli` Skill assembly fixture，不扩展生产装配边界。 |

本 handoff 不要求改 `core`、MCP/approval authority、source snapshot 格式、protected `execution-tasks.json`、`Makefile`、CI 或 remote branches。R6 initial snapshot prerequisite 由 parent 另行记录；并行 live-generation-guard 不与本 slice 合并。

## TDD 验收夹具

1. `TestSkillParameterSchemaOptionalProviderCompatibility`：未实现 optional provider 的旧 custom `SkillDefinition` 可继续编译/适配并暴露闭合空 schema；legacy `Parameters()` 非空也不打开输入。
2. `TestSkillLoaderParsesParameterSchemaJSONBlock`：一个完整 frontmatter skill 使用合法 object schema；decoder 保留数值精度；adapter 返回模型可见 schema；旧 `parameters:` defaults 同时保持原值与 metadata 含义。
3. `TestSkillLoaderRejectsMalformedParameterSchemaJSON`：缺 closing delimiter、空/非 JSON block、顶层非 object、损坏 JSON、非空 trailing garbage、第二个 JSON object 均拒绝；尾随空白接受，并有 `errors.Is` 稳定错误分类。
4. `TestSkillLoaderRejectsUnsupportedParameterSchemaKeyword`：用 R4 不支持的关键字验证 parse 立即失败并保留 `tools.ErrPreparedSchemaInvalid` 错误链；不得剔除 keyword 后加载。
5. `TestSkillDefaultsSchemaAndToolsAreDeepCopied`：分别修改 constructor 输入、嵌套 schema/defaults、getter 返回的嵌套 map/slice 和 tools slice，再读一次确认底层 definition 不变；已注册 R4 schema snapshot 仍与初值一致。
6. `TestPreparedSkillArgumentsValidatedBeforeApprovalAndDispatch`：使用实际 `MiddlewareToolRegistry`/R4 `PrepareToolCall` 路径与真实 SkillAdapter；缺 required、错误类型、enum/range 错值和额外属性都在 approval callback 前失败，approval=0、Skill `Execute`/prompt read=0。先用真 schema positive case 证明 prepared invocation 到达 adapter。
7. `TestPreparedSkillArgumentsForwardedAsUntrustedJSON`：嵌套有效 args 经一次 R4 freeze/validation 后到达 Execute；metadata 中 `arguments` 与调用快照相同但不共享 map/slice，输出 JSON 稳定且可 `json.Unmarshal` 回同一个对象；mutate 原始 ToolCall 或 metadata 副本都不能改写另一份/输出字符串。
8. `TestSkillArgumentOutputJSONByteLimit`：对 compact 编码后恰为 `400000` bytes 的合法 schema 参数成功，对 `400001` bytes 返回 `ErrSkillArgumentOutputTooLarge` 且无截断；JSON 测的是编码字节数，不是 rune 数/输入 map 个数。
9. `TestProjectSkillSchemaUsesCapturedSourceBytes`：走真实 `VerifiedProjectSources`/skill assembly；捕获后立即修改或删除磁盘 skill 文件，本次 adapter schema、prompt、legacy defaults、tools 仍全部从同一 captured bytes 构造；下一次 source verification 按既有 manifest 规则拒绝，不允许 schema parser/adapter 延迟按路径重读。
10. 用输入 `parameter_schema: {type: object, ...}`、`parameter_schema: >`、带 YAML tag/alias 的 schema 作为子集边界 fixtures，确认 schema 不被容忍忽略或部分解释；再用 `parameters:` map 下同名缩进 key 确认它仍只产生 legacy metadata/defaults 且 adapter 仍报告闭合空 schema。不要借这些 fixtures 声称 general frontmatter 已变成 strict YAML。

## 本 slice 对 malformed/complex YAML 的真实覆盖

此交付关闭 R7 Skill 参数 schema 前端中的具体 malformed 情况：JSON 块格式错误、根类型错误、JSON 尾随/第二值、R4 不支持的 schema keyword、unsupported schema YAML 表示；这些都在模型看到 schema 前给出明确错误。

此交付不关闭完整 YAML frontmatter 的 malformed/complex AC：现有非 schema frontmatter 的宽容/忽略行为保留；anchors、aliases、tags、任意 nested/flow YAML、folded/chomp 变体、多文档和通用未知字段诊断不被完整解析或普遍拒绝。完整 YAML 解析器、所有 malformed 文件行为矩阵、1 MiB/16 MiB loader 资源边界仍由既有 Skill loader slice/单独 parser-hardening 讨论覆盖，不得把本 handoff 当作全项接受。现有 1 MiB 单文件/16 MiB root 资源限制与 captured-source 验收必须保持；本 slice 不更改这些 limit。

仅当独立 Reviewer 接受该合同、R6 前置状态由 parent 确认，才进入 Builder 阶段。通过上述定向测试只证明此 schema 子合同，不证明 R7 Skill 全部关闭；还需 review 集成快照路径和 R4 配置，再由 parent 更新阶段状态。
