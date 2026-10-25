# Sol runtime iteration: MCP discovery with an honest durable contract

Date: 2026-10-25. Baseline: `379134d` on `codex/sol-runtime-2026-10-25`.

## User story and scope

As a user of a durable interactive session, I want the model to inspect configured MCP server names without starting a connection or terminating the session, so that capability discovery remains a cheap, repeatable first step. Activating a server and executing its remote tools retain their current approval, trust, exact-generation lease, and completion requirements.

This iteration proposes one local metadata tool, `mcp_list`, with the same query semantics and safe status projection as `/mcp list`. The existing `mcp_discover` activation interface remains compatible. A passing local discovery journey will not be presented as a passing durable remote MCP journey.

## Primary-source findings

1. The baseline has already integrated dynamic runtime commit `d813dba` and live evaluation fix `de6ea76`. The latter grants a completion proof only to reviewed synchronous `tool_search` registry chains, improves Chinese-adjacent tool identifiers, and distinguishes actual user denial from callback/cancellation. See [search implementation](../../../internal/tools/search.go#L104), [registry proof selection](../../../internal/tools/trusted_registry.go#L293), and [approval denial source](../../../internal/approval/prepared_policy.go#L268). Reimplementing those fixes would duplicate completed work.
2. Lazy MCP registration currently uses ordinary `Register` for a single mixed `mcp_discover` tool. Its `Execute` either lists local metadata or first activates a server. See [registration](../../../internal/cli/progressive_mcp.go#L46) and [mixed execution](../../../internal/cli/progressive_mcp.go#L222). An ordinary registration has unknown effects and no trusted completion proof: [default registration metadata](../../../internal/tools/mcp_generation.go#L640).
3. Activation has lasting effects: it constructs/connects a client generation, prepares schemas, publishes the exact owner and registry transaction, and can start a notification watcher. Returning from `Execute` is therefore insufficient to declare the entire tool a synchronous local control. See [activation](../../../internal/cli/progressive_mcp.go#L105), [candidate preparation](../../../internal/cli/progressive_mcp.go#L144), and [watcher ownership](../../../internal/cli/progressive_mcp.go#L321).
4. Remote MCP candidates deliberately retain unknown effects and an exact dispatch lease. They do not currently carry a completion proof. See [candidate metadata](../../../internal/tools/mcp_generation.go#L376). The exact client generation already supports acquire/retire/drain; it forwards calls through the MCP client: [generation implementation](../../../internal/mcp/tool_adapter.go#L134).
5. The transport already distinguishes ambiguous remote outcomes from definite JSON-RPC responses. The HTTP request owner tracks generation activity and closes the response body before returning; its pending operation signal explicitly does not establish a remote side-effect outcome. See [request/outcome classification](../../../internal/mcp/request_boundary.go#L11), [pending HTTP operations](../../../internal/mcp/http_adapter.go#L72), and [request lifecycle](../../../internal/mcp/http_adapter.go#L670). These are useful ingredients for a future remote completion contract, but they are not yet such a contract.
6. Prepared completion is sealed and action scoped. Unknown completion is intentionally different from an already closed signal; only reviewed paths mint a closed positive proof. See [completion signal](../../../internal/tools/prepared_completion.go#L36), [proof modes and validation](../../../internal/tools/prepared_completion.go#L134), and [dispatch](../../../internal/tools/prepared.go#L963). This must remain intact.
7. The existing cross-run lazy MCP test exercises Harness and a mock model without a durable sink, while the project Skill regression explicitly binds two real durable runs. See [non-durable MCP journey](../../../internal/cli/dynamic_runtime_review_test.go#L70) and [durable Skill journey](../../../internal/cli/dynamic_project_durable_test.go#L20). The former does not establish durable discovery/activation/call support.

## Options considered

| Option | Interface and implementation | Depth / locality | Decision |
| --- | --- | --- | --- |
| Declare current `mcp_discover` trusted inline | One registration change; list and network activation still share one interface | Small change hides an invalid resource-lifecycle assertion | Reject: activation owns a live connection and watchers, and failures may retain pending cleanup |
| Split or rename existing discovery/activation tools | Make `mcp_discover` list-only and add `mcp_activate` | Clean separation, but changes existing model/tool callers and fixtures | Defer: compatibility cost exceeds the local discovery objective |
| Add local `mcp_list`, preserve existing activation | One additional tool interface; reuse the existing status projection and host inline registrar | High locality: one implementation serves model discovery and shares `/mcp list` semantics; no new generic proof seam | Recommended: bounded, executable, compatible |
| Complete durable remote MCP execution now | Introduce an action-scoped remote request receipt, distinguish response-known/outcome-unknown/local-joined, bind the exact connection owner, and extend durable proof acceptance/recovery | Potentially deep module at the transport/generation seam, with coordinated tools/core/session changes | Defer: requires an independently reviewed contract across success, protocol failure, disconnect, cancellation, and late completion; a happy-path patch would overclaim |
| Reject all MCP in durable sessions before dispatch | Early honest error | Safe but removes useful local discovery and delivers no capability improvement | Reject as the main outcome |

## Proposed module/interface

The module is the existing progressive MCP controller. Its interface gains a local metadata projection exposed to models as `mcp_list(query?: string)`. The new tool is an adapter at the existing `ToolDefinition` seam. It has no server-activation argument, executor callback, arbitrary registry dependency, or transport dependency. Do not introduce a hypothetical generic local-MCP completion interface.

The implementation:

- Register `mcp_list` through the existing host-only `TrustedInlineControlRegistrar`; retain ordinary registration and unknown effects for `mcp_discover` and all remote tools.
- Accept only `query`, with `additionalProperties: false`; return sorted JSON containing only names and disclosure states. Explicitly omit all diagnostics, including the existing controller's `Error` field. Command, URL, environment, headers, credentials, and remote tool output do not enter this projection.
- Check cancellation before accessing the controller and before publishing its output; reject a missing controller cleanly. Use the same local status function as `/mcp list` and the existing discovery response. No new goroutine or background worker is created by listing.
- Emit a `tool.call` span with tool name, success, and result count using the established trace logger. Do not log server configuration or arbitrary query text.
- Clarify both tool descriptions: `mcp_list` is metadata-only; existing `mcp_discover(server)` activates a configured server; remote calls keep normal approval. Documentation and test reporting explicitly state that durable activation/remote completion remains unsupported in this iteration.

The inline proof covers the synchronous listing invocation and its reviewed synchronous execution wrappers. It does not certify completion of any connection, activation, remote request, or watcher. Opaque wrappers and arbitrary tools with the same name must not gain a proof.

## Executable acceptance criteria

Tests are written first; demonstrate the focused red failure before implementation. Suggested new file: `internal/cli/progressive_mcp_listing_test.go`. Reuse real Assembly and existing loopback protocol/provider fixtures rather than adding a production test seam.

| AC | Executable observation |
| --- | --- |
| AC-1: no connection | Lazy Assembly advertises `mcp_list`; execute it with one/multiple configured servers and a query; the unit controller also covers zero servers; response is deterministic sorted names/states with all diagnostics omitted, including injected raw failure canaries; loopback MCP peer request/list/call counts stay zero |
| AC-2: concrete proof | Prepare/dispatch the assembled `mcp_list` through the real `ToolCallPreparer`; effect is `control`, identity is the host control registration, `Done` is closed after dispatch, proof is `inline_pipeline_joined`; ordinary `mcp_discover` retains unknown effects and no completion proof |
| AC-3: no activation argument | Prepared schema rejects `server`, other unexpected fields, and non-string query before executor/peer activity; invalid arguments cannot turn listing into activation |
| AC-4: cancellation | An already canceled context returns an error matching `context.Canceled`, publishes no status output, and produces no MCP request or owner generation; missing controller fails cleanly |
| AC-5: durable continuity | Through actual Assembly, local HTTP provider requests, Harness, and durable sink, run two submissions that invoke `mcp_list` and then finish; physical session is unchanged; both terminal checkpoints publish; each exact call has one joined positive completion record; MCP peer counts remain zero |
| AC-6: unchanged safety | Existing lazy activation/schema refresh and cross-run generation tests still pass; a same-name ordinary registration/custom implementation does not inherit the host completion declaration; remote tools retain unknown effects and their existing exact dispatch lease |
| AC-7: observability | Captured structured trace has one completed `tool.call` span per listing with tool name/success/result count, and does not contain fixture URL, command/env/header values, or query text |
| AC-8: real Sol journey | Fresh task-owned workspace: real Sol invokes `mcp_list`, consumes safe output, answers, and accepts a second request. Inspect actual provider catalog, transcript, positive durable receipts and continued physical session. Record activation/remote MCP as unimplemented rather than silently skipping or relabeling it |

Targeted checks (root schedules them to avoid overlapping heavy Go jobs):

```sh
go test -race -count=1 ./internal/cli -run 'TestProgressiveMCPList|TestDurableMCPList|TestProgressiveMCPActivationRefreshesNextRequestSchema|TestReviewLazyMCPRemainsCallableAcrossOwnedRuns'
go test -race -count=1 ./internal/tools -run 'TestHostInlineControlProof|TestPreparedToolSearch|TestPrepared.*DispatchLease'
make verify
```

The exact test regex is updated to the implemented test names; a command that selects zero tests is not acceptance evidence. Full verification and real Sol replay remain required after the focused suite passes. Tests and live preparation own fresh temporary stores/authority IDs and never inspect or rewrite existing user credentials or personal session stores.

## Primary protocol evidence for the remaining remote MCP work

The currently implemented wire profile is MCP `2025-06-18`. The following protocol claims use that fixed version rather than assuming compatibility with a newer specification.

- A matching `tools/call` response represents that request's result. `isError` distinguishes a tool execution error from a JSON-RPC protocol error. The protocol does not attest that a server process has exited or that all unrelated background work has stopped. Accepting a validated result as the terminal outcome of this invocation is a client contract inference; it must not become an assertion about every external consequence. See [official Tools specification](https://modelcontextprotocol.io/specification/2025-06-18/server/tools#calling-tools).
- Cancellation is optional and can be ignored when work cannot be cancelled. A sender is advised to ignore a later response. A successful cancellation notification therefore cannot become a remote terminal receipt. See [official Cancellation specification](https://modelcontextprotocol.io/specification/2025-06-18/basic/utilities/cancellation#behavior-requirements).
- Streamable HTTP allows notifications before a response and separate notification streams. Stream disconnection does not imply cancellation; connection/session lifecycle extends beyond individual requests. See [official Transports specification](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports#sending-messages-to-the-server).

## Remaining remote MCP work

Future design must specify a small interface at the existing generation/transport seam that reports the exact request identity, matching response or pre-dispatch rejection, local operation completion, and any pending owner resource. A parsed response may establish the tool's remote outcome, while local join and connection retirement are separate facts. The connection stays owned by Assembly across successful runs. Cancellation notifications do not prove the remote action stopped; malformed envelopes, transport failures after possible dispatch, and ignored cancellation keep unknown outcome. Generation leases must remain held until the exact owned operation joins, including late completion. Replay/recovery must accept only the new reviewed proof shape and must never retry an ambiguous side effect automatically.

The current missing pieces are concrete:

| Existing module / seam | Existing behavior | Required contract before durable remote support |
| --- | --- | --- |
| `MCPClient.CallTool` | Returns result/error; custom implementations are interchangeable | First-party adapters must produce an opaque receipt bound to request ID, client generation, matching validated response and exact local operation. An arbitrary implementation or embedded interface cannot claim that authority |
| `MCPClientGeneration` and tools generation handle | Pin client/lease but carry no completion attestation | Keep ordinary handles unproven. A separate explicit host declaration must retain the reviewed receipt producer; never inspect tool names to assign proof |
| HTTP request / stdio owner | HTTP joins request/body/generation activity; stdio pending errors expose an owned child join channel | Model per-request completion independently of the long-lived connection/process. On pending local cleanup, retain the exact resource channel and generation lease until actual completion |
| `preparedCompletionState` | Three positive final proofs plus private dispatch modes | Add a request-specific mode and a final proof only after a definite request terminal receipt and joined local pipeline. Failed/ambiguous remote requests cannot inherit inline or process-group proof |
| Core durable consumer | Any accepted positive completion currently becomes `ResolutionLocalResourcesStopped` in `resolvePreparedTask` | Audit and explicitly select remote-terminal evidence. A local HTTP join with unknown remote outcome must not clear the workspace fence. See [current resolver](../../../internal/core/run_commit_consumer.go#L716) and [resolution semantics](../../../internal/core/execution_resolution.go#L3) |
| Durable journal / recovery | Proof enums are checked by core and session; action references bind receipt/execution/resolution | Coordinate all closed-enum validators and recovery validation for the new proof and exact action evidence; malformed/replayed evidence remains rejected |
| Notifications | Long-lived controller watcher marks a catalog stale | Limit notifications to catalog invalidation. They must not dispatch a tool, run shell code, refresh schemas automatically, or change a run's pinned executable definitions. The connection owner closes/joins the watcher separately at refresh/Assembly shutdown |

The first remote implementation should exercise a three-state distinction: definite terminal response plus local join; unknown remote outcome plus local join; local request still pending. Only the first state receives positive remote completion evidence. A pre-dispatch rejection can use existing never-dispatched evidence only when the transport proves no request was sent; it must not be inferred from the absence of a response. A notification-only or disconnected stream is not a terminal response.

This is why this iteration does not convert remote tools or the existing activation tool to `control`, does not infer trust from a tool name or interface embedding, and does not manufacture `inline_pipeline_joined` from a normal result.

## Local listing implementation and focused validation

The implemented source is `internal/cli/progressive_mcp.go` with permanent regression tests in `internal/cli/progressive_mcp_listing_test.go`. The fixture uses a fresh owned project, real captured source grant in an in-memory trust manager, `LoadTrustedWithManager`, and Assembly. All exported execution/storage settings are replaced by the explicit fixture configuration while preserving the loader's sealed project provenance. HOME is not reset; existing personal credential/session/trust content is neither printed nor modified. Only this fixture's unique workspace-quarantine authority files are created and removed.

TDD evidence is retained outside the repository under the task root `/private/var/folders/_l/f743070s7yj3qk47q2krwqt00000gn/T/go-cli-sol-20261025.b216amge`:

| Evidence | Result |
| --- | --- |
| `mcp-list-red-trusted.log` | Expected red: actual prepared lookup reported missing `mcp_list`; actual provider call was rejected because `mcp_list` was not disclosed |
| `mcp-list-default-final.jsonl` | Default race: CLI 5 top-level tests and tools 11 top-level tests; 24 passing cases including subtests, zero failures |
| `mcp-list-headless-final.jsonl` | `headless` race: same 16 top-level tests / 24 cases, zero failures |

The final selection includes all three new listing tests, existing activation/schema refresh and cross-run generation tests, explicit host-inline proof, synchronous/custom search completion, and all prepared dispatch-lease regressions. No temporary Go helper remains. Full `make verify`, E2E tags and real Sol UI acceptance are pending root scheduling and are not implied by these focused results.

Frozen production source SHA-256: `5598738621431f285c8a73d52e82b4e574bc0fb6b66a73884f98917c075d7075`.
Frozen test source SHA-256: `026cad4e256f60ffd4faf87a915e6b607e065f5ee4f4b19a3d1455f968e6ad7e`.

## Follow-up decision: reject unsupported durable dispatch before intent

Independent review found a remaining reliability problem: the old mixed activation tool is still advertised, so a model can activate a connection and watcher before the durable consumer discovers that no completion proof exists. A description is insufficient. The selected follow-up is a generic sealed prepared-completion preflight. It does not grant any completion proof or special case MCP names.

The proposed tools interface inspects an exact sealed prepared invocation, with the dispatch mode expressed by the same attested stream sink the caller would pass to `DispatchStreaming`. It checks the captured completion mode after wrapper suppression; an ordinary `Dispatch` of streaming Bash remains supported, while streaming dispatch additionally requires the exact captured host-attested sink. The preflight returns support information and current context/trust/fence errors without consuming the permit or invoking the executor. An opaque invocation wrapper cannot inherit support by embedding a valid invocation.

Core calls this interface from both branches of `runCommitConsumer.prepareInvocation`, before `actionIntent` or `beginIntentBatch`. A missing supported completion mode becomes a typed, nonfatal `ErrRunCommitCompletionUnsupported`. The existing known-receipt path writes `not_dispatched/error`, invokes the model again, and permits another user submission. This applies to every unsupported durable tool, including activation and remote MCP calls; non-durable execution retains its prior behavior. Cancellation and current authority/fence errors take precedence over an unsupported-capability diagnosis.

Positive support only means that this captured dispatch has a reviewed way to establish completion. A wrapper panic, unknown process containment, or missing actual report after dispatch still produces no final proof and must retain the existing unknown/fenced behavior. The existing three proof enum values and durable recovery rules are unchanged.

Additional executable acceptance criteria, written before implementation:

| AC | Executable observation |
| --- | --- |
| AC-9: sealed support | An actual safe inline prepared invocation has support while its pre-dispatch `Done` and `Proof` are empty; all existing reviewed modes are recognized; empty/invalid modes, typed nil, ordinary same-name registrations and opaque embedded invocations are rejected |
| AC-10: streaming mode | Normal prepared Bash supports ordinary dispatch; streaming preparation supports only its exact captured host-attested sink; a preparer that ignores the supplied sink cannot make plain preparation eligible for streaming |
| AC-11: final wrapper capture | A non-attested execution wrapper suppresses support. The same wrapped registration with the explicit synchronous host contract is supported, but an actual dispatch panic still has no completion proof |
| AC-12: preflight ordering | Already cancelled context, revoked project binding and changed dispatch fence preserve their own errors; preflight causes no dispatch commit or executor side effect |
| AC-13: sequential/parallel continuity | Both durable execution modes reject unsupported proposals as known `not_dispatched/error`, reserve no action intent for them, dispatch no executor, request the next model response, publish a normal terminal checkpoint and accept a second submission |
| AC-14: actual MCP lifecycle | Through the real project grant/loader/Assembly/provider/durable chain, propose `mcp_discover(server)` and an activated remote MCP tool; unsupported proposals do not create/connect/call resources. Verify zero additional peer requests, no new connection owner/watcher, known error receipts and continued physical session |
| AC-15: retain post-dispatch uncertainty tests | Existing tests for action dispatch panic, late cleanup, local completion without positive proof and restart fences keep their assertions. Tests whose former contract deliberately dispatched an entirely unproven ordinary registration are updated to assert the new pre-dispatch rejection; they do not replace post-dispatch uncertainty coverage |

The prior source freeze and 16-test focused result above refer only to the completed local-listing step. This follow-up requires a new source manifest and focused core/tools/CLI checks in default and headless builds before root schedules full verification and real Sol replay.

## Final preflight implementation and focused results

The follow-up is implemented. [PreparedCompletionSupported](../../../internal/tools/prepared_completion.go#L20) checks only the exact sealed prepared types, current context/trust/fence and the captured final dispatch mode. Queued completion requires a scheduler and cannot be used for streaming dispatch. Streaming dispatch requires the exact captured attested sink. [The durable consumer](../../../internal/core/run_commit_consumer.go#L618) uses it before reserving an action and keeps unsupported completion outside the existing fatal preparation-error set. No proof enum, effect grant, connection-owner behavior or recovery acceptance rule was relaxed.

Focused race verification on the final source passed in both build configurations:

| Build / evidence at the task root | Passing top-level tests | Passing cases including subtests | Failures |
| --- | ---: | ---: | ---: |
| Default: `completion-preflight-broad-default-final2.jsonl` | 71 | 116 | 0 |
| Default additional consumers: `completion-preflight-extra-default-final.jsonl` | 18 | 24 | 0 |
| Headless, the union of the same selections: `completion-preflight-broad-headless-final.jsonl` | 89 | 140 | 0 |

The default selections do not overlap. The combined suite covers core 71 top-level tests / 103 cases, CLI 11 / 21, tools 4 / 13, and approval 3 / 3. It includes real durable MCP activation/remote rejection in sequential and parallel modes, subsequent local listing and another submission on the same physical session, zero new connection/call/watcher activity, safe listing privacy, unsupported same-name and opaque invocations, exact streaming-sink identity, wrapper suppression, cancellation and current authority precedence, post-dispatch panic, late cleanup, restart fences, compaction/overflow, active loop detection, approval continuity, and the existing positive durable project Skill/Hook journey.

The real Sol/UI and full-gate conclusions belong to the root iteration report. These focused results do not claim a positive durable remote MCP call.

### Preserved red evidence and fixture contract changes

- `completion-preflight-core-red.log` reproduced unsupported ordinary actions being executed before quiescence failure in both sequential and parallel loops, and a missing preparation-time current-generation check.
- `completion-preflight-cli-red.log` reproduced all four actual Assembly journeys failing after unsupported activation/remote execution: sequential/parallel × activation/already explicitly activated remote tool.
- `completion-preflight-broad-default.jsonl` retained two old project-fixture failures. During Ask, preflight now detects the actual cross-process revocation before an intent is reserved; the regression now requires zero intents, one known not-dispatched receipt, and the typed stale-authority error rather than an unsupported-capability diagnosis. The former after-dispatch fixture was an ordinary project tool with no completion contract, so dispatch is now correctly prohibited.
- To preserve the actual after-dispatch scenario, that fixture now uses an explicitly reviewed synchronous host controller that holds its own project binding, records admission inside a short trust transaction, then blocks outside the store lock. A real child process revokes the grant after admission. The test keeps its dispatched/success receipt and adds positive inline-join assertions, while a current-binding run guard prevents another provider request: model call count stays one and the run ends with the typed stale error. Reopening checks immutable records and resource completion, and does not restore the old grant. This fixture does not claim to test registry-level project-source final-dispatch linearization. That unchanged contract remains covered by [TestProjectTrustCommitWinsBeforeLateRevoke](../../../internal/approval/prepared_policy_runtime_test.go#L272) and the tools project-binding regressions.
- The dispatch-panic and live-worker cancellation regressions retain their original unknown/fenced assertions. Their old opaque invocation wrappers were replaced with real sealed preparation plus explicitly synchronous execution wrappers and a terminal observer. This preserves the post-dispatch race without allowing opaque prepared capabilities through preflight. [Panic regression](../../../internal/core/run_commit_consumer_test.go#L825), [live-worker regression](../../../internal/core/run_commit_consumer_test.go#L866), and [closed signal without proof/reopen regression](../../../internal/core/run_commit_consumer_session_test.go#L987) all passed.
- `completion-preflight-extra-default.jsonl` retained the active-loop detector fixture failure: its ordinary synchronous probe had no supported completion contract. The fixture now explicitly declares the host inline contract; all original detector/cancellation/intent assertions remain intact. The corresponding final additional-consumer suite passed.
- `completion-preflight-broad-default-final.jsonl` also preserves a test-setup failure: the new ordinary-Bash support fixture initially omitted a reviewed sandbox. It now uses the existing explicit `AllowAllSandbox` registration option and passes both ordinary-dispatch and ignored-stream-sink assertions. No production sandbox or proof rule was relaxed.

Original `make fmt` completed successfully after the source changes. SHA-256 comparison of all 1,251 tracked/untracked Go files before and after the target found no changes. Its existing Makefile logic reported that `goimports` was unavailable and skipped it. `git diff --check` passed.

### Final source freeze

All Go jobs started by this builder have ended. No temporary Go helper remains. The complete ten-file SHA-256 manifest is retained at the task root as `completion-preflight-source-manifest.json`; root was notified before starting its build/full gates/live run.

| Production source | SHA-256 |
| --- | --- |
| `internal/cli/progressive_mcp.go` | `5598738621431f285c8a73d52e82b4e574bc0fb6b66a73884f98917c075d7075` |
| `internal/tools/prepared_completion.go` | `d5ce7dd498bbd32461ced8e5912350dbbd6ed35d3c125d0d847fc6c3d16bcada` |
| `internal/core/run_commit_consumer.go` | `86af758e64e4a2650da084145a3173b668350a700868cb5087dcbbe7d7ab001d` |

Permanent test changes are limited to `internal/cli/progressive_mcp_listing_test.go`, `internal/cli/mcp_durable_preflight_test.go`, `internal/cli/loop_detector_active_integration_test.go`, `internal/tools/prepared_completion_preflight_test.go`, `internal/core/run_commit_completion_preflight_test.go`, `internal/core/run_commit_consumer_test.go`, and `internal/core/run_commit_consumer_session_test.go`.

## Full-gate follow-up: test fixtures and approval-display parity

The first full gate exposed two test-fixture problems beyond the focused set. The binary E2E helper derived a filesystem directory from `runtime.Caller`; the required `-trimpath` build turns that into a module-relative source identity. The helper now walks upward from the actual test working directory to this module's real `go.mod`, preserving `go build -race` and the existing scenario assertions. No production export or fabricated directory is introduced. The previous full-gate and isolated-failure evidence remains preserved by the gate owner.

The configured model-timeout fixture used 25 ms / 15 ms budgets and a 150 ms delayed success response. Under a full-suite load, a deadline could expire before the HTTP handler saw one request, so a request-count assertion failed without actually testing that model's dispatched request. The revised fixture retains the two real primary/small requests and both deadline assertions, records the actual model and streaming flag, emits an initial valid SSE frame for streaming setup, and then waits for real request-context cancellation. Explicit 2 s total / 500 ms stream-idle budgets, per-call lower/upper elapsed bounds and an 8 s outer guard distinguish the intended configured timeout from default or parent timeouts. An idle-specific error cause is required for the primary stream; the small model must report its configured total. The fixture never manufactures a success response on a timer and observation sends cannot block on unexpected extra requests.

Root additionally authorized a CLI Assembly-only approval parity change after actual UI evaluation found that the same `--approve ask` had different Bash policies depending on whether an approval channel existed. The modal path installed `AuditResolver(DefaultPermissionModeResolver)` and therefore asked for Bash, while the stdio path retained a static classifier that forbade Bash outright. Callback transport must not select a different default policy.

Selected scope:

- Build the Assembly's default Ask base classifier from the same `DefaultPermissionModeResolver` used for prepared decisions, and install its `AuditResolver` for Ask regardless of presentation.
- Let channel presence choose only Tea versus stdio approval callback. Auto and Deny keep their explicit branches. There is no new classifier/resolver option and no change to `NewApprovalMiddleware` defaults or explicit caller-supplied policy behavior.
- Ask still requires an actual affirmative human choice before Bash dispatch. No, EOF, cancellation, missing input and non-interactive input retain zero execution and no positive grant. Existing plan/trust/cache/fence/completion-preflight rules remain in force.

Additional executable AC:

| AC | Observation |
| --- | --- |
| AC-16: actual Ask display parity | Real Assembly prepared Bash with Tea channel and with an owned PTY stdin both reach the same Ask proposal, wait before any marker side effect, execute only after yes, and deny after no |
| AC-17: missing/cancelled input | EOF/non-TTY input and cancelled requests never start Bash; a pending prompt followed by cancellation and a late always-allow input joins without dispatch; the same call in a fresh context asks again, with no cache/store grant |
| AC-18: explicit modes and policy | Auto and Deny behave identically with/without an offered modal channel; the unused channel stays empty. Explicit direct classifier Deny and explicit resolver Deny remain authoritative in lower-level prepared middleware |
| AC-19: audit parity | Actual yes/no decisions record `safety_policy`, default mode and real user provenance consistently across Tea/stdin; EOF/cancellation retain their own bounded sources and cannot be reported as a user denial |
| AC-20: both build configurations | Default/headless targeted approval/timeout tests and the prior full-gate failure selection pass under the original race/trimpath flags and umask 022. Permission tests are unchanged |

This is a new production candidate because Assembly changes. Earlier live results remain attributed to their earlier binary SHA; root will rebuild and repeat the affected actual UI/stdio approval journeys on the new bits before reporting parity acceptance.


### Approval implementation and evidence boundaries

The production change is limited to [Assembly policy selection](../../../internal/cli/assemble.go#L1358) and the default [stdio callback](../../../internal/approval/callback.go#L77). Both default Ask displays now use the same resolver and audited proposal; channel presence selects presentation only. The callback checks cancellation at entry and immediately after each read, before accepting any choice or classifying a read error. No generic custom callback, publisher, trust binding, dispatch fence, proof mode or Auto/Deny rule changes.

The default callback performs an ordinary blocking read. Cancellation does not interrupt that read. Its caller/input owner must release the read and join the call; the PTY regression explicitly sends a late `a` after cancellation to release the owned input, waits for return, and then verifies a fresh request with the same key prompts and denies. No detached reader or ownership transfer is introduced. An affirmative choice accepted before cancellation retains the existing authorization linearization; a cancellation already observed when the read returns rejects the choice.

The new [display parity test](../../../internal/cli/approval_display_parity_test.go#L41) exercises actual prepared Bash dispatch and audit with a real PTY or Tea callback channel. Its append-only marker proves exactly one execution after yes and zero after no/EOF/cancel. It does not attach a durable sink and therefore does not claim canonical durable receipts or physical-session continuity. The existing [durable approval recovery regression](../../../internal/cli/durable_approval_recovery_test.go#L58) covers known receipts and subsequent submission with the real runtime plan; root additionally repeats affected UI/stdin same-session journeys on the new release binary. The [lower callback regression](../../../internal/approval/callback_context_test.go#L48) directly checks zero cache/store grants after a cancellation occurring during the read.

TDD red evidence `approval-display-parity-red.jsonl` captured the prior stdio hard deny, mismatched base classifier and cancellation accepting a late always-allow. The unrelated positive Tea fixture initially used only the owned directory in the Bash sandbox whitelist while the real Bash cwd was the package test cwd; it now includes that actual cwd and the owned marker directory. This is a fixture correction with sandbox checks retained. The fixture uses `>>` and exact marker bytes to detect repeated execution.


Focused results before the final root gate, all under `-race -count=1`, `GOFLAGS='-trimpath -p=2'`, owned cache/temp paths and child umask 022:

| Evidence under the task root | Result |
| --- | --- |
| `approval-display-parity-green1.jsonl` | New display/callback selection: 19 cases passed, zero failures |
| `approval-display-parity-broad-default.jsonl` | Related default approval, cache, policy, timeout and durable recovery: 120 top-level tests / 181 cases; 180 passed, one existing skip, zero failures |
| `approval-display-parity-broad-headless.jsonl` | Same headless selection and counts; zero failures; includes the final explicit cache/store zero-grant assertions |
| `approval-parity-final-failed10-extra-default.jsonl` | All original full-gate failure functions plus three callback/source checks on the new source: 13 top-level tests / 23 passing cases, zero failures |
| `approval-parity-extra-headless.jsonl` | Two remaining Tea source/cancellation checks pass in headless |

The earlier test-only release rebuild `bin/go-cli-testonly-check` retained SHA-256 `a6efefa788fee9cc84d2066d90b84fb6e8859200c0387e847bb4e405566e5912`. That equality establishes the two fixture repairs alone did not change the previous candidate's production bits. The subsequent Assembly/callback changes require a new binary and new live attribution; no previous model matrix is credited to that new candidate.


Final Go source/test freeze: 1,253 Go files. Original `make fmt` passed and changed no Go files; the original target reported goimports unavailable and skipped it. `git diff --check` passes. The previous ten-file MCP/preflight manifest remains byte-for-byte unchanged. The added production files are `assemble.go` and `callback.go`; both require new release attribution. The task-root `approval-parity-final-source-manifest.json` records all 16 owned changed Go files, and `approval-parity-final-allgo.json` records the whole tree (JSON SHA-256 `711bac90cdc05770ada6481be889101cba6a0bf1f261aaca750eedb11d27b9f4`). The canonical sorted `sha256  relative-path\n` file is `approval-parity-final-allgo.sha256` (SHA-256 `246a891cd821912d37a66113906a74b3ed90748bdc71d7408fd4f45a3713369d`). All owned Go tasks have ended before handing the frozen tree to the independent gate owner. Full verification and new-candidate live results are reported separately by root.
