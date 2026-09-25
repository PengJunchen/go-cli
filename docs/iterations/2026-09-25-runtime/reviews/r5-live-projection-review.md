# R5 live tool projection review

**Verdict: request fixes before accepting `7f66b3d6a22f6603662a86b2256cc314ad584091`.** The scoped bridge/model path preserves normal tool-call metadata and renders sanitized arguments/output, but three boundary cases remain: unsupported custom argument code runs during cloning, `done` leaves the active association open, and duplicate terminal receipts are presented as valid children. This review covers the live TUI slice only; it does not certify durable transcript/history wiring, default CLI acceptance, or full R5 closure.

## Fixed source and review method

Reviewed frozen commit `7f66b3d6a22f6603662a86b2256cc314ad584091` in a clean detached worktree based on `e017f97`. The six changed files are `internal/tui/{accordion.go,app.go,app_extended_test.go,bridge.go,r5_live_projection_test.go,tool_status_test.go}`. I read the approved R5 handoff and followed `BridgeEvents → CoreEventToAgentEvent → teaModel.Update/View`, plus the default Loop, Harness, CLI, and EventStream producer paths. Temporary adversarial tests were removed after the probes; the review worktree is clean.

The new tests do exercise the actual bridge-to-model path. They verify pending-to-running transitions, repeated IDs across assistant groups, duplicate/empty proposal IDs staying uncorrelated, legacy and unmatched events remaining visible as incomplete, streamed final tool metadata, and that `done` does not duplicate answer text. A separate probe confirmed argument OSC 52 controls are removed and expanded tool details show stdout, stderr, and error content.

## Findings

### [P1] Reject custom argument encoders before invoking them

[`copyToolCalls`](../../../../internal/tui/bridge.go:91) calls `json.Marshal(call.Args)` before `isToolArgsJSONValue` rejects types implementing `json.Marshaler`. This invokes arbitrary encoder code on the Bridge conversion path. A probe type whose `MarshalJSON` performs an out-of-bounds slice access caused a runtime panic to propagate from `CoreEventToAgentEvent`; a side-effecting encoder was invoked once even though the result was subsequently marked `ArgsUnavailable`. `BridgeEvents` calls this conversion in its forwarding goroutine without recovery, so a runtime panic can terminate the process. The current cycle-only `TestLiveBridgeInvalidToolArgsBecomeUnavailableWithoutPanic` does not cover custom encoders.

Reject unsupported Go types before any marshal/clone operation. A bounded clone over the accepted JSON scalar/map/slice tree should produce `ArgsUnavailable` for unsupported values without invoking custom methods.

### [P2] `done` must close the active tool-call association

The `done` branch at [`app.go:1255-1260`](../../../../internal/tui/app.go:1255) clears only the streaming entry and buffer. It leaves `toolCallGroup` available to [`handleToolResultEvent`](../../../../internal/tui/app.go:1554), which then associates a same-ID event after `done` as a complete child rather than an incomplete standalone event. A Bridge-to-model probe observed `group retained=true`, `children=1`, `child incomplete=false` after sending `done` followed by a same-ID result.

This boundary is reachable without relying on a guessed future-run identity. The Loop emits a final assistant message only when content or tool calls are non-empty, but returns success on an empty final response without emitting that message (`internal/core/loop.go:655-661`); the preceding tool group therefore has no later assistant-message boundary to clear it. The CLI then sends `done` (`internal/cli/repl_session.go:718-725`). Also, `EventStreamImpl.SendTerminal` is a single terminal-event enqueue, not a send fence; ordinary `Send` remains accepted until `Close` (`internal/core/eventstream.go:103-107,231-247`). Clear the active group in the same `done` handling that ends streaming so any post-terminal event remains visible but unassociated.

### [P2] A tool call may consume only one terminal receipt

After the first receipt, the ID remains in `toolCallGroup.byID`; [`handleToolResultEvent`](../../../../internal/tui/app.go:1558) appends every later result to the same parent. A probe sent success followed by an error for the same ID: the call retained `ToolStatusCompleted`, two children were attached, the second child was not marked incomplete, and its `IsError` disagreed with the status icon. The R1 contract requires one authoritative terminal receipt ([`r1-contract.md:159`](../r1-contract.md:159)); the default Loop emits one result per call. A duplicate is therefore malformed input and should be rendered as a standalone incomplete event rather than accepted as another receipt. Retire/mark the ID consumed after its first terminal result while retaining the original row for display.

## Verification

The exact test names were listed before targeted runs. The temporary probes were run with `rtk proxy go test -p 2 ./internal/tui -run '^TestReviewProbe' -count=1 -v`; they reproduced all three findings and confirmed sanitized argument controls and expanded stdout/stderr/error rendering.

```text
rtk proxy go test -list 'Test(LiveBridge|AppToolOutput)' ./internal/tui
  exit 0; listed the 13 bridge/tool-output tests.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -run '^(TestAppToolOutputAppendsToToolCall|TestAppToolOutputNoToolCallFallsBack|TestLiveBridgeCopiesToolCallArgsAndPreservesOutputMetadata|TestLiveBridgeInvalidToolArgsBecomeUnavailableWithoutPanic|TestLiveBridgeToolArgsRenderUnavailableAndSanitizeTerminalControls|TestLiveBridgeDeepCopiesArgsBeforeModelProjection|TestLiveBridgeDoneDoesNotRenderDuplicateAssistantAnswer|TestLiveBridgeDoneFinalizesIncrementalEntryWithoutRenderingContent|TestLiveBridgeStreamedFinalStillConsumesToolCallMetadata|TestLiveBridgeToolGroupsReuseCallIDWithoutCrossGroupUpdates|TestLiveBridgeDuplicateAndEmptyIDsStayUncorrelated|TestLiveBridgeLegacyEmptyIDEventsRemainVisibleWithoutFalseCompletion|TestLiveBridgeToolOutputUsesStreamAndErrorRenderers)$' -count=1
  exit 0; internal/tui passed under race.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -count=1
  exit 0; full internal/tui race suite passed.

rtk proxy go test -list 'Test(E2E_(SlashCommandsViaInteractive|ErrorRecoveryViaInteractive|MultiTurnConversationViaInteractive|SessionPersistenceViaInteractive)|Phase39_REPLSessionCompleteTurn)' ./internal/cli
  exit 0; all five selected CLI tests were present.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/cli -run '^(TestE2E_SessionPersistenceViaInteractive|TestE2E_MultiTurnConversationViaInteractive|TestE2E_SlashCommandsViaInteractive|TestE2E_ErrorRecoveryViaInteractive|TestPhase39_REPLSessionCompleteTurn)$' -count=1
  exit 0; selected CLI race tests passed.

rtk env GOMAXPROCS=2 golangci-lint run ./internal/tui ./internal/cli
  exit 1; one goimports issue remains at internal/tui/r5_live_projection_test.go:8:1.
```

The lint finding is import grouping/formatting in the new test file; no other lint issue was reported. No full CLI E2E journey or durable-session acceptance was attempted in this UI-only review.

## Builder validation: `78473f3c48f03c2667d857e205d42111cc2f90f9`

**Builder validation only: the follow-up claims to address the three findings above.** This section records the builder's evidence, not an independent reviewer acceptance. The original rejection of `7f66b3d` remains historical. The claims are limited to the live TUI slice and do not certify durable transcript/history wiring, default CLI acceptance, or full R5 closure.

Reviewed the follow-up in `/Users/pengjunchen/Documents/github/go-cli-r5-live-projection`, on `codex/runtime-r5-live-projection` at `78473f3c48f03c2667d857e205d42111cc2f90f9`. Only `internal/tui/app.go`, `internal/tui/bridge.go`, and `internal/tui/r5_live_projection_test.go` changed in the fix commit, within the approved six-file slice.

The bridge now uses a single bounded recursive clone for JSON scalar/map/slice argument values. It checks both value and pointer method sets for custom JSON and text encoders before traversing values, does not call `json.Marshal` during cloning, and reports unsupported, cyclic/deep, or over-budget arguments as unavailable. The new panic encoder test exercises direct event conversion; bridge-to-BubbleTea tests verify custom JSON and text encoders are not called and the view reports unavailable details.

The `done` branch now clears `toolCallGroup` under the model lock. The first terminal result retires its ID from lookup while keeping the original row and result child; later events with that ID are standalone incomplete entries. The builder's Bridge-to-model tests verify a late result after `done` cannot attach, and a duplicate failure after a successful receipt cannot change the completed status or append a second valid child.

The test import grouping issue reported above is fixed. Exact verification on the follow-up worktree:

```text
rtk proxy go test -list 'Test(LiveBridge|AppToolOutput)' ./internal/tui
  exit 0; listed all 18 targeted TUI tests, including the new custom-encoder and terminal-boundary cases.

rtk env GOMAXPROCS=2 go test -p 2 ./internal/tui -run '^(TestLiveBridgeRejectsCustomToolArgEncoderWithoutCallingIt|TestLiveBridgeDoneClosesToolCallGroupBeforeLateResult|TestLiveBridgeDuplicateTerminalReceiptIsStandaloneIncomplete)$' -count=1
  exit 1 before the fix; reproduced the custom MarshalJSON panic, retained group after done, and duplicate terminal receipt being attached to the original call.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -run '^(TestLiveBridge.*|TestAppToolOutput.*)$' -count=1
  exit 0; targeted TUI bridge/projection tests passed under race.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -count=1
  exit 0; full internal/tui race suite passed.

rtk proxy go test -list 'Test(E2E_(SlashCommandsViaInteractive|ErrorRecoveryViaInteractive|MultiTurnConversationViaInteractive|SessionPersistenceViaInteractive)|Phase39_REPLSessionCompleteTurn)' ./internal/cli
  exit 0; all five selected CLI tests were present.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/cli -run '^(TestE2E_SessionPersistenceViaInteractive|TestE2E_MultiTurnConversationViaInteractive|TestE2E_SlashCommandsViaInteractive|TestE2E_ErrorRecoveryViaInteractive|TestPhase39_REPLSessionCompleteTurn)$' -count=1
  exit 0; selected CLI race tests passed.

rtk env GOMAXPROCS=2 golangci-lint run ./internal/tui ./internal/cli
  exit 0; 0 issues.

rtk git diff --check
  exit 0.
```

## Independent review: `78473f3c48f03c2667d857e205d42111cc2f90f9`

**Verdict: reject pending one P2 bounded-size fix.** The three historical findings are fixed in the frozen code: the bridge rejects value- and pointer-receiver `json.Marshaler`/`encoding.TextMarshaler` types before traversal; `done` clears the active group; and a first result consumes its ID so a duplicate or post-`done` result renders as a standalone incomplete entry without changing the original row's status. Valid nested map/slice arguments and the exact `int64(9007199254740993)` value remain preserved. The committed bridge tests exercise `EventStream → BridgeEvents → teaModel.Update/View` for custom encoders, `done`, duplicate receipts, and ordinary arguments.

The remaining blocker is that `cloneToolArgValue` limits recursion depth to 128 and node count to 10,000, but does not charge bytes in string values or map keys. `renderToolCallDetails` then marshals the complete argument map and emits the complete string value in the collapsed row. A temporary probe, removed after the run, passed a single 2 MiB string through `CoreEventToAgentEvent` and `renderToolCallDetails`: `ArgsUnavailable` stayed false and the rendered row retained more than 2 MiB. This demonstrates unbounded argument/render allocation by bytes despite the node limit. Add a total byte budget across string values and map keys; over-budget arguments should render an explicit “detail unavailable” marker, never a silently truncated prefix presented as full arguments. Do not rely on the R1 provider-candidate cap as proof for every Bridge input source.

Independent verification:

```text
rtk proxy go test -list 'Test(LiveBridge|AppToolOutput)' ./internal/tui
  exit 0; listed all 18 bridge/tool-output tests.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -run '^(TestLiveBridge.*|TestAppToolOutput.*)$' -count=1
  exit 0; targeted bridge/projection tests passed under race.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -count=1
  exit 0; full internal/tui race suite passed.

rtk proxy go test -list 'Test(E2E_(SlashCommandsViaInteractive|ErrorRecoveryViaInteractive|MultiTurnConversationViaInteractive|SessionPersistenceViaInteractive)|Phase39_REPLSessionCompleteTurn)' ./internal/cli
  exit 0; all five selected CLI tests were present.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/cli -run '^(TestE2E_SessionPersistenceViaInteractive|TestE2E_MultiTurnConversationViaInteractive|TestE2E_SlashCommandsViaInteractive|TestE2E_ErrorRecoveryViaInteractive|TestPhase39_REPLSessionCompleteTurn)$' -count=1
  exit 0; selected CLI race tests passed.

rtk env GOMAXPROCS=2 golangci-lint run ./internal/tui ./internal/cli
  exit 0; 0 issues.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -run '^TestReviewToolArgsStringByteBudgetProbe$' -count=1
  exit 0; temporary probe accepted and rendered a 2 MiB scalar argument; probe file was removed.
```

The independent result is scoped to this live projection slice. It does not accept durable transcript/history wiring or claim full R5 closure.

## Independent review: `50878e54d3ab009f0b8236ef56c99c8fb8cc6541`

**Verdict: accept the bounded live tool-argument projection fix.** The 64 KiB budget is charged per call against the exact JSON representation of the cloned argument tree: scalar bytes, quoted strings and map keys, JSON escaping, and map/array framing. Oversized strings and every map key are checked before copying or allocating the cloned container; map/slice allocations are also bounded by the existing 10,000-node cap. The existing 128-level depth limit and bounded recursion keep cyclic or deeply nested input finite. Encoder-method checks still reject custom `json.Marshaler` and `encoding.TextMarshaler` values before any marshal operation; the only `json.Marshal` performed during cloning is for fixed-width built-in numeric kinds after those checks.

Over-budget inputs set `ArgsUnavailable` while retaining the tool-call ID. The bridge-to-model tests show the row remains visible with pending status and an explicit “detail unavailable” marker, without exposing a prefix of rejected arguments. The boundary fixtures build map and array encodings exactly 65,536 bytes, including multibyte `é`, newline and `U+0001` escaping, and HTML-sensitive `<`/`&`; additional cases cover oversized values, a key larger than the limit, aggregate map values, per-call budgets, and custom encoders that panic or record invocation.

Independent verification on the clean `codex/runtime-r5-live-projection` worktree at `50878e54d3ab009f0b8236ef56c99c8fb8cc6541`:

```text
rtk env GOMAXPROCS=2 go test -list 'Test(LiveBridge|AppToolOutput)' ./internal/tui
  exit 0; listed the 19 bridge/tool-output tests.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -run '^(TestLiveBridge.*|TestAppToolOutput.*)$' -count=1
  exit 0; targeted bridge/projection suite passed under race.

rtk env GOMAXPROCS=2 go test -race -p 2 ./internal/tui -count=1
  exit 0; full internal/tui race suite passed.

rtk env GOMAXPROCS=2 go vet ./internal/tui/...
  exit 0.

rtk golangci-lint run ./internal/tui
  exit 0; no issues.

rtk git diff 78473f3..50878e5 --check
  exit 0.
```

This acceptance covers the live argument-detail budget and its Bridge-to-View behavior only. It does not claim durable transcript/history wiring, viewport memory bounds, CLI E2E acceptance, or full R5 closure.
