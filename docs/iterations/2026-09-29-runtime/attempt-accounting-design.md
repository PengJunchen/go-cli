# R3 operation retry and usage provenance design

状态：面向下一轮 builder 的源码约束与最小实施契约。基线源码为 HEAD 17ab7b76ef0b7636abff1e5a055de7acf6f6bb31。本文只写设计，不表示代码、测试、集成或线上验收已完成。

依据：[`r3-provider-design.md`](../2026-09-25-runtime/r3-provider-design.md)、[`r3-production-remaining-contract.md`](../2026-09-25-runtime/r3-production-remaining-contract.md)、[`r3-breaker-loop-handoff.md`](../2026-09-25-runtime/r3-breaker-loop-handoff.md)、[`r2-core-consumer-handoff.md`](../2026-09-25-runtime/r2-core-consumer-handoff.md)；reviews [`r3-retry-safety-review.md`](../2026-09-25-runtime/reviews/r3-retry-safety-review.md)、[`r3-invalid-request-retry-review.md`](../2026-09-25-runtime/reviews/r3-invalid-request-retry-review.md)、[`r3-contract-review.md`](../2026-09-25-runtime/reviews/r3-contract-review.md)、[`r2-core-loop-consumer-review.md`](../2026-09-25-runtime/reviews/r2-core-loop-consumer-review.md)。规则依据主仓与 go-cli-infra 的 AGENTS/CLAUDE、`rules/go-validation-rules.md` 和 `rules/verify-rules.md`。

## 范围与实施顺序

本设计只覆盖一个仍在运行的 Loop operation 中的 provider dispatch 重试预算，以及每次真实 dispatch 的 usage/cost provenance。现有 provider resolver 已给出 canonical ProviderID、ModelID、ProtocolID 和 Origin；本设计消费该身份，不重做 resolver，也不把成本记录塞进 RunCommit 或 session action journal。PRD-04 的硬预算、breaker 状态迁移和 live loop detector 都不在此切片。

先完成当前已分派的 stream-close/join BUILD-1 和 durable CLI prompt/REPL 接线，再由 R2-REV-01 确认 restart-safety。随后按单一 core-loop owner 顺序做 durable compaction，然后做 operation dispatch authority 与 usage accounting。BUILD-1 仍修改 Loop stream reader 时，不并行修改 Loop 的 operation 调度。最终以 assembled loopback provider 验收 retry、fallback、overflow 和 stream usage。

## 当前源码事实

| 源码 | 当前可核实行为 | 实施含义 |
|---|---|---|
| internal/llm/middleware_retry.go:254-320 | Generate 和同步 Stream 初始化错误各自循环并调用 next；mid-stream channel error 不重试。 | 默认 production chain 不能保留此 dispatch authority。保持 mid-stream 不 replay 的性质。 |
| internal/llm/middleware_failover.go:89-143 | Generate 和 Stream 对候选模型逐个再次调用。 | 候选选择可以由中间件分类，但实际下一次 dispatch 必须回到 Loop operation owner。 |
| internal/llm/middleware_overflow.go:88-160 | overflow wrapper 自己循环重新调用 inner；它还保有单独的 maxRetries。 | overflow compact-and-retry 与普通 retry/fallback 必须共用一个 dispatch 计数。 |
| internal/llm/cycler.go:219-299 | `cycledModel.Generate` 在 selected model build 或 generation error 时调用 primary；`Stream` 在 build/init error 时调用 primary。selected stream 成功建立后，cleanup 由 forwarding goroutine drain 后执行。cycler 不在 retry/failover/overflow middleware map 内。 | selection 本身不计 dispatch，但每次实际候选请求都要通过同一 operation authority；cycler 的隐藏 primary fallback 不能绕开共享预算或 typed facts。当前 BUILD-1 owner 负责 cycledStream drain/error/cleanup/close 次序；本切片依赖该修复完成后再接入 authority 与禁止隐藏 replay。 |
| internal/cli/assemble.go:1218-1233 | 默认组装设置 MaxAttempts=3 并将 retry middleware 安装进链。 | 这是 retry wrapper 的本地上限，不证明整个 model chain 的 dispatch 总量不超过 3。 |
| internal/llm/provider.go:492-514、internal/llm/native.go:179-190,256-268 | 实际 HTTP 请求分别在 OpenAI-compatible 与 native adapter 的 client.Do 边界发出；adapter 已持有实际 provider/model/endpoint。 | AttemptID 必须由这里的 dispatch fence 分配；不能从 Loop 的初始 config 或 wrapper 创建次数推断。 |
| internal/llm/model_resolver.go:111-125 | ResolvedModel 已含 canonical provider/model/protocol/origin 身份。 | Raw adapter 的不可变身份从实际 ResolvedModel 构造时捕获，并随实际候选传到 attempt。 |
| internal/production/llm_integration.go:121-161 | CostTracker 只在成功的非 stream Generate 且 Usage 非 nil 时记账；Stream 直接透传。 | 失败 dispatch、stream、terminal-error usage 和 unknown usage 当前没有一次性 attempt 记录。 |
| internal/core/loop.go:651,1262-1310 | Loop 逐轮调用 model；length continuation 已有单独的 run-level 上限。 | Loop 可持有 logical-operation 预算；continuation 保持独立 operation 和原 run-level cap。 |

这些事实来自当前工作树静态读取。既有 retry-safety review 接受的是本地 attempt 次数、取消门和 typed 分类的窄切片，并明确不覆盖 operation-wide budget 或 usage accounting；不得把该 review 的局部验收当作这些剩余项已完成。旧 review 中的 P1 仅作历史记录，不作为当前缺陷结论。

## Operation 与 dispatch 契约

一个 provider operation 是 Loop 为一次逻辑模型请求创建的调度单元。普通 retry、显式 fallback 和 overflow-triggered compact-and-retry 都消耗同一个 operation 的 MaxAttempts 总 provider dispatch 预算。长度 continuation 是新的 operation，但它仍消耗原来固定的 run-level continuation cap；切换候选模型、retry 或 compaction 不重置任一计数。

Loop 是唯一重试与 fallback 决策者，并持有该 operation 的计数、typed facts、候选切换与 backoff。Retry、failover、overflow middleware 和 ModelCycler 不再启动隐藏的第二次 dispatch loop，只返回 typed classification 或候选给 Loop。特别是 cycler 当前选中候选的 build/generation/setup error 会在 wrapper 内自动走 primary；builder 必须把这个候选与 primary 都暴露为同一 Loop operation 的候选 dispatch，或让 Loop 能观察并授权每个实际 attempt。不能保留 wrapper 内先发 selected、再无条件发 primary 的路径。实际 HTTP adapter 是 dispatch authority：在请求体/协议/凭据校验和 http.Request 构造完成后、紧邻 client.Do 之前，它向当前 operation 申请一个 attempt。请求格式错误、协议关闭、缺少 scoped credential、序列化失败等在此边界之前失败时，不分配 AttemptID，不记录 provider usage，也不计入 dispatch cap。

若 BeginDispatch 成功，provider 已可能接收并计费；即使 client.Do 立即报网络错误，也算一次已派发 attempt。owner 为它原子分配唯一 typed AttemptID，并在同一次提交中冻结实际 resolved identity。身份至少为 ProviderID、ModelID、ProtocolID、规范 Origin。不得保存 key、Authorization header、prompt body 或 raw provider error 到 attempt fact。

最小跨层 API 建议放在 internal/llm，使 core 可实现 owner，而 provider 不依赖 core：

    type ProviderOperationID string
    type ProviderAttemptID string

    type ResolvedProviderIdentity struct {
        ProviderID string
        ModelID    string
        ProtocolID ProtocolID
        Origin     string
    }

    type ProviderDispatchAuthority interface {
        BeginDispatch(context.Context, ProviderOperationID, ResolvedProviderIdentity) (ProviderAttempt, error)
    }

    type ProviderAttempt interface {
        ID() ProviderAttemptID
        Finish(ProviderAttemptTerminal) error
    }

    type ProviderAttemptResult struct {
        OperationID ProviderOperationID
        AttemptID   ProviderAttemptID
        Identity    ResolvedProviderIdentity
        Outcome     AttemptOutcome
        Usage       UsageEvidence
    }

ProviderAttemptTerminal 是 typed terminal outcome 与 UsageEvidence，不带可重试的自由文本判定；authority 以 handle 捕获的 OperationID、AttemptID 和 identity 组装不可变 ProviderAttemptResult，再交给 accounting sink。Loop 为每个逻辑调用创建隔离的 operation owner，attempt 数、retry facts 与 continuation policy 不放进 middleware 或全局 tracker。Attempt handle 捕获 BeginDispatch 时的 identity；Finish 只允许成功一次。BeginDispatch 在 client.Do 前被拒绝表示没有 dispatch、没有 usage 行。成功分配的每个 handle 最终必须有且只有一个 terminal record。

同步 Generate 在 client.Do/响应读取结束时 Finish。Stream 把同一 handle 交给拥有 response reader 的 adapter；它只在该 reader 到 terminal 并完成 BUILD-1 的 source close/join 后 Finish。若已 dispatch 却没有 authoritative usage，UsageEvidence 明确为 unknown；未知不能编码成输入/输出 token 数值零。若 provider 报告有效 usage，即使终态是 provider error 或取消，也保留该 usage 和 provider 来源。若 stream source 仍未 close，attempt 仍 pending；不能先记 unknown 后又覆写成最终 usage，也不能以未 join 的 stream 作为 retry 依据。

CostTracker 扩为单一的 attempt 入口，例如 RecordAttempt(ProviderAttemptResult)，按 typed AttemptID 去重并保留一次 attempt 的 outcome、实际身份、usage source 与价格来源。Provider-reported usage、unknown usage、provider 明确报告的费用、由有来源价格计算的 estimated cost 必须是不同状态。usage 已知但价格未知时 cost 是 unknown；usage unknown 时不得显示为零成本。/cost 与 token_usage 输出需保留 known subtotal 和 unknown attempt 数，不把一次 unknown 隐藏为零。该数据只用于运行期可观测和展示，不是资金账本，不新增硬预算。

## Loop 的安全重试判定

Loop 收到当前 provider attempt 的 typed terminal 后，只有同时满足以下事实才可决定再发一次：

- 当前 context 未取消，terminal 是 typed transient provider failure，分类不是 auth、invalid-request、protocol/schema、普通工具或策略错误。
- 当前 operation 的实际 BeginDispatch 次数小于 MaxAttempts；候选身份已完整解析且有正确 credential scope。
- 失败 attempt 的 stream reader/resource 已 join；没有可继续到达的 candidate。
- 没有 assistant candidate/partial text 已发布给调用方，没有工具 dispatch 开始，也没有该 provider result 的 partial durable acknowledgment。

任一事实未知、缺失或冲突时停止并返回原 typed error。CompleteResponseGuard 隐藏候选只能影响 CandidatePublished 的真实观测；它不能替代底层 stream join。Provider 已产生 partial chunk 但未 terminal 的 stream 不得透明重播。cycler 的 selected stream 即使 setup 返回 error，也只有在真实 adapter 能证明尚未 dispatch、无可见 candidate 且资源已关闭时，Loop 才能授权 primary fallback；一旦 selected candidate 有 AttemptID，就消耗同一预算。continuation request 仍是独立 operation，但 `ContinuationNoReplay` 必须禁止 retry、failover、overflow 和 cycler 自动 fallback；它不能因穿过另一层 wrapper 而重获 fallback 权限。工具调用组完成后属于后续 operation；工具 receipt 为 unknown、intent/dispatch 已开始或结果仍 pending 时，绝不 replay action 或重跑含该 action 的 Loop run。

RunCommit 的 initial input 在第一次 provider request 前已同步，这是可由同一个仍存活 Loop operation 复用的输入，不等于 partial model result acknowledgment。已同步的 compaction projection 也只可由原 Loop 同一 run 复用。进程重启后的恢复永不从旧 AttemptID 自动派发 provider 请求；下一次用户提交才创建新的 operation。

Overflow 只允许协议层明确的 context-limit typed error 进入 compaction 分支；安全判定仍由 Loop 按上述 facts 完成。compaction projection 若属于 durable run，必须先成功 CommitCompaction/AppendAndSync，再尝试共享预算内的下一次 provider dispatch。通用 network error、已发布 partial candidate、stream 未 join、tool dispatch、unknown receipt 或 durable partial acknowledgment 均不得以 overflow 名义绕过限制。各 adapter 的 failed POST 也必须生成自己的 AttemptID 和 unknown usage（若无 provider usage）。

## 文件 owner 与交接边界

| Owner | 文件 | 责任 |
|---|---|---|
| R3 attempt builder | internal/llm/attempt.go（新）、provider.go、native.go、model_resolver.go | typed dispatch/attempt API；从实际 ResolvedModel 固定身份；在各真实 client.Do 前授权、terminal 后 Finish。 |
| R3 attempt builder | internal/llm/middleware_retry.go、middleware_failover.go、middleware_overflow.go、cycler.go、internal/cli/assemble.go | 移除生产链中的本地 dispatch 重试/切换权；保留纯分类，统一候选交给 Loop；cycler selected/primary 分别按真实 resolved identity 经同一 authority。ContinuationNoReplay 沿所有候选路径生效。 |
| R3 attempt builder（BUILD-1 完成后） | internal/core/loop.go | 每次 provider request 创建 operation、传入 dispatch authority、保存 visibility/tool/durable acknowledgment 与 join facts、集中做 retry/fallback/backoff 决策。 |
| R3 accounting builder | internal/production/llm_integration.go、cost_tracking.go、internal/cli/slash_handlers.go、interactive.go | 只消费唯一 attempt result；unknown/known/estimated provenance 在累计值与展示中可见。 |
| R2 / session owner | internal/session/* | 不增加 provider attempt/action ledger；RunCommit 仍只保存已批准的 transcript、tool action 与 compaction projection 事实。 |

R3 builder 不接管当前 streamjoin agent 的 BUILD-1 source reader/join 改动，不接管当前 durable CLI builder 的 session lease、prompt/REPL 生命周期改动，也不修改受保护 infra 文件。接手时先检查这些切片已整合的准确 SHA 与 Loop 工作树，再开始重叠的 core 改动。

## 必须先写的 RED 合约

- TestLogicalOperationBudgetCountsRealDispatchAcrossRetryFailoverAndOverflow：真实 assembled middleware + httptest server 记录 HTTP request 数；一次逻辑 operation 所有普通 transient retry、显式 fallback 与 overflow retry 合计不超过 MaxAttempts，切换模型不重置。
- TestCyclerFallbackUsesSameOperationBudgetAndIdentity：selected candidate dispatch 后发生 typed retry-safe failure 时，任何 primary fallback 都由 Loop 授权、计入相同预算；每条 attempt 各记录其真实 ProviderID、ModelID、protocol 和 origin。build failure 在 provider dispatch fence 前发生时不虚构 selected attempt。
- TestCyclerCleansSelectedStreamBeforeAuthorizedFallback：selected Stream setup failure 的资源已关闭/join 后才允许 Loop 决定 primary；stream channel 错误、partial candidate 或仍未 join 时没有 primary dispatch。沿用 BUILD-1 的错误 channel drain/cleanup/close 次序，不由此切片改写。
- TestContinuationDoesNotCyclerFallback：`ContinuationNoReplay` 下 selected model build/setup/generation error 都不隐式调用 primary；primary HTTP request 数为零，Continuation 使用的自身 attempt 仍按实际 dispatch 记录。
- TestAttemptIdentityUsesActuallyResolvedCandidate：fallback/cycler 请求的 AttemptID 记录 fallback 的 canonical ProviderID、ModelID、protocol 和 origin，而不是初始 assembly model。
- TestPreDispatchFailureCreatesNoProviderAttempt：provider disabled、credential scope 错误、marshal/request construction 错误均为零 HTTP dispatch、零 usage attempt、零 cost charge。
- TestEveryDispatchedFailureRecordsOneUnknownUsage：服务器收到了请求后返回无 usage error，恰好一条有 AttemptID、实际身份、unknown usage 的 attempt 记录；调用 CostTracker 两次不得重复累计。
- TestStreamFinalAndTerminalErrorUsageRecordedOnce：成功 final usage 与带 usage 的 terminal error 都在 source join 后各记一次；无 usage stream 为 unknown；partial/mid-stream error 不会触发重播。
- TestLoopRetryStopsAfterPublicationOrToolOrAck：分别在 candidate 已可见、工具 dispatch 已开始、partial model result 已 durable ack、source 未 join 时注入 transient error，断言无下一 HTTP 请求。
- TestContinuationUsesIndependentOperationButSharedRunCap：continuation 使用新 operation/AttemptID，retry 或 provider switch 不重置固定 run-level continuation cap。
- TestRecoveryNeverRedispatchesOldProviderOperation：恢复一个已同步 input 的 incomplete run 后，provider fixture request count 不增加。

这些 RED 测试应在 builder 实现前通过真实 httptest dispatch seam 重现失败；只断言 mock model 被调用次数，不能证明实际 dispatch 预算或 usage provenance。设计阶段没有运行测试。
