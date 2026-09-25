# R3 sampling presence implementation handoff

**状态：** R3 sampling 的窄实现交接，供现有 provider builder 与 consumer reviewer 对齐后执行。只处理 temperature/max_tokens 从配置来源到当前 OpenAI-compatible HTTP body 的 presence；不扩大到 retry、cycler、subagent、compaction 或启用 native protocol。

## 必须保留的行为

每个数值字段按 `per-call explicit option > model setting > provider setting > built-in fallback` 解析。temperature 与 max_tokens 分别解析，不能因一个字段缺省而丢弃另一个字段。显式 `0` 是有效值；只有缺少字段/option 才是 unset。

保留 `defaultConfig()` 中现有的 `4096` 作为最低内建 fallback，但不能把它同时当作显式 model 设置和 provider 设置。否则默认 `Model.MaxTokens=4096` 会再次遮住 provider 的显式值。temperature 没有显式配置时继续交给当前 adapter 的默认行为；显式 temperature `0` 必须能到达 wire DTO。

现有的公开标量字段和调用方式保持兼容。直接构造的旧 `Config`/`llm.ModelConfig`/`ModelRequest` 中非零 sampling 值仍按旧调用习惯视为已设置；旧标量为零时仍表示 unset。需要以代码显式设置零值时，使用新增的 per-field `Set`/presence API。正常 `Loader.Load` 与 `UnmarshalConfig` 的来源 presence 是权威信息，不能用字段值是否为零推断。

## 源码路径与缺陷

1. `internal/config/types.go` 的 Provider、Model、SmallModel 都只有 `float64`/`int` 标量。`internal/config/loader.go` 的 `defaultConfig()` 在 Provider 和 Model 两层都填入 4096。
2. JSON 由 `UnmarshalConfig` 直接解入 struct；YAML 经 `parseYAMLTree` 和 `assignFromMap`。两条路径目前都不保留 key 是否出现。环境层 `loadFromEnv` 调用 `parseFloatEnv`/`parseIntEnv`，其返回值也无法区分未设置与有效的 `"0"`。
3. `mergeConfigs` 使用 `overlayValue`，numeric zero 一律不覆盖低优先级值。因此文件/环境/flag/override 的显式零不能形成最终配置。
4. `internal/cli/assemble.go:buildModel` 当前只把模型名传给 provider；small-model 路径传普通标量。`internal/llm.ModelRequest`、`llm.ModelConfig` 也只带标量，解析时无法继续传递零值 presence。
5. 当前 Eino/OpenAI request encoding 先把值放入 `openAIRequest`，其 `temperature` 与 `max_tokens` 使用 `omitempty`。编码后的零值会消失，即使上游已表示它是显式零。

## 最小接口方案

保留现有 `Temperature float64` 和 `MaxTokens int` 字段；用只针对这两个参数的 presence 元数据贯穿，不替换成新配置框架：

- 在 config 的 Provider、Model、SmallModel 段以及 `llm.ModelConfig`、`llm.ModelRequest` 增加 additive 的 `TemperatureSet`、`MaxTokensSet`（或等价的可选值 API）。配置文档仍只接受原有 `temperature`、`max_tokens` 键；presence 元数据不序列化到配置文件。
- 对 `Config` 增加内部 `samplingPresenceKnown` 来源标记，并提供窄的 exported accessor/helper 给 `internal/cli` 读取各 section 的 sampling `{value, set}`。`UnmarshalConfig`/`Loader.Load` 产生 presence-known 配置；`defaultConfig` 也明确该状态，两个 4096 值是兼容展示值/最低 fallback，而不是显式 Model/Provider override。
- JSON/YAML 解码按 key 是否存在设置两个 presence bit，即使值为零。`loadFromEnv` 使用存在性感知解析：已设置且合法的 `"0"` 标记为 set；空值/未设置保留现有 unset 行为。对五层合并，来源 presence-known 时严格用 bit 决定是否覆盖，并允许 set-zero 覆盖；未标记为 presence-known 的程序构造层沿用兼容约定，非零视为 set，零只有显式 Set bit 才覆盖。合并结果为 presence-known。
- Resolver/assembly 将有效值及 set 状态一次性放进 `ModelRequest` 和最终 `llm.ModelConfig`。未显式 model 值时不得读取 Model 的合成 4096 作为 model override；先选显式 provider 值，否则使用最低 4096 fallback。SmallModel 也要携带自己的 presence，遵守既有 resolver/provider identity 结果，不从 primary model 的合成值复制采样。
- 目前 `GenerationOptions.Temperature`/`MaxTokens` 已是指针，保留此 API；非 nil 的 per-call option（包括指向零的值）最高优先。不要用 `!= 0` 作为选择条件。
- Adapter 编码必须同时表达“缺省”和“显式零”。对 OpenAI-compatible DTO，推荐把可省略 numeric 字段编码为 `*float64`/`*int` 并仅在有效值存在时赋指针；显式零因此保留在 JSON，unset 则交给最低 fallback/adapter。不得只移除 `omitempty` 而让所有 unset 字段被编码为零。

该方案不要求改动 `Temperature`/`MaxTokens` 原字段的 Go 类型，也不改变配置键、Loader 五层优先级、credential/protocol resolver 或 `GenerationOptions`。若实现者选择等价的 presence 表示，必须保留上述 loaded-vs-programmatic 区分和零值行为，并在 review 前记录具体 API。

## 文件所有权与顺序

由当前 R3 provider builder 单独拥有以下文件，按“config presence → request/resolver → adapter encoding → assembly fixture”顺序完成，避免与其他 lane 并行改相同文件：

| 文件 | 责任 |
|---|---|
| `internal/config/types.go` | 保留标量，增加 sampling presence 元数据与 CLI 可读 accessor。 |
| `internal/config/loader.go` | 默认来源标记、env presence 解析、presence-aware `mergeConfigs`。 |
| `internal/config/yaml_loader.go` | JSON/YAML 输入 key presence 标记，不改变既有配置语法。 |
| `internal/llm/llm.go` | `ModelConfig`/`ModelRequest` 兼容标量上的 presence seam；per-call pointer API 不变。 |
| `internal/llm/model_resolver.go`, `internal/llm/registry.go` | 保留 sampling 的值与 presence，生成 adapter config 时不把零误作 unset。 |
| `internal/llm/provider.go` | 当前 active Eino/OpenAI-compatible JSON body 的可选字段编码。 |
| `internal/cli/assemble.go` | 把 Loader accessor 用于 primary/small-model request；只改 sampling 组装 hunk，保留同一 builder 的 credential/resolver 改动。 |
| 上述包的定向测试文件；新增的真实装配 fixture 可放 `internal/cli/assemble_sampling_test.go` | 下表的 RED/GREEN 与真实 Loader→HTTP 验收。 |

`internal/llm/native.go` 的 Claude/Gemini protocol 在当前 CLI 仍 disabled；本切片不得启用它们或宣称 native sampling 已验收。共享 `ModelConfig` 若触及这些测试，维持既有 adapter defaults，并把 native wire presence 留在对应 protocol 启用的独立验收中。

## 可执行验收矩阵

所有 RED 都必须是可编译且命中断言的行为失败；不接受缺符号/编译失败或 0 tests matched。GREEN 使用同一组测试并对受影响包跑 race。

| 层 | 回归验收 |
|---|---|
| JSON 与 YAML 来源 | 分别覆盖缺省、非零、显式 `temperature: 0`、`max_tokens: 0`，验证 provider/model/small_model 的 value 与 Set。缺省字段 Set=false；显式零 Set=true。 |
| 环境变量与优先级 | `GO_CLI_PROVIDER_TEMPERATURE=0`、`GO_CLI_MODEL_MAX_TOKENS=0` 等合法零值保留 presence；higher layer 的零覆盖 lower layer 的非零。空/未设置仍不覆盖。 |
| programmatic 兼容 | 旧式 code-built nonzero scalar 继续胜出；code-built zero 不意外覆盖；新 Set API 可表达显式零。Loaded 配置里的 4096 默认标量不会伪装成显式 Model setting。 |
| request 解析 | 分字段断言 per-call > model > provider > fallback。至少包含 provider `0.8/512`、model 未设置时解析为 `0.8/512`；model 显式 `0/0` 时解析为 `0/0`；per-call `WithTemperature(0)`/`WithMaxTokens(0)` 覆盖更高层非零。 |
| wire encoding | 对当前 active OpenAI-compatible adapter，用原始 JSON map/bytes 断言显式零是 numeric `0`，而非字段缺失；unset temperature 遵守 adapter 默认；无显式 model/provider max token 时最低 fallback 为 4096。确认普通非零旧 `ModelConfig` 仍编码。 |
| 真实装配闭环 | `httptest.Server` 接收经真实 `Loader.Load`、CLI primary/small-model assembly、resolver/provider adapter 发送的请求；断言 endpoint/model/temperature/max_tokens。复现 provider `0.8/512` 不被默认 model 的 `0/4096` 遮住，并覆盖 model 明确零到 wire。 |

原始 `Loader` API/precedence、credential header scope、native protocol enabled state 必须保持现状。完成后只报告本切片确切 owned commits、RED/GREEN 命令、受影响包 race 与真实 HTTP fixture 结果；不要以 resolver helper 单测替代 Loader→HTTP 验收。
