# MCP durable completion: independent contract review

Date: 2026-10-25. Role: independent Designer/Reviewer. Baseline: `379134d` on `codex/sol-runtime-2026-10-25`. This review reads source and primary protocol documents; it does not run a model, inspect credentials or personal sessions, or change protected infrastructure.

## Decision

The frozen `mcp_list` implementation in `internal/cli/progressive_mcp.go` (SHA-256 `5598738621431f285c8a73d52e82b4e574bc0fb6b66a73884f98917c075d7075`) has no identified blocking defect for its stated local metadata operation. This is a source review, not an independent execution of the builder's tests.

It is unsafe to present the accompanying `mcp_discover` and remote tools as usable durable operations while leaving completion rejection until after dispatch. Activation can already create an owned connection, publish registrations, and start a watcher before `resolvePreparedTask` discovers that it has no proof. The required immediate correction is a generic, sealed, pre-dispatch completion-support check in the durable consumer. Unsupported calls must produce an ordinary known `not_dispatched/error` receipt so that the model can respond and the same physical session can accept another submission.

Full remote completion is feasible, and the implementation route below is concrete. It requires typed request-terminal evidence and changes to the durable record/recovery contract, not merely another accepted completion enum. The existing `ResolutionRemoteTerminal` is the right reserved seam. Both the live sink and recovery deliberately reject it today. A small preflight is justified by that contract gap and the need to preserve fail-closed behavior, rather than by a desire to avoid implementation or testing.

## Primary sources and their scope

- [MCP tools, 2025-06-18](https://modelcontextprotocol.io/specification/2025-06-18/server/tools): `tools/call` returns its result in the matching JSON-RPC response. `isError: true` describes a tool execution error; a JSON-RPC error describes an invocation/protocol error. Neither should be confused with an absent response. Tool annotations from an untrusted server cannot grant host authority.
- [MCP canonical schema, pinned 2025-06-18](https://raw.githubusercontent.com/modelcontextprotocol/modelcontextprotocol/2025-06-18/schema/2025-06-18/schema.ts): `CallToolResult` has a required content array and optional boolean `isError`. This supports method-specific validation, rather than treating any result object as a valid call result.
- [JSON-RPC 2.0 response contract](https://www.jsonrpc.org/specification#response_object): a response identifies the request by its ID and contains exactly one of result or error. An error is a response, not proof that no side effect occurred.
- [MCP cancellation, 2025-06-18](https://modelcontextprotocol.io/specification/2025-06-18/basic/utilities/cancellation): cancellation is optional, can be ignored, and can race completion. A response arriving after cancellation should generally be ignored. A successful cancellation notification is not a remote-stop acknowledgement.
- [MCP transport, 2025-06-18](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports): HTTP streams may disconnect without cancelling the remote request. A request response may arrive through SSE; reconnection can resume a stream, rather than reissue an uncertain mutating call. A long-lived transport/session is independent of one request.

The repository negotiates `2025-06-18` in [the HTTP adapter](../../../internal/mcp/http_adapter.go). This review intentionally applies that version, rather than inferring behavior from future protocol revisions. The interpretation that a validated response establishes this request's protocol outcome, while making no universal claim about every remote background effect, is a design inference from these protocol contracts.

## Local listing review

The host registers a new concrete `mcpListTool` using `RegisterTrustedInlineControl`. Its schema accepts only optional string `query`, with no activation field. `Execute` calls the controller's local `List`, explicitly projects only `name` and `state`, marshals the bounded configuration list synchronously, and checks cancellation before and after the read. Diagnostic values are not projected. Query text and connection/configuration values are absent from the added trace/log attributes. See [progressive MCP](../../../internal/cli/progressive_mcp.go) and [listing tests](../../../internal/cli/progressive_mcp_listing_test.go).

`List` shares the controller mutex with activation. It can therefore wait behind an already running activation. This is a possible latency limitation; it does not cause the listing invocation to activate a server or make its synchronous completion proof false. The post-read context check preserves cancellation after a lock wait.

Ordinary registration replaces completion metadata with unknown effects and empty completion authority, even when the replacement has the same name and concrete type. The final prepared executor also suppresses completion for unattested wrappers. Those are the relevant protections against proof inheritance: [ordinary registration](../../../internal/tools/mcp_generation.go), [trusted registration](../../../internal/tools/trusted_registry.go), and [final prepared capture](../../../internal/tools/prepared_registry.go). Trust revocation and generation changes remain subject to the existing final dispatch fence; listing does not acquire remote authority from its name.

The limitation is the surrounding catalog: `mcp_discover` still mixes a metadata response with activation. It is not eligible for the inline listing declaration. Tool descriptions that invite the model to activate are not enforcement.

## Existing implementation facts

| Layer | Existing positive fact | Missing fact for durable MCP |
| --- | --- | --- |
| `mcp/request_boundary.go` | Strict JSON-RPC envelope/ID validation; a valid RPC error is distinguished from ambiguous dispatch | A sealed, action-scoped observation carrying that identity through the generation adapter |
| `mcp/http_adapter.go` | `request` tracks the exact connection generation's active work; closes response bodies and finishes its activity before returning; cancellation notification is awaited | Method-specific result validation and exported sealed evidence; a local join signal is not remote-terminal authority |
| `mcp/adapter.go` | Stdio request serializes matching response reads; owned I/O cancellation has a joinable process owner; `tools/call` is not blindly retried | A request receipt independent of the long-lived stdio process owner; unknown cancellation must stay unknown even after process cleanup |
| `mcp/tool_adapter.go` | `MCPClientGeneration` pins a source-owned client, supports acquire/retire/drain, and forwards through the exact handle | Completion evidence that arbitrary `MCPClient` implementations cannot forge by returning a non-nil result |
| `tools/mcp_generation.go` | Frozen schemas, exact client and lease, reserved names, project trust binding; effects default to unknown | An explicitly reviewed completion mode for receipt-capable generations only |
| `tools/prepared_completion.go` / `prepared.go` | Sealed action-scoped signal; closed channel alone is insufficient; lease can transfer to final resource completion | A mode that combines accepted protocol outcome, local pipeline return and exact operation join without granting an effect |
| `core/run_commit_consumer.go` / `run_owner.go` | Reservations precede dispatch; exact completion and receipt are durable before resolution; late cleanup is owner-managed | A remote resolution path, including late owner drainage, which must not reuse `ResolutionLocalResourcesStopped` |
| `session/run_commit.go` / `durable_recovery.go` | Exact occurrence identity, sequence/order checks, immutable receipts, cross-session workspace authority | Supported typed protocol-terminal records and validation; `ResolutionRemoteTerminal` is explicitly rejected today |

Relevant source: [request classification](../../../internal/mcp/request_boundary.go), [HTTP ownership](../../../internal/mcp/http_adapter.go), [stdio transport](../../../internal/mcp/adapter.go), [client generation](../../../internal/mcp/tool_adapter.go), [candidate publication](../../../internal/tools/mcp_generation.go), [completion state](../../../internal/tools/prepared_completion.go), [dispatch](../../../internal/tools/prepared.go), [consumer](../../../internal/core/run_commit_consumer.go), [run owner](../../../internal/core/run_owner.go), [durable sink](../../../internal/session/run_commit.go), and [recovery](../../../internal/session/durable_recovery.go).

### Current result parsing is not sufficient authority

Both transport adapters currently map `res["content"]` and `boolVal(res["isError"])` into `MCPToolResult`, substituting an empty string for missing content. Consequently `err == nil`, a non-nil `MCPToolResult`, and a matching envelope are not yet a validated standard `CallToolResult`. Full completion must add a strict method-result validator before minting terminal evidence. Compatibility result mapping may remain available for legacy non-durable use, but cannot silently grant completion authority.

## Small immediate implementation: durable preflight

Add an inspector in `tools`, where the private prepared types and final captured completion mode are visible. This is a query about a supported future completion path, not a new proof and not a guarantee that execution will succeed. Inspect only the package's sealed concrete invocation; an embedded/opaque invocation wrapper fails closed.

The check recognizes reviewed public proof modes and the two private hybrid process/search modes. It must use the final captured value after execution-wrapper suppression and queue selection. It must not infer support from tool name, effect labels, `Done`, a prematurely closed channel, or `Proof()`: even a legitimate inline invocation has no completed proof before execution.

There are two dispatch paths. Ordinary `Dispatch` may legitimately execute a `preparedStreamingInvocation` without a stream sink. `DispatchStreaming` clears its effective proof when its captured attested sink is nil. The support inspector therefore needs the requested dispatch mode, or separate ordinary/streaming inspectors. For streaming, verify the captured attested sink as well as the mode. A custom streaming preparer that ignores the supplied sink and returns a normal prepared streaming invocation must fail before dispatch.

The selected implementation uses `tools.PreparedCompletionSupported(ctx, invocation, streamSink) (bool, error)`: nil sink selects ordinary dispatch; a non-nil sink must match the sealed invocation's captured host-attested sink. A queued completion additionally requires a captured scheduler and cannot support `DispatchStreaming`, which does not execute that scheduler. Current cancellation, project trust and generation-fence errors precede a support rejection; this check does not consume the permit or replace dispatch's final authority check.

Call the inspector in `runCommitConsumer.prepareInvocation` for both the streaming and ordinary return paths, before `BeginIntentBatch`, task reservation, lease acquisition, or executor entry. A new ordinary preparation error must use the existing known-receipt path and must not join the consumer's fatal-error list. Sequential and parallel callers already support nonfatal preparation failures. Existing context, project/fence and capability failures retain their current precedence and semantics.

This applies generically to currently unsupported completion paths, including mixed activation and remote calls. It does not lower default unknown effects, claim that all MCP is control, or alter the behavior of an operation already dispatched. Existing forensic tests that intentionally execute an unsupported custom invocation need to exercise the lower dispatch/outcome seam directly, or give the fixture a real supported completion path if that is what the test claims. They must not be weakened to expect fewer unknown protections after dispatch.

The reviewed fixture migrations preserve this distinction. Dispatch-panic coverage uses a real sealed prepared invocation with a synchronous host wrapper that panics after calling its executor; it still requires dispatched/unknown state, no resolution and no later provider request. The parallel-cancellation race keeps the sealed executor active through a synchronous wrapper and delays its return through the terminal observer, preserving the live-worker result-slot race. Project revocation during approval now produces a known rejection before intent reservation. For late revocation, a host-declared synchronous fixture tests immutable committed receipts while holding its own verified binding; its current-project guard rejects a later provider request after revocation while preserving the already admitted action's receipt. The fixture asserts its host registration identity and actual inline completion, and does not claim to be an ordinary project-sourced tool that gained completion authority. The original exact project-registration dispatch linearization remains exercised by `TestProjectTrustCommitWinsBeforeLateRevoke` in [approval runtime tests](../../../internal/approval/prepared_policy_runtime_test.go), which uses real file-store trust generations and the prepared dispatcher directly.

The catalog should accurately communicate which operations are supported. At minimum, an explicit model-selected unsupported call must return a useful nonfatal error before execution; optionally hide known unsupported entries using the same frozen support metadata. Catalog filtering alone is insufficient because a model can emit a remembered tool name. A successful listing is not a successful activation or remote-call evaluation.

## Full remote implementation: minimum safe contract

### 1. Define the exact claim

The new proof should mean: the captured request received a validated terminal protocol response, the captured local request operation joined, and the complete prepared executor pipeline returned. It must not mean that the connection ended, the server process exited, arbitrary server-spawned background jobs stopped, or the remote world was rolled back.

A successful `CallToolResult`, `isError: true`, and a valid matching JSON-RPC error each establish a protocol response outcome. A tool can report a failed operation after a partial mutation; its recorded outcome is error, not never-dispatched. The host still keeps remote effects unknown for authorization. A dishonest server can lie in a valid result; a protocol receipt provides the same response-trust premise as any RPC, not independent verification of business effects.

This claim can support `ResolutionRemoteTerminal` if the durable contract explicitly defines it as resolution of that protocol request. If project policy instead requires physical containment of all effects of a server that can mutate the workspace, a standard MCP response cannot satisfy that stronger policy. Such a server must remain gated until it has a separately reviewed containment/reconciliation capability. Do not silently change the meaning of `ResolutionLocalResourcesStopped` to bridge the two policies.

### 2. Put the interface at the transport/generation seam

Introduce an optional receipt-capable request interface implemented by the reviewed HTTP and owned-stdio adapters. Keep the existing `MCPClient` compatibility interface unchanged. The transport owns an opaque operation observation, with private construction authority, containing:

- A fresh connection-generation token, request ID, negotiated protocol and method, plus a digest binding the requested remote tool and normalized arguments. Request ID alone can be reused by another connection.
- Dispatch state: definitely not sent, possibly sent, or terminal response observed. Do not equate the prepared dispatch commit with network transmission.
- A terminal classification: validated call result success, validated call result error, or validated JSON-RPC error. Store digests and fixed enums, not endpoint credentials or full remote content, in authority records.
- An exact request-operation join signal, including response body/readers/cancellation callback and notification work belonging to that operation. Its closure may occur without terminal evidence.
- A cancellation terminal choice that cannot be rehabilitated by a late post-cancel response. No mutable global “last response” slot.

`MCPClientGeneration` captures this interface on construction and returns the observation with the tool result through an explicit completion-aware execution method. Receipt authority must be opaque/sealed and validated by the receiving module. Any legacy/fake/custom client which only implements `CallTool` continues with empty completion support. Do not export an unrestricted “report arbitrary remote proof” context helper.

The small external interface should hide HTTP/stdio lifecycle detail. Tests and the normal caller must cross this same interface. Internal HTTP and stdio adapters provide the real variation; a new abstraction solely for one hard-coded tool name would add no depth.

### 3. Integrate exact leases and local joins

The candidate stores a new reviewed mode only if its captured client supports this receipt interface. Unknown effects remain unchanged. Prepared dispatch starts its action-scoped signal and holds the existing exact-generation lease until the request operation and pipeline join. On a pending error, lease release follows the exact operation's eventual join, not the first returned error. A nil operation join signal remains unproved.

HTTP success can normally join synchronously after `closeResponseBody` and activity release. The Assembly-owned connection can remain open between runs. Stdio success joins the request I/O and cancellation watcher while its server process remains Assembly-owned. A cancelled/stopping stdio owner can require the existing process-owner join. Neither closing the connection nor killing its local subprocess proves the remote request's business outcome.

On ambiguous outcome, local join can release memory/lease ownership once observed, but cannot grant the remote-terminal proof or clear its durable unknown marker. Keep local cleanup evidence and request-outcome evidence independent. This also prevents a cancellation notification's HTTP 202 from becoming request completion authority.

### 4. Activation requires its own handoff contract

`mcp_discover(server)` is a host lifecycle operation, not a `tools/call` response. Implement it separately: connect/initialize and all `tools/list` pages finish; schemas validate; project trust and registry fences are current; candidate generation is atomically published; the lifetime connection and watcher are explicitly adopted by the Assembly owner. Only then can an action-specific activation handoff proof close. Failed construction must join candidate cleanup or remain cleanup-pending; failed publication cannot leak a live unowned candidate.

Watcher ownership is independent from the finished activation request. It may notify/mark stale until Assembly cleanup, but must not make the activation's action signal pretend that it joined. Document the allowed owner handoff in the proof's semantics and verify that watcher cleanup joins. Never use the ordinary inline proof for the current mixed method as-is.

### 5. Persist typed evidence before remote resolution

Add a typed terminal-evidence DTO to the versioned execution/checkpoint contract. At runtime only a sealed observation can mint it; on replay its persisted fields are audit facts, not newly reconstructed execution authority. It must bind the exact action occurrence, source/client generation, request identity, method/tool/argument digest and terminal classification. The associated local join proof must reference the same action and operation.

Core selects `ResolutionRemoteTerminal` for that specific proof and supplies the typed evidence. Update both the normal consumer and `runOwner.drainLateCompletionTasks`; neither may route a new remote proof through generic local-resource resolution. Session must require a matching dispatched receipt, validated typed evidence, local join, correct owner generation and sequence ordering before writing the resolution checkpoint. Replay, direct-view validation and workspace authority must enforce the same conditions. Old readers should reject an unknown proof/record shape rather than infer success.

Crash before evidence persistence leaves the intent/dispatch marker unresolved. Crash after evidence but before resolution can reconcile only from matching durable evidence. A known protocol response with failed local join stays pending. An unknown receipt must not be turned into a known success merely because local cleanup finishes later. Post-cancel reply evidence is not admissible for automatic rehabilitation.

## Fail-closed outcome matrix

| Observation | Request outcome | Local state | Durable action |
| --- | --- | --- | --- |
| Unsupported captured completion mode | Known error, not dispatched | No executor/connection started | Nonfatal known receipt; continue model and next run |
| Rejected by trust/approval/fence before dispatch | Existing denied/interrupted/error semantics | No executor | Existing never-dispatched resolution |
| Valid matching call result, local pipeline joined | Success or tool error | Request joined; connection may stay owned | Typed remote-terminal resolution |
| Valid matching RPC error, local pipeline joined | Known invocation error | Request joined | Typed remote-terminal resolution; no automatic retry |
| Matching envelope but invalid call-result shape | Unknown after possible dispatch | Joined or pending | No terminal proof; retain unknown marker |
| Wrong/duplicate ID, malformed JSON/envelope, oversized body | Unknown after possible dispatch | Joined or pending | No terminal proof; retain unknown marker |
| Timeout/cancel/disconnect after possible send | Unknown | Request/process may join later | Record cleanup separately; never infer remote stop |
| Cancellation notification accepted | Original request remains unknown | Notification request joined | No original-request completion authority |
| Late result after cancellation was chosen | Remains unknown | May permit local release | No receipt rewrite or automatic unquarantine |
| Resume of exact SSE request returns valid response before cancellation | Known response, if exact cursor/session/request checks pass | Join required | Same single request terminal evidence; never a repeated call |
| Connection generation retired while call pending | Original request unchanged | Exact lease remains until join | No replacement client/ID can satisfy the old action |

## Executable acceptance plan

Write failing tests first and keep transport tests free of provider credentials. Existing MCP HTTP and stdio mock peers are suitable, but completion authority must also be exercised through real adapters, exact generation publication, Assembly and the durable writer.

| Test area | Required assertions | Focused command after implementation |
| --- | --- | --- |
| Immediate preflight | Safe inline with pre-dispatch nil `Done` accepted; ordinary same-name and unknown rejected; opaque wrapper rejected; mutation queue/process/search modes accepted; streaming attested sink required only for streaming dispatch | `go test -race ./internal/tools ./internal/core -run 'PreparedCompletionSupported|DurableCompletionPreflight'` |
| Durable continuity | Sequential and parallel unsupported MCP requests yield exact known `not_dispatched/error` receipts; zero connect/list/call; no owner/watchers; second physical-session submission succeeds; current cancellation/fence behavior preserved | `go test -race ./internal/cli -run 'DurableMCPList|DurableMCPUnsupported'` |
| Transport receipts | HTTP JSON/SSE and stdio valid success/isError/RPCerror; strict result validator; wrong/duplicate IDs; case/duplicate reserved fields; oversize/malformed/truncated body | `go test -race ./internal/mcp -run 'RequestTerminal|CallToolResult'` |
| Cancellation/ownership | Delayed body close, cancellation callback, pending process owner; no early join/proof; notification accepted but original unknown; late reply ignored; disconnect before response; resumable exact SSE; generation retire waits exact lease | `go test -race ./internal/mcp ./internal/tools -run 'MCP.*Completion|MCP.*Retire|MCP.*Cancel'` |
| Full activation | Paginated list, failed connect/list/validation/publish, candidate cleanup pending, exact trust revocation, registry update failure, watcher handoff and cleanup; no partial public catalog | `go test -race ./internal/cli -run 'MCPActivationCompletion|MCPGeneration'` |
| Core publication | Success/error receipt plus evidence precedes resolution; late owner path selects remote resolution; evidence persistence failure cannot publish success; sibling calls cannot borrow evidence | `go test -race ./internal/core -run 'RemoteTerminal|MCP.*Completion'` |
| Durable validation | Roundtrip/reopen/resume; exact identity/digest/generation mismatch; cross-action/request replay; evidence after resolution; unsupported version/proof; torn tail and restart at each persistence phase | `go test -race ./internal/session -run 'RemoteTerminal|MCP.*Recovery'` |
| Full multi-turn path | Real Assembly + mock MCP HTTP/stdin peers + provider fixture: list → activate → next model catalog → call → finish → second submission; same SID; one request, one receipt/proof/resolution; connection owned until cleanup | `go test -race ./internal/cli -run 'DurableMCPRemoteRoundTrip'` |

These are proposed executable test names for new work, not claims that these commands already match implemented tests. Root owns final `make verify`, headless variants where required, and real MagpieSol evaluation. Real evaluation should cover both a positive local listing and an explicitly selected unsupported activation that returns a useful error with zero server starts and permits a subsequent turn.

## Scope and implementation cost

The immediate preflight changes one small tools inspector and the durable consumer's shared preparation return seam, plus tests in tools/core/CLI. It does not change the durable wire format or post-dispatch unknown contract. Its broad consequence—unsupported completion modes stop being executed in durable sessions—is intentional and must be reported clearly.

Full request completion requires at least six coordinated implementation slices: method validation/receipt construction in both transports; receipt-aware exact generation execution; prepared completion/lease state; core normal and late publication; versioned session writer/recovery/authority validation; activation owner handoff. Each has adversarial tests above. A realistic first implementation is a multi-file state-machine change, not a local inline annotation. It can begin as the next explicitly scoped task with the same preflight rejecting unsupported adapters throughout development. Release full discovery → activation → call only when the end-to-end, replay and cancellation matrix passes; remove the preflight rejection only for captured generations whose complete proof path exists.

## Review status

- Local `mcp_list`: source review approved within its local metadata scope.
- Existing durable activation/remote execution: not approved for dispatch without new preflight or full contract implementation.
- Generic preflight: source and test-design review approved after the queued-streaming correction. The implementation checks the final captured mode, current authority and exact attested sink; ordinary safe calls with nil pre-dispatch signals remain valid. Real Assembly fixtures cover sequential/parallel activation and preloaded remote-call rejection, known receipts, unchanged owned MCP resources and two submissions on the same physical session. Builder-run focused tests and root-run final gates remain separate evidence; this reviewer did not independently execute Go or a model during the review phase.
- Full remote contract: implementation-ready design proposal; no completion authority has been added by this document.
