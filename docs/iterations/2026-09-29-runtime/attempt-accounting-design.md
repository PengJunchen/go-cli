# R3 operation retry and usage provenance design

状态：面向下一轮 builder 的源码约束与最小实施契约。原重核基线为 HEAD 74e33759c538e083cb7cc7b494e8a959b0b6e40c；review 记录基于 17ab7b7。operation/dispatch seams 已在当前根 source HEAD 1144e1ebc9f339a04d502f0bdca04069c9d30ff5 复核；native Claude codec 与 CLI tool-cache/close-owner 的后续修改不改变 provider dispatch fence、resolver/cycler/middleware 或 Loop authority 边界。本文只写设计，不表示代码、测试、集成或线上验收已完成。

依据：[`r3-provider-design.md`](../2026-09-25-runtime/r3-provider-design.md)、[`r3-production-remaining-contract.md`](../2026-09-25-runtime/r3-production-remaining-contract.md)、[`r3-breaker-loop-handoff.md`](../2026-09-25-runtime/r3-breaker-loop-handoff.md)、[`r2-core-consumer-handoff.md`](../2026-09-25-runtime/r2-core-consumer-handoff.md)；reviews [`r3-retry-safety-review.md`](../2026-09-25-runtime/reviews/r3-retry-safety-review.md)、[`r3-invalid-request-retry-review.md`](../2026-09-25-runtime/reviews/r3-invalid-request-retry-review.md)、[`r3-contract-review.md`](../2026-09-25-runtime/reviews/r3-contract-review.md)、[`r2-core-loop-consumer-review.md`](../2026-09-25-runtime/reviews/r2-core-loop-consumer-review.md)。规则依据主仓与 go-cli-infra 的 AGENTS/CLAUDE、`rules/go-validation-rules.md` 和 `rules/verify-rules.md`。

## 范围与实施顺序

本设计只覆盖 Loop operation 的 provider dispatch 重试预算，以及每次真实 dispatch 的 usage/cost provenance；不增加资金账本，也不把成本记录塞进 RunCommit 或 session action journal。`ResolvedModel` 当前持有实际 ProviderID、ModelID、ProtocolID、规范化 Origin 和私有 adapter/config/credential 状态。本设计沿用现有 endpoint/credential resolution，只要求 resolver 的 candidate handle 同时保留 typed source class（default endpoint 或 explicit request endpoint）供安全 identity 使用。实际 endpoint 与 scoped credential 仅保留在不可序列化的 candidate handle 中；公开 attempt identity 使用 provider/model/protocol、typed OriginKind 和 opaque CandidateID，不写任意 URL、secret、header 或 prompt。PRD-04 的硬预算、breaker 状态迁移和 live loop detector 不在此切片；保留现有“一次逻辑 chain call 进一次 breaker”的边界。

先完成当前已分派的 stream-close/join BUILD-1 和 durable CLI prompt/REPL 接线，再由 R2-REV-01 确认 restart-safety。随后按单一 core-loop owner 顺序做 durable compaction，然后做 operation dispatch authority 与 usage accounting。BUILD-1 仍修改 Loop stream reader 时，不并行修改 Loop 的 operation 调度。最终以 assembled loopback provider 验收 retry、fallback、overflow 和 stream usage。

## 当前源码事实

| 源码 | 当前可核实行为 | 实施含义 |
|---|---|---|
| internal/llm/middleware_retry.go:254-320 | Generate 和同步 Stream 初始化错误各自循环并调用 next；mid-stream channel error 不重试。 | 默认 production chain 不能保留此 dispatch authority。保持 mid-stream 不 replay 的性质。 |
| internal/llm/middleware_failover.go:89-143 | Generate 和 Stream 对候选模型逐个再次调用。 | 候选选择可以由中间件分类，但实际下一次 dispatch 必须回到 Loop operation owner。 |
| internal/llm/middleware_overflow.go:88-160 | overflow wrapper 自己循环重新调用 inner；它还保有单独的 maxRetries。 | overflow compact-and-retry 与普通 retry/fallback 必须共用一个 dispatch 计数。 |
| internal/llm/cycler.go:219-311 | `cycledModel.Generate` 在 selected build failure 或任何 selected Generate error 后直接调用 primary（:246-265）；`Stream` 在 build failure 或 selected Stream initialization error 后调用 primary（:268-290）。成功建立且有 cleanup 的 stream 由 forwarding goroutine 转发；当前 cancellation 分支提前 return，不 drain source（:300-311），而 defer 顺序会先 close output 再执行 cleanup（:300-302）。这些分支当前没有 `ContinuationNoReplay` 检查；cycler 不在 retry/failover/overflow middleware map 内。 | selection 本身不计 dispatch，但每次 selected/primary 实际候选请求都必须通过同一 Loop operation authority；其隐藏 primary fallback 不能绕开共享预算、typed facts 或 ContinuationNoReplay。当前 BUILD-1 owner 负责修复 source drain、cleanup、close ordering；本切片依赖该修复先整合，再接入 authority。Loop 只有在 actual attempt/source/cleanup 已 join 后才能授权下一个 candidate。 |
| internal/cli/assemble.go:1218-1233 | 默认组装设置 MaxAttempts=3 并将 retry middleware 安装进链。 | 这是 retry wrapper 的本地上限，不证明整个 model chain 的 dispatch 总量不超过 3。 |
| internal/llm/provider.go:492-514、internal/llm/native.go:179-190,256-268 | 实际 HTTP 请求分别在 OpenAI-compatible 与 native adapter 的 client.Do 边界发出；adapter 已持有实际 provider/model/endpoint。 | AttemptID 必须由这里的 dispatch fence 分配；不能从 Loop 的初始 config 或 wrapper 创建次数推断。 |
| internal/llm/model_resolver.go:111-125 | ResolvedModel 持有 canonical provider/model/protocol、实际规范化 Origin、adapter 和私有 ModelConfig。Origin 当前是 endpoint origin 字符串，不是可直接放日志的 analytics 字段。 | candidate handle 封装实际 ResolvedModel 并绑定真实 transport/credential scope；attempt identity 用 typed OriginKind 与 opaque CandidateID 标识来源，不导出 Origin URL 或凭证。 |
| internal/production/llm_integration.go:121-161 | CostTracker 只在成功的非 stream Generate 且 Usage 非 nil 时记账；Stream 直接透传。 | 失败 dispatch、stream、terminal-error usage 和 unknown usage 当前没有一次性 attempt 记录。 |
| internal/cli/output_guard_adapter.go:366-407 | assembled order places the CircuitBreaker outside the complete seven-layer model chain, so one top-level Generate/Stream enters the breaker once. | Keep candidate proposals inside that single chain call; Loop authority synchronously authorizes each downstream candidate. Do not re-enter the top-level wrapped model per retry, or breaker calls would multiply. |
| internal/core/loop.go:651,1262-1310 | Loop 逐轮调用 model；length continuation 已有单独的 run-level 上限。 | Loop 可持有 logical-operation 预算；continuation 保持独立 operation 和原 run-level cap。 |
| internal/cli/assemble.go:1607-1614、internal/compaction/factory.go:47-65、internal/cli/assemble.go:2164-2179 | Factory 支持 `summary` 注入 configured Summarizer，但 assembly 只在空/unified/micro_first strategy 注入 `smallModel`；`llmModelSummarizer` 直接调用 `s.model.Generate`。 | 加 assembly-level `summary` 与 `unified` RED；每次 summary call 用独立 Summary operation + identity/AttemptID/usage 记录，不占 main operation cap。 |

这些事实来自当前工作树静态读取；后续 native Claude codec 扩展改变了请求/流解析，但实际 native `client.Do` fence 仍在 `native.go:190` 和 stream round-trip 路径，OpenAI-compatible fence 仍在 `provider.go:513`。既有 retry-safety review 接受的是本地 attempt 次数、取消门和 typed 分类的窄切片，并明确不覆盖 operation-wide budget 或 usage accounting；不得把该 review 的局部验收当作这些剩余项已完成。旧 review 中的 P1 仅作历史记录，不作为当前缺陷结论。

## Operation 与 dispatch 契约

一个 provider operation 是 Loop 为一次逻辑模型请求创建的调度单元。普通 retry、failover、overflow 后 compact-and-retry 和 cycler selected→primary 都共用它的 MaxAttempts。长度 continuation 是新 operation，但仍消耗固定 run-level continuation cap；切换候选、retry 或 compaction 不重置任一计数。summary-model 调用另开 `OperationKindSummary` operation，不占主模型 operation 的 dispatch cap，但使用同一 attempt/cost sink。

middleware 和 cycler 只能提出下一候选与 typed failure；它们没有 authority 直接 dispatch。Loop 在进入一次逻辑 chain call 前调用 `RunOperationFactory.NewOperation`，持有返回的 `OperationDispatchAuthority`，并用 `WithOperationDispatchAuthority` 把它附加到该次 model context；Loop 是 `AuthorizeCandidate` 的唯一实现者。`WithRunOperationFactory` 另留在 run context，供 summarizer 创建独立的 `OperationKindSummary`。候选 proposals 在这一次 chain call 中逐次同步送给 Loop authority，再把 permit 传给下一个 candidate；Loop 不为每个 retry 重新调用最外层 wrapped model。每个 initial/retry/failover/overflow/cycler 候选都必须走 proposal→authorization→permit→adapter fence：初始候选也要 permit。wrapper 提案获准后用 `WithDispatchPermit` 只向一个 downstream candidate 传递 one-shot permit；adapter 从该次 context 取 permit 并校验真实 candidate handle。没有 permit、重复消费、或 candidate handle 不匹配时，adapter 在 client.Do 前拒绝，且不创建 AttemptID/usage row。这样 Retry/Failover/Overflow proposals 仍在已有 breaker 的一次 chain call 内；Summary 是独立 chain call，也只经过 breaker 一次。

最小跨层 API 放在 internal/llm；core 持有操作状态并实现 authority，provider 不依赖 core：

```go
type ProviderOperationID string
type ProviderAttemptID string
type CandidateID string

type CandidateReason uint8 // Initial, Retry, Failover, Overflow, CyclerSelected, CyclerPrimary
type ProviderBuildMode uint8 // UnboundLibraryBuild, GuardedCandidateBuild; fixed when model is constructed
type ProviderOperationKind uint8 // MainRequest, Continuation, Summary
type OriginKind uint8 // ResolverDefaultEndpoint, ExplicitRequestEndpoint; never a URL
type AttemptOutcome uint8 // Success, ProviderFailure, TransportFailure, Cancelled
type UsageState uint8 // UsageKnown, UsageUnknown
type UsageSource uint8 // ProviderResponse, Unknown
type FailureClass uint8 // actual terminal failure, only for a dispatch that crossed BeginDispatch
type PreDispatchFailureClass uint8 // typed construction failure before BeginDispatch
type AntecedentKind uint8 // Initial, FinishedAttempt, FinishedOverflow, PreDispatchFailure

const (
    CandidateInitial CandidateReason = iota + 1
    CandidateRetry
    CandidateFailover
    CandidateOverflow
    CandidateCyclerSelected
    CandidateCyclerPrimary
)
const (
    OperationMainRequest ProviderOperationKind = iota + 1
    OperationContinuation
    OperationSummary
)
const (
    OriginResolverDefault OriginKind = iota + 1
    OriginExplicitRequest
)
const (
    AttemptSuccess AttemptOutcome = iota + 1
    AttemptProviderFailure
    AttemptTransportFailure
    AttemptCancelled
)
const (
    UsageKnown UsageState = iota + 1
    UsageUnknown
)
const (
    UsageProviderResponse UsageSource = iota + 1
    UsageNoAuthoritativeUsage
)
const (
    FailureNone FailureClass = iota
    FailureTransientProvider
    FailureOverflow
    FailureAuth
    FailureInvalidRequest
    FailureProtocol
    FailureCancelled
    FailureOther
)
const (
    PreDispatchCandidateUnavailable PreDispatchFailureClass = iota + 1 // the sole SafeBuildFallback-eligible setup class
    PreDispatchAuth
    PreDispatchInvalidRequest
    PreDispatchProtocol
    PreDispatchCancelled
    PreDispatchSetupUnknown
)
const (
    UnboundLibraryBuild ProviderBuildMode = iota + 1
    GuardedCandidateBuild
)
const (
    AntecedentInitial AntecedentKind = iota + 1
    AntecedentFinishedAttempt
    AntecedentFinishedOverflow
    AntecedentPreDispatchFailure
)

type AttemptIdentity struct {
    ProviderID string
    ModelID    string
    ProtocolID ProtocolID
    OriginKind OriginKind
    Candidate  CandidateID // opaque resolver-issued identity; no endpoint or credential
}

// CandidateAntecedent keeps an actual finished dispatch separate from a
// construction failure. Fields are a tagged union: Initial has neither
// AttemptID nor PreDispatch; FinishedAttempt/FinishedOverflow have only
// AttemptID; PreDispatchFailure has only PreDispatch.
type CandidateAntecedent struct {
    Kind       AntecedentKind
    AttemptID  ProviderAttemptID
    PreDispatch *PreDispatchCandidateFailure
}
type PreDispatchCandidateFailure struct {
    FailedCandidateID CandidateID
    Class             PreDispatchFailureClass
    PreviousAttemptID ProviderAttemptID // optional prior actual attempt; never classified as this build failure
}

type DispatchDenialCode uint8 // cancelled, replay-forbidden, safety-fact, budget, stale-previous, candidate-set, repeated-pre-dispatch, overflow-used
type DispatchDenial struct { Code DispatchDenialCode }
type DispatchDecision struct {
    Permit DispatchPermit
    Denial *DispatchDenial // exactly one of Permit or Denial on a policy decision
}
const (
    DeniedCancelled DispatchDenialCode = iota + 1
    DeniedReplayForbidden
    DeniedSafetyFact
    DeniedBudget
    DeniedStalePrevious
    DeniedCandidateMismatch
    DeniedCandidateOutsideSet
    DeniedRepeatedPreDispatchFailure
    DeniedOverflowRecoveryUsed
)

type UsageEvidence struct {
    State  UsageState
    Usage  *Usage // required only when State == UsageKnown
    Source UsageSource
}

type ProviderAttemptTerminal struct {
    Outcome AttemptOutcome
    Failure FailureClass // FailureNone on success; typed classification otherwise
    Usage   UsageEvidence
}

type DispatchPermit interface {
    OperationID() ProviderOperationID
    CandidateID() CandidateID
}

// Opaque: wraps one actual ResolvedModel, including private transport and
// scoped credential state. Only safe Identity() is observable to accounting.
type ResolvedCandidateHandle interface {
    CandidateID() CandidateID
    Identity() AttemptIdentity // provider, model, protocol, typed OriginKind; no URL or secret
}

type CandidateProposal struct {
    OperationID ProviderOperationID
    Candidate   ResolvedCandidateHandle
    Reason      CandidateReason
    Antecedent  CandidateAntecedent // typed trigger; actual result and pre-dispatch failure are distinct
}

type OperationDispatchAuthority interface {
    ID() ProviderOperationID
    AuthorizeCandidate(context.Context, CandidateProposal) (DispatchDecision, error) // error is an internal authority failure; policy rejection is typed Denial
}

type RunOperationFactory interface {
    NewOperation(ctx context.Context, kind ProviderOperationKind, maxDispatches int, candidates []ResolvedCandidateHandle) (OperationDispatchAuthority, error)
}

func WithRunOperationFactory(ctx context.Context, factory RunOperationFactory) context.Context
func WithOperationDispatchAuthority(ctx context.Context, operation OperationDispatchAuthority) context.Context
func OperationDispatchAuthorityFromContext(ctx context.Context) (OperationDispatchAuthority, bool)
func WithDispatchPermit(ctx context.Context, permit DispatchPermit) context.Context

type ProviderDispatchAuthority interface {
    BeginDispatch(DispatchPermit, ResolvedCandidateHandle) (ProviderAttempt, error)
}

type ProviderAttempt interface {
    ID() ProviderAttemptID
    Finish(ProviderAttemptTerminal) (ProviderAttemptResult, error)
}

type ProviderAttemptResult struct {
    OperationID ProviderOperationID
    AttemptID   ProviderAttemptID
    Identity    AttemptIdentity
    Outcome     AttemptOutcome
    Failure     FailureClass // FailureNone on success; frozen with the terminal result
    Usage       UsageEvidence // includes explicit Unknown, never numeric zero-as-unknown
}
```

`NewOperation` receives a finite resolver-issued `[]ResolvedCandidateHandle` and copies it into private operation state. The set contains the configured primary/failover handles and, for a cycler, distinct selected and primary handles; resolver-issued CandidateIDs are unique. Wrappers may propose only one of those exact handles and cannot mint or substitute candidates. Candidate-set construction is a required production Loop prerequisite, not a fallback callback. A missing resolver handle or empty guarded set is a typed setup error and terminates without calling an unbound `Provider.Build` path. The authority records first pre-dispatch failures by CandidateID, so setup-only proposal count is at most the set size; total proposals are bounded by that size plus the shared actual-dispatch allowance.

The initial candidate also has an explicit caller: the operation-entry adapter inside the existing breaker boundary prepares the initial candidate/request, proposes `CandidateInitial` (or `CandidateCyclerSelected` when ModelCycler made the initial choice) with `AntecedentInitial`, and invokes it only after the Loop authority returns a matching permit. A cycler's selected handle is separately identified from its configured primary. Every later retry/failover/overflow/cycler-primary call follows the same proposal path; no adapter silently assumes an initial permit. A denial returns before calling the next candidate or `client.Do`.

`DispatchPermit` has unexported owner/operation/candidate bindings. Only `BeginDispatch` verifies the exact handle and atomically consumes the one-shot permit, allocates AttemptID, and increments the operation's actual-dispatch count immediately before `client.Do`; wrappers cannot consume/reuse it themselves. A permit by itself is not a provider dispatch and does not consume `MaxAttempts`; any client.Do call after the fence does, even when it returns a transport error. The handle keeps the actual resolved endpoint and credential scope private; its public identity includes a typed origin provenance class plus opaque CandidateID, never raw Origin URL or credentials. `ResolvedModel.Origin` currently contains the normalized actual origin and remains private to this handle.

`CandidateProposal` intentionally contains no booleans claiming `CandidatePublished`, `ToolDispatchStarted`, `DurableAcknowledged`, `StreamJoined`, or cancellation state. A dispatched failure is referenced as `CandidateAntecedent{Kind: AntecedentFinishedAttempt, AttemptID: id}` or `{Kind: AntecedentFinishedOverflow, AttemptID: id}` and checked against that immutable terminal result. A candidate-construction failure uses `CandidateAntecedent{Kind: AntecedentPreDispatchFailure, PreDispatch: &PreDispatchCandidateFailure{FailedCandidateID, Class, PreviousAttemptID}}`; it creates no AttemptID/usage row. `CandidateProposal.Candidate` is the proposed next handle; `PreDispatch.FailedCandidateID` is the distinct handle whose setup failed, and `PreviousAttemptID` still names the last actual dispatch, if any. Thus the latest setup class never overwrites or relabels the immutable terminal of that prior attempt. `AntecedentInitial` has no previous attempt. Loop-owned operation state obtains safety facts from its own event-send path, tool-dispatch boundary, successful RunCommit acknowledgements, current-operation receipt/action state, and completed stream join/attempt handle. `AuthorizeCandidate` checks that the antecedent shape is valid, loads any referenced actual attempt result, verifies candidate membership and the antecedent's typed policy, then checks the current context, dispatch count, and owner-tracked facts before issuing a permit or typed denial. Wrappers never update safety facts or invent a successful terminal.

The active Loop operation context carries a `RunOperationFactory` backed by the same in-memory run dispatcher/accounting observer. When `llmModelSummarizer.Summarize` runs, it opens `OperationKindSummary` with an independent counter using the existing configured MaxAttempts policy, attaches that operation authority to the small-model call, and uses the same adapter permit/Finish/accounting path. The summary request has its own OperationID/AttemptID/identity/usage (including unknown); it is never charged to the main request's counter. Source/input sync must precede it; a later marker-sync failure does not erase this completed provider fact.

Each constructed model has a fixed `ProviderBuildMode`. Exported direct provider `Build`/`BuildModel` APIs, including Eino and native adapters, deliberately create an `UnboundLibraryBuild`: a one-call library model outside the Loop operation/attempt guarantee, with no automatic retry/fallback. This mode is fixed at construction; it is never inferred later from a missing operation context. `ResolvedModel.Build` must use a resolver-only guarded constructor and create `GuardedCandidateBuild`, permanently bound to that resolver-issued handle's actual provider/model/protocol, private transport/endpoint, and scoped credentials; it must not delegate through a public direct Build that returns an unbound model. Default CLI assembly, the configured summary model, failover candidates, and cycler selected/primary all use guarded resolver builds. A guarded model with no matching permit fails before HTTP; it must never fall back to direct unbound Build.

Before the dispatch fence, the adapter may resolve local configuration, construct the provider model, serialize/validate the request, and prepare scoped credentials. Typed failures there use the separate pre-dispatch antecedent and do not receive an AttemptID. Once ready to dispatch, the model submits its candidate proposal to Loop, receives a one-shot permit, and calls `BeginDispatch(permit, actualResolvedCandidateHandle)` immediately before `client.Do`. `BeginDispatch` validates the bound private handle, consumes the permit, creates AttemptID, and increments the actual-dispatch counter. A `client.Do` error or response returned after that point is an actual attempt and must Finish with provider usage if authoritative, otherwise explicit unknown. `Finish` is one-shot and stores the immutable result at the Loop operation owner before any wrapper can propose another candidate; `CostTracker.RecordAttempt(result)` consumes the same result idempotently by typed AttemptID. Only the Loop owner applies retry/fallback policy.

Synchronous Generate finishes after response/body decode reaches terminal. Stream finishes only after BUILD-1 closes and joins the source/cleanup chain; until then its handle is pending and no next proposal can pass. Provider-reported usage is retained even on terminal provider error or cancellation. Unknown usage is an explicit state, not zero tokens/cost. Pre-dispatch denial has no AttemptID; post-fence error has an AttemptID and terminal result.

The logical breaker remains outside the candidate loop: one main-model operation invokes its configured breaker once around the full chain call, while retry/failover candidates do not each enter a new breaker. The summary-model operation is a distinct logical chain call and follows that operation boundary once.

CostTracker 扩为单一的 attempt 入口，例如 RecordAttempt(ProviderAttemptResult)，按 typed AttemptID 去重并保留一次 attempt 的 outcome、实际身份、usage source 与价格来源。Provider-reported usage、unknown usage、provider 明确报告的费用、由有来源价格计算的 estimated cost 必须是不同状态。usage 已知但价格未知时 cost 是 unknown；usage unknown 时不得显示为零成本。/cost 与 token_usage 输出需保留 known subtotal 和 unknown attempt 数，不把一次 unknown 隐藏为零。该数据只用于运行期可观测和展示，不是资金账本，不新增硬预算。

## Loop 的安全重试判定

Loop 对每次真实 candidate 请求实行不同的 typed antecedent，而不是把 setup error 填进上一个 attempt 的 failure 字段：

- **Initial:** `CandidateAntecedent{Kind: AntecedentInitial}`，无 AttemptID/上一错误；只用于 operation 的 first candidate。
- **Finished attempt:** `CandidateAntecedent{Kind: AntecedentFinishedAttempt, AttemptID}` 引用已 Finish 的 immutable `ProviderAttemptResult`。普通 retry/failover 只允许结果为 `FailureTransientProvider`；auth、invalid-request/schema、protocol、cancelled、策略和普通工具错误均无通用重试路径。
- **Finished overflow:** `CandidateAntecedent{Kind: AntecedentFinishedOverflow, AttemptID}` 必须引用实际已 dispatch、明确 context-limit 的 `FailureOverflow` result。Loop 每个 logical operation 最多启动一次 overflow recovery，并在开始该次 compact-and-retry 流程前消耗 `overflowRecoveryUsed`。只有 compaction 得到可用结果，且 durable 模式下 `CommitCompaction`/AppendAndSync/PublishedView refresh 成功后，才允许一个 `CandidateOverflow` permit。compaction 不可用、写入/同步失败或第二次 overflow 都不会获得下一次 dispatch；共享 MaxAttempts 仍另行检查。
- **Pre-dispatch failure:** `CandidateAntecedent{Kind: AntecedentPreDispatchFailure, PreDispatch: &PreDispatchCandidateFailure{FailedCandidateID, Class, PreviousAttemptID?}}` describes a real candidate's build/request-setup failure before the dispatch fence; it creates no AttemptID and does not claim that the previous actual attempt failed during setup. The proposed `Candidate` must be the next handle allowed by the immutable candidate ordering, while `FailedCandidateID` names the just-failed handle; the same setup-failed handle cannot be proposed again in this operation. If `PreviousAttemptID` is present, Loop must load that immutable actual result separately and confirm its terminal class allowed the sequence to reach the failed handle; the new setup class never substitutes for that result. `SafeBuildFallback` accepts only resolver-classified `PreDispatchCandidateUnavailable` and a configured next candidate (including a selected-cycler-to-primary transition). Auth/credential, invalid-request/schema, protocol, cancellation, missing/mismatched permit, missing resolver handle, and `PreDispatchSetupUnknown` all deny the transition. A continuation denies even an otherwise safe build fallback after its first candidate. Never turn arbitrary Build/Generate errors into failover-eligible setup failures.

`AuthorizeCandidate` 是 Loop 中唯一签 permit 的实现。它验证 proposal 的 tagged-union 形状、operation 和 candidate-set membership，从 owner 保存的前一实际 AttemptID 读取 immutable result，并从 Loop 自己的 event-send/stream sink、tool-dispatch boundary、RunCommit acknowledgement、当前 operation receipt/action state 和 stream-join owner 读取安全事实。proposal 不含 wrapper 自称的 visibility/tool/ack/join/cancel booleans。仅当当前 context 未取消、引用的真实 attempt 已 Finish（stream source 与 cleanup 已 join）、operation 实际 dispatch 数小于 MaxAttempts、无已发布 assistant candidate/partial text、工具 runner 尚未开始 dispatch、RunCommit 对 model result 尚未返回成功 partial acknowledgement、且相应 antecedent 的 typed policy 分支允许时，Loop 才签发 one-shot permit。Durable input sync 与已同步 compaction projection 可由同一活跃 run 继续使用，不表示 partial model-result ack。任一事实未知、缺失或冲突均拒绝；被拒绝的 proposal 不会触发下一 HTTP 请求。

历史范围由 Loop 的来源与 operation state 判定，不能用“history 中任何 unknown 一律拒绝”的全局条件。新用户 operation 的 initial main-request proposal 可以把完整、闭合且带 canonical `ReceiptOutcomeUnknown` 的既有 PublishedView group 原样作为历史上下文；它不是本 operation 的 previous attempt/receipt，旧 action 绝不由恢复或 initial request 自动重放。这个例外不允许同一 logical user operation 在新 action/tool dispatch 结果 unknown/pending 后 retry、fallback 或 continuation；当前 operation 内的 unknown、已开始的 tool intent/dispatch 与未完成结果都阻断后续 permit。R2/R4 quarantine 的 safe-read permit 仍需 trusted read-only effect 与实际 path-scope 正向证明；没有 proof 时工具 dispatch 仍被拒绝。Compaction eligibility 单独决定历史 unknown 是否可被压缩，见 durable-compaction-design。

`ContinuationNoReplay` 只允许该 continuation operation 的 first-candidate proposal（`CandidateInitial` 或 `CandidateCyclerSelected`），前提是 Loop 没有同一 turn/action scope 的 unresolved unknown。即使首个候选在 HTTP 前 build/setup 失败，后续任何 candidate proposal 都拒绝，包括 cycler primary、retry/failover/overflow，以及 `cycledModel.Generate/Stream` 内部任何隐藏 fallback；其错误 channel 仍须由 owner 完整 drain/join，但 drain 不授权重播。Cycler selected/primary 使用各自 resolver-issued handle/identity。CompleteResponseGuard 隐藏候选只能影响 Loop 实际观察到的 `CandidatePublished`，不能替代底层 join。

一个 candidate 可以在实际 dispatch 后因 `FailureTransientProvider` 于共享 MaxAttempts 内再次被选中；candidate identity 相同不构成拒绝理由，每次过 `BeginDispatch` 仍获得新 AttemptID 并占一次预算。相反，同一个 CandidateID 的 pre-dispatch build/setup failure 在一个 operation 中最多尝试一次，不因该错误消费 MaxAttempts；Loop 的 visited-pre-dispatch set 与有限 CandidateSet 共同限制此类 proposals。若候选集大小为 K，最多出现 MaxAttempts 次 fenced proposals 加 K 次第一次 pre-dispatch failure proposals；不能为获得这个上界而禁掉按 actual transient result 重试同一 candidate。

RunCommit 的 initial input 在第一次 provider request 前已同步，这是可由同一个仍存活 Loop operation 复用的输入，不等于 partial model result acknowledgment。已同步的 compaction projection 也只可由原 Loop 同一 run 复用。进程重启后的恢复永不从旧 AttemptID 自动派发 provider 请求；下一次用户提交才创建新的 operation。

Overflow 只允许协议层明确的 context-limit typed result 进入一次 compaction recovery；不得从自由文本错误、通用 network failure 或 wrapper boolean 推断。该 operation 的一次 recovery bit 先消费，再执行 typed compaction；durable projection 必须先成功 `CommitCompaction`/AppendAndSync/PublishedView refresh，之后才可请求 `CandidateOverflow` permit。sync 失败、compaction 非法、已发布 partial candidate、stream 未 join、tool dispatch、当前 operation unknown/pending receipt 或 durable partial acknowledgement 均禁止下一 dispatch；overflow recovery 不重置共享 MaxAttempts。各 adapter 跨过 `BeginDispatch` 的失败都需生成自己的 AttemptID 和 unknown usage（若无 provider usage）。

## 文件 owner 与交接边界

| Owner | 文件 | 责任 |
|---|---|---|
| R3 attempt builder | internal/llm/attempt.go（新）、operation_dispatch.go（new operation-entry adapter）、provider.go、native.go、model_resolver.go、llm.go | Typed antecedent/identity/permit/attempt/terminal result API; resolver creates a finite immutable set of opaque actual candidate handles; exported direct builds are fixed `UnboundLibraryBuild`, resolver builds are permanently `GuardedCandidateBuild`; each guarded adapter validates and consumes one-shot permit immediately before real `client.Do`, then Finish exactly once. |
| R3 attempt builder | internal/llm/middleware_retry.go、middleware_failover.go、middleware_overflow.go、cycler.go | Replace private dispatch loops with typed candidate proposals only. `SafeBuildFallback` accepts only resolver-classified `PreDispatchCandidateUnavailable` to a configured next handle; auth, invalid-request/schema, protocol, cancellation, missing-permit, and arbitrary setup failures do not fallback. No wrapper may set owner facts or call a second candidate without a Loop-issued permit. Preserve `ContinuationNoReplay` on every Generate/Stream hidden path. |
| R3 attempt builder（BUILD-1 完成后） | internal/core/loop.go、run_commit_consumer.go | Loop creates operation authority/factory and owns finite candidate set, attempt cap, previous immutable result, visited pre-dispatch candidate IDs, one-time overflow recovery bit, cancellation/publication/tool/ack/join facts, and current-vs-historical unknown scope. RunCommit ack and actual stream join update owner state. One operation can only dispatch after `AuthorizeCandidate` issues a permit. |
| R3 accounting builder | internal/production/llm_integration.go、cost_tracking.go、internal/cli/assemble.go、slash_handlers.go、interactive.go | Record each immutable result once by AttemptID (success/error/stream/unknown); preserve one breaker call per logical chain operation; summary calls create an independent operation but use the same accounting sink. |
| R3 summary-operation builder | internal/cli/assemble.go、internal/compaction/factory.go | Inject configured small model for explicit `summary` and `unified`; `llmModelSummarizer` opens a Summary operation through the run operation factory before Generate, so it cannot silently bypass attempt accounting. |
| R2 / session owner | internal/session/* | No provider-attempt/action ledger. RunCommit keeps only existing transcript/tool/compaction facts. |

R3 builder 不接管当前 streamjoin agent 的 BUILD-1 source reader/join 改动，不接管当前 durable CLI builder 的 session lease、prompt/REPL 生命周期改动，也不修改受保护 infra 文件。接手时先检查这些切片已整合的准确 SHA 与 Loop 工作树，再开始重叠的 core 改动。

## 必须先写的 RED 合约

- TestLogicalOperationBudgetCountsRealDispatchAcrossRetryFailoverAndOverflow：真实 assembled middleware + httptest server 记录 HTTP request 数；一次逻辑 operation 所有普通 transient retry、显式 fallback 与至多一次 overflow compact-and-retry 合计不超过 MaxAttempts，切换模型不重置。
- TestAuthorizationControlsActualDispatch：向 Loop authority 提交 initial 和 retry proposals；验证 adapter 只有拿到与具体 candidate 匹配的一次性 permit 才抵达 httptest server，重复/错配 permit 没有第二个请求。
- TestProposalUsesFinishedAttemptHandle：实际派发错误携带已 Finish 的 AttemptID/class；Loop 根据该 immutable result 验证 proposal。伪造、过期、非最近、跨 operation ID 或把实际结果误标成 setup failure 的 proposal 都不导致下一 HTTP 请求。
- TestLoopOwnerRejectsDisqualifyingRetryFacts：分别注入 cancelled context、已发布 partial candidate、已开始 tool dispatch、成功 durable model-result ack、未 join 的 stream、当前 operation unknown/pending receipt；每种情况下即使 wrapper 提交 typed eligible antecedent，第二个真实 HTTP 请求都为零。历史 closed unknown 不放在这组拒绝条件里。
- TestHistoricalUnknownInitialAndCurrentUnknownReplayHaveDifferentScope：恢复完整 closed historical unknown receipt 后，一个 fresh MainRequest initial proposal 可把旧 assistant/tool rows 原样带给 main model，不能把它当本 operation attempt 或重放旧 action；同 operation 新 action 产生 unknown 后的 retry/continuation proposal 均被 Loop 拒绝。
- TestAuthorizedTransientRetryMayReuseSameResolvedCandidate：一次真实 transient terminal 后同 CandidateID 可在 MaxAttempts 内再次获 permit；每个实际 dispatch 有独立 AttemptID/真实 identity。另测 failover/cycler primary 只能用 candidate set 中的 distinct configured handle，analytics/result 不含 URL、credential 或 prompt。
- TestOverflowCompactionRetryIsTypedSingleAndDurablySynced：只有真实 terminal `FailureOverflow` 可进入恢复；一次成功 compaction 后最多允许一个 `CandidateOverflow`，再一次 overflow 不会触发第二次 compaction/retry；durable projection sync/refresh 失败时下一 HTTP 请求为零。
- TestPreDispatchAntecedentKeepsSetupFailureSeparate：selected candidate 在 client.Do 前 `PreDispatchCandidateUnavailable` 不产生 AttemptID；安全分类可以按 `SafeBuildFallback` 规则选择配置的 primary，auth/credential、invalid-request/schema、protocol、cancelled、missing-permit、unknown setup 均不能触发 fallback。后续 candidate build failure 保留前一实际 AttemptID 和其原失败类作为独立字段，不将 previous result 改标为 build failure。
- TestPreDispatchCandidateFailuresAreBoundedByFiniteSet：NewOperation 接受的 resolver handle 集合有限且每个 CandidateID 唯一；每个 handle 的首次 pre-dispatch failure 最多提案一次且不得重试同一 setup-failed handle。所有 setup 都失败时无 HTTP/AttemptID/usage row，operation 以 typed setup failure 终止；总 proposals 不超过实际 dispatch cap 加候选集大小。
- TestDirectBuildIsUnboundAndResolvedBuildIsPermanentlyGuarded：导出的 direct Eino/native `Provider.Build`/`BuildModel` 固定为无自动重试的单调用 library mode；resolver `ResolvedModel.Build` 构造的 CLI primary、small model、failover 和 cycler model 永久要求匹配 permit。guarded model 缺 permit、错配或重放 permit 时 HTTP count 不增加，且不得 fallback 到 direct build。
- TestPreDispatchDenialVersusPostFenceFailure：credential/request-construction failure 在 fence 前无 AttemptID；越过 `client.Do` fence 后的网络错误有唯一 AttemptID、unknown usage terminal。
- TestSummaryModelIsSeparateOperationButAccounted：从 Loop compaction path 执行 configured summarizer；summary AttemptID/实际身份/known 或 unknown usage 均被记录，summary 不占 main-model MaxAttempts，main request 与 summary request 分开计数；summary attempt 不会因后续 CommitCompaction failure 被吞掉。
- TestAssembledCLIInjectsSummarizerForSummaryAndUnified：分别组装 strategy=`summary` 与 `unified`，配置 small model，证明实际 small model 被调用；不以 factory-only test 代替 CLI wiring 验收。
- TestBreakerRunsOncePerLogicalCandidateChain：一次 main operation 内即使有 retry/failover，breaker chain call 计数为一；一个 summary operation 另计一次。
- TestContinuationNoReplayDeniesAllAutomaticCandidateProposals：无当前 operation unknown 时 continuation 的 first candidate 可 dispatch；其后 retry/failover/overflow/cycler-primary proposal 均无 permit、无额外 HTTP request，包含 first candidate build/setup error 后 `cycledModel` 隐藏 primary 的情况；有同一 turn/action scope unknown 时连 continuation initial proposal 也无 permit。Run 级固定 continuation cap 仍跨 model switch 共享。
- TestCyclerFallbackUsesSameOperationBudgetAndIdentity：selected candidate dispatch 后发生 typed retry-safe failure时，configured primary fallback 由 Loop 授权并计入相同预算；仅 `PreDispatchCandidateUnavailable` 可使用明确的 safe build fallback 路径，不能将 selected build/setup error 泛化为 primary fallback。每条实际 attempt 记录真实 ProviderID、ModelID、protocol、OriginKind/CandidateID；pre-dispatch error 不虚构 selected attempt。
- TestCyclerCleansSelectedStreamBeforeAuthorizedFallback：selected Stream 完成准备后若在 fence 前发生被 allowlist 接受的 setup unavailable，所有资源关闭/join 且 Loop 授权后才允许 primary；stream channel error、partial candidate、auth/protocol/arbitrary setup error 或仍未 join 时无 primary dispatch。沿用 BUILD-1 drain/cleanup/close 顺序。
- TestContinuationDoesNotCyclerFallback：`ContinuationNoReplay` 下 selected model build/setup/generation error 均不能触发隐藏 primary；primary HTTP request 数为零。若 selected dispatch 已跨 fence，其 own attempt 按真实 identity/usage 记录；pre-dispatch failure 无 AttemptID。
- TestAttemptIdentityUsesActuallyResolvedCandidate：fallback/cycler 请求的 AttemptID 记录 fallback 的 canonical ProviderID、ModelID、protocol 和 origin，而不是初始 assembly model。
- TestPreDispatchFailureCreatesNoProviderAttempt：provider disabled、credential scope 错误、marshal/request construction 错误均为零 HTTP dispatch、零 usage attempt、零 cost charge。
- TestEveryDispatchedFailureRecordsOneUnknownUsage：服务器收到了请求后返回无 usage error，恰好一条有 AttemptID、实际身份、unknown usage 的 attempt 记录；调用 CostTracker 两次不得重复累计。
- TestStreamFinalAndTerminalErrorUsageRecordedOnce：成功 final usage 与带 usage 的 terminal error 都在 source join 后各记一次；无 usage stream 为 unknown；partial/mid-stream error 不会触发重播。
- TestContinuationUsesIndependentOperationButSharedRunCap：continuation 使用新 operation/AttemptID，retry 或 provider switch 不重置固定 run-level continuation cap。
- TestRecoveryNeverRedispatchesOldProviderOperation：恢复一个已同步 input 的 incomplete run 后，provider fixture request count 不增加。

这些 RED 测试应在 builder 实现前通过真实 httptest dispatch seam 重现失败；只断言 mock model 被调用次数，不能证明实际 dispatch 预算或 usage provenance。设计阶段没有运行测试。
