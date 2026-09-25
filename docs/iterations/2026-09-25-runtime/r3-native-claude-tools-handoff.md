# R3 native Claude tool-use handoff

状态：基于 be08f808c69f8e7c464f068aea7d1cd326c2a929 的实现交接设计；本次只读核对，不修改产品代码、支持注册或执行台账，也未运行测试。范围只覆盖 Anthropic Messages API 的 client-defined tool protocol；默认 CLI 的 native provider fail-closed 门禁保持不变。

## 用户故事与协议边界

用户在 CLI 中选用 Claude 并请求执行本地工具时，模型必须收到真实 JSON Schema；Claude 返回的 tool_use 必须成为含原始 ID、名称和 object 参数的 llm.ToolCall；工具执行后，下一轮请求必须把 assistant 的 tool_use 和按 ID 对应的 user tool_result 一起送回，之后才能返回最终文本。普通 Generate 和 Stream 必须得到相同的完整工具调用事实。

Anthropic 定义的 client-tool 流程是：请求带 tools；assistant 以 stop_reason: "tool_use" 返回一个或多个含 id/name/input 的 tool_use 块；客户端执行；紧接着以 user 消息中的 tool_result.tool_use_id 返回结果并继续对话。工具定义使用 name、可选 description 与 input_schema，而不是 OpenAI 的 function 包装层。[Tool use overview](https://platform.claude.com/docs/en/agents-and-tools/tool-use/overview), [How tool use works](https://platform.claude.com/docs/en/agents-and-tools/tool-use/how-tool-use-works), [Handle tool calls](https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls), [Define tools](https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools), [Messages API](https://platform.claude.com/docs/en/api/messages/create)

本切口只支持当前 llm.Message 可表达的文本、已支持的 base64 image 输入、assistant tool_use 与 user tool_result。Claude message content 中若出现不支持块，包括 thinking、redacted_thinking、server_tool_use、document、citation 等，必须返回不含原始 body/内容的明确错误，不能跳过块后继续或宣称 transcript 可安全复用。Anthropic 要求 thinking 签名原样、按原顺序回传；redacted thinking data 也是不透明值，现有 llm.Message 无字段承载它们，因此它们不属于本次支持矩阵。[Messages API: content block and thinking parameters](https://platform.claude.com/docs/en/api/messages/create)

llm.Message 将正文聚合在 Content，工具调用聚合在有序 ToolCalls，不能无损表示文本与工具块任意交错。最小实现限定支持“零个或多个文本块在前、随后零个或多个 tool_use 块”的 canonical 顺序；response 中 tool_use 后再出现文本或交错内容时，Generate 必须报 unsupported-order，Stream 必须报错且不发成功 Final，不能静默重排。这样工具调用列表顺序、ID 与各自参数得以准确保留。若以后要求接受任意块交错，需先为已有 ContentBlocks 增加可持久化的 typed tool block；那会触及 transcript clone/compaction，不是本 handoff 的扩框理由。

Anthropic 的 tool_result.is_error 是可选字段。当前 llm.Message 没有类型化结果错误位；实现不得从 “Error:” 等文本前缀推断错误，也不应增加第二套执行语义。本切口将已有结果字符串编码为 content；需要 provider-visible is_error 时，应先单独审定从 ReceiptOutcome 到 LLM 消息的窄类型字段。

## 源码核对与精确改动范围

- internal/llm/llm.go 已有 ToolDefinition{Name, Description, Parameters}、GenerationOptions.Tools、Message.ToolCalls、ToolCallID 和 Name，无需新 provider 抽象或 BaseChatModel 接口。
- internal/core/loop.go 每轮已通过 llm.WithTools 传入 tools.Parameterized.Parameters()，并在 assistant 调用后追加带 ToolCallID/Name 的 RoleTool 结果；并发结果会按原始 tool-call 顺序回写。Loop 走 Stream 并只从完整 Final 接受 tool calls。本任务不改 dispatch、权限或 receipt 语义。
- internal/llm/native.go 是唯一生产改动文件：claudeRequest 目前没有 tools；encodeClaudeRequest 忽略 genOpts.Tools，把 RoleTool 原样写成不合法的 role="tool"，assistant ToolCalls 也未编码；buildClaudeBlocks 对未知块和非 base64 image 静默跳过。新增 Anthropic tool definition（input_schema）、tool_use/tool_result wire 块映射及校验，修复这些静默丢弃路径。参数 schema 原样序列化；缺省 schema 沿用 OpenAI encoder 的空 object 约定 {"type":"object","properties":{}}，不可把 schema 改写成新结构。工具名不符合 Anthropic 名称限制时在发 HTTP 前拒绝，不得 sanitize 后改变执行名称；ToolCall.Args == nil 对无参工具编码成 {}，非 object 参数拒绝。
- decodeClaudeResponse 当前只拼接 text 并忽略其余块。解析 tool_use.input 为 JSON object，要求非空 ID/name、ID 唯一，并按 response 顺序填 Message.ToolCalls。只允许 text 在前、tool_use 在后；未知块、缺失/null/非 object input、重复 ID、stop_reason 与 tool-call presence 不匹配均 fail closed。把 max_tokens 与现有 Stream 的 length 映射统一放入一个小 helper；不要新增采样默认或改现有 sampling presence/优先级。
- streamClaude 已有 message/block lifecycle、SSE event 与 data type 一致性、tool_use ID/name、分片 JSON object 校验和 final ToolCalls。它将 tool args 只放在成功 Final，已有 GuardedStreamMaxBytes=400000、GuardedStreamMaxChunks=16384、GuardedStreamMaxToolCalls=4096 及 message_stop/终态校验。本任务须以 adversarial fixture 复核且保持这些限制；增加对 tool_use 后文本块的 unsupported-order 检查，保持无 Final/no tool dispatch。Claude stream 的事件顺序依据 [Anthropic streaming messages](https://platform.claude.com/docs/en/build-with-claude/streaming)：named SSE event 与 JSON type 相符、block 以连续 index 开始、tool input 经 input_json_delta 累积，最终 input 必须为 object。
- internal/llm/native_extra_test.go 现有 TestDecodeClaudeResponse_JoinsText 明确要求跳过 tool_use，这是要翻转的 RED 断言；它和 TestDecodeClaudeResponse_NoUsage 使用欠完整伪响应，应改成含 assistant role、终止 stop reason 的合法 fixture。集中放 encoding/decoder/stream 边界测试。
- internal/llm/native_test.go 现有 Claude 请求 httptest 可扩成 Generate 两请求回归，或新增 internal/llm/native_claude_tool_test.go 隔离 wire tests。增加 internal/core/claude_native_tool_loop_test.go 的真实 LoopAgent + httptest 集成，以覆盖实际 Stream→工具 side effect→第二请求闭环；测试只访问 loopback。

请求编码规则：Tools 中每项输出 {name,description?,input_schema}，保留既有 Auto 默认，不虚构 tool_choice。system 仍在顶层。assistant 有工具调用时输出一条 role="assistant"，先放聚合 text block，再按 Message.ToolCalls 顺序放 tool_use{id,name,input}。必须消费紧随其后的结果组：按 ToolCallID 精确匹配每个 call，拒绝缺失、重复、未知 ID 或可用时不相符的 Name；将结果收敛成一个 role="user" 消息，tool_result 块按原 assistant ToolCalls 顺序输出，禁止 wire role="tool"。游离 RoleTool 或不闭合 call group 在 HTTP 前失败。

Generate 应校验响应是可用的完整 assistant terminal：stop_reason == "tool_use" 当且仅当存在 client tool_use；其他 stop reason 不应携带 client tool calls。Stream 对应输出相同聚合 text、ID/name/args、FinishReason 和 usage；工具调用只能位于 Final，缺 message_stop、未闭合 block、错误的终态、坏 JSON、超限和 unsupported block/order 均不能发成功 Final。文本可能已由底层 stream 发出后才发现坏终态，这是 R1 明确的 partial-stream 语义；默认完整候选 guard 必须继续挡住未经确认的完整响应，不能调整 retry、sanitizer 或输出发布边界。

HTTP 成功 response 继续由 readBoundedProviderBody(..., maxProviderJSONBodyBytes) 限制在 10 MiB，错误 body 继续用 maxProviderErrorBodyBytes 的 4 KiB typed safe diagnostic；SSE frame 与 tool 累加维持以上 400000-byte/16384-event/4096-call 限制。请求体、schema、参数、tool output、thinking 与 provider body 不得进入 slog/span/错误字符串；httptest 使用运行时生成 canary，断言 canary 不进入错误或日志。此切口不重写通用 body reader，也不扩大最大值。

## TDD 验收

先落负测，确认在 be08f80 的 RED 原因分别是工具 schema 缺失、role="tool"/tool group 未映射、Generate 丢弃 tool_use、Stream/Generate 对相同工具 terminal 不一致；不以手写 resolver mock 或只测 JSON helper 代替实际 provider HTTP。

1. TestEncodeClaudeRequestIncludesToolsInputSchema：真实 provider 收到标准工具 schema、system 与既有 sampling 参数；schema 保持 JSON 层级/required，不出现 OpenAI function wrapper。无效工具名或不可 marshal schema 在 server request count 仍为 0。
2. TestDecodeClaudeResponsePreservesToolUseCalls：合法 assistant body 带 text + 两个 tool_use；断言原始 ID、name、object args、调用顺序、usage、tool_use terminal。table negatives 覆盖空/重复 ID、空 name、input 缺失/null/array/scalar/坏 JSON、tool_use stop reason 无调用、非工具 stop reason 却带调用、tool_use 后 text、thinking/redacted_thinking/server_tool_use/任意未知块；所有错误不含 raw input/body。
3. TestEncodeClaudeRequestGroupsToolResultsByCallID：assistant 两个调用，随后以不同顺序构造两条 RoleTool history；第二个 HTTP JSON 必须是 assistant 原始 tool_use IDs/args，再是单个 user message，其 tool_result 按 call 顺序且每份 content 对应正确 ID。断言没有 role=tool。缺/重复/陌生 ID、游离结果和不相符 Name 必须在 request=0 时失败。
4. TestClaudeGenerateToolUseTwoRequestLoopback：通过 ClaudeProvider.Build 发两次本地真实 HTTP；首响应给工具调用，第二请求包括准确闭合的 assistant/user group，返回最终文本；request count=2，direct tool results 按 ID 与顺序精确回传。
5. TestClaudeStreamToolUseParity：相同 fixture 经真实 Stream，比较拼接 text、final ToolCalls、FinishReason、Usage 与 Generate；调用 tool 只在完整 Final 后。另用真实 LoopAgent.RunCanonical/工具 registry 做 TestClaudeStreamToolUseLoopbackRunsExactlyOnce，server 恰好两请求、每项本地工具恰执行一次、第二请求包含 tool_result ID/order、最终 transcript/assistant text 正确。
6. TestClaudeStreamToolUseMalformedTerminalDoesNotDispatch：坏/不完整 input JSON、tool_use 后 text、message_delta/message_stop 缺失或冲突、post-message_stop 追加、事件 type/name 不符与 caps 超限；允许底层 partial text，但必须有 terminal error、无成功 Final、无工具副作用。默认 assembled OutputGuard 集成证据须单独由 native registration gate 提供，不能从 adapter helper 测试推断。
7. 对现有 TestClaudeStream... suite、provider_body_test.go 里的 10 MiB/4 KiB body 用例、SSE 限额测试运行定向 race；再跑 GOMAXPROCS=2 go test -p 2 -race ./internal/llm ./internal/core。固定测试 regex 的命中数与完整 race 输出为 evidence。设计阶段不预称这些检查已运行。

## 明确留给后续

Gemini 是不同协议，不纳入 Claude P1：geminiPart 已有 functionCall/functionResponse，encoder/decoder 有基本转换，Generate/Stream 当前都按位置合成 call_N ID；但 encodeGeminiRequest 同样忽略 GenerationOptions.Tools，没有发送 functionDeclarations，geminiFunctionCall 又没有解码 Google wire 可选的 id。Google 当前 API reference 把 FunctionCall.id 定义为 optional；后续必须按明确支持的 API/model 版本核实该字段并保留真实 ID（缺失时才有版本化的映射策略），不能假定永远无 ID。[Gemini GenerateContent FunctionCall](https://ai.google.dev/api/generate-content#v1beta.FunctionCall), [Gemini function calling](https://ai.google.dev/gemini-api/docs/generate-content/function-calling) 后续再单独核对 function declaration/call/response correlation 与多工具 stream 顺序，设计 provider-specific ID 处理和真实两请求 fixture；不得复制 Claude 的 tool_use_id 假设或借本任务打开 Gemini gate。

本 handoff 不启用 Claude/Gemini 的默认 CLI 路由，不改 provider resolver、注册表、采样、权限、tool schema 本身、retry/fallback、session projection 或 compaction。Claude native 注册仍需独立评审通过该协议的完整本地 fixture 后单独改 gate；非支持的 thinking 和 content block 必须保留 fail-closed。
