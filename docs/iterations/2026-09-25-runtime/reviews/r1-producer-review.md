# R1 Producer Code Review

Date: 2026-09-25

Reviewed the fixed producer source commit `c2a187d79ad229e8b997c4a6b23388344bba5efe`, integrated snapshot `c847496a1b5acabdd20e1ee14370e4cee285d792`, Claude event-order follow-up `e6ecab0dd158aa3b79271ce5f4572a26e5d371d6` as integrated at `eff4ac3f0818575ec60eeb4d28ef459e7dc3c744`, and source follow-up `9ad9973d22b3e0f01e70aa3abd870103d444479a`, now integrated at `3082213e67d96a955d27af9951f83bf42cc75f94`. The integration snapshots retain the privacy changes from `27d8f06` and `7350dab`; this review did not modify either worktree.

Review status: accepted through integrated commit `3082213e67d96a955d27af9951f83bf42cc75f94`. The original P2 event-order finding is fixed in `98f791a`; the Claude protocol and text-order follow-ups are fixed in `e6ecab0` and `9ad9973`. The Claude source diff is unchanged in integration commit `3082213`. Root's final integrated full-repository gate remains pending. The budget, complete-guard, canonical-delta, and privacy-merge checks in this report passed.

## Finding

The e6 implementation initially allowed two text blocks to interleave deltas in arrival order, which could disagree with their final block order because `MessageChunk` has no block-index field ([`llm.go`](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r1-integration/internal/llm/llm.go:76)). Source follow-up `9ad9973` now fails closed on a higher-index text delta while a lower-index text block remains open ([`native.go`](/Users/pengjunchen/Documents/github/go-cli-runtime-worktrees/r1-claude-order/internal/llm/native.go:702)); a regression fixture asserts an `A1(index 0)`, `B1(index 1)`, `A2(index 0)` stream errors without a final chunk, and another asserts text from index 1 is accepted after index 0 stops. The accepted interleave positive remains text block 0 plus tool block 1, where the emitted text order is representable. Anthropic documents content-block indices as their positions in the final message content, and the official Go SDK states that deltas/stops may interleave across open blocks ([streaming flow and block indices](https://platform.claude.com/docs/en/build-with-claude/streaming), [SDK accumulator](https://github.com/anthropics/anthropic-sdk-go/blob/main/messageutil.go#L1480-L1488), [SDK index handling](https://github.com/anthropics/anthropic-sdk-go/blob/main/messageutil.go#L1696-L1708)).

The earlier P2 that allowed deltas before `message_start`/`content_block_start` is closed by `98f791a`: the parser validates message/block lifecycle, block-index progression, and delta type against the opened block. Commit `e6ecab0` also covers legal interleaving by index, multiple `message_delta` events, terminal-reason overwrite/clear behavior, named-event/data-type equality, and top-level `message_delta.usage`. Its positive fixtures now use matching Claude SSE event names and the documented top-level usage shape. Claude remains disabled from default routing; these changes only harden its existing stream parser.

No P1 finding was identified in the reviewed terminal, continuation, budget, candidate-guard, canonical-delta, or privacy-merge paths.

## Reviewed behavior

- OpenAI streaming requires a terminal finish reason and `[DONE]`; later choice chunks fail, while usage-only chunks remain accepted.
- Gemini requires a candidate finish reason, rejects candidates after that reason, and permits usage-only trailing metadata. Claude requires `message_stop` and a stop reason, and rejects data after `message_stop`.
- `timeoutModel.Stream` continues reading after `Final`, preserves a later error, and rejects a stream that does not close after its final chunk. Core and stream wrappers select on cancellation instead of blocking on an inner channel.
- Candidate limits are 400,000 bytes, 16,384 chunks, and 4,096 tool calls. The core counts text across continuation segments, checks each complete serialized segment (including tool-call payload) before discarding partial tool calls, then checks the merged candidate. The complete-response guard runs once after continuation and structural tool-call validation, before guarded text is published.
- `Run` delegates to `RunCanonical`; the canonical result starts with only the current submission and records this call's assistant messages, steering/follow-up inputs, and tool receipts. It does not copy prior submission history into the returned delta.
- The integrated snapshot retains bounded `ProviderError` status/type span attributes and excludes provider response bodies from `Error()`. Targeted redaction/error-boundary tests passed.

## Validation evidence

Commands were run from the stated worktrees with `rtk`.

On source commit `c2a187d79ad229e8b997c4a6b23388344bba5efe`:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 ./internal/llm ./internal/core
exit 0
internal/llm: 2.667s
internal/core: 3.587s
```

On integrated commit `c847496a1b5acabdd20e1ee14370e4cee285d792`, terminal-state, timeout-after-final, continuation, segment-budget, complete-guard, and canonical-delta tests:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^(TestStream(ReadErrorIsNotComplete|MalformedEventIsNotComplete|TimeoutIsNotComplete)|TestOpenAIStream(RejectsChoicesAfterTerminalFinishReason|RequiresTerminalFinishReason)|TestStreamGeminiRejectsCandidatesAfterTerminalFinishReason|TestTimeoutModelMiddleware(StreamPreservesErrorAfterFinal|FinalMustBeFollowedByChannelClose)|TestCompleteResponseGuard(SeesWholeContinuationAndPreservesToolCalls|ContinuationFailurePublishesNoCandidate|DenialPublishesNoCandidate|RejectsInvalidToolGroupBeforePublishingText)|TestGuardedStreamBudgetIncludes(ToolCallPayload|LengthSegmentToolPayload)|TestStreamGenerateCancellationDoesNotWaitForInnerChannelClose|TestCanonicalLoopTranscriptIsPerCall|TestCanonicalLoopIncludesUserWhenModelIsMissing|TestCanonicalFailureSynthesisDoesNotRetryOrRebuildTranscriptFromEvents|TestCanonicalMiddlewareLogsErrorMetadataWithoutRawText|TestContinuationMaxAttemptsStillPartialSafe|TestMaxContinuationAttemptsEnforced)$' ./internal/llm ./internal/core
exit 0
internal/llm: 1.063s
internal/core: 1.073s
```

On the same integrated commit, provider-error redaction and recovery-boundary tests:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^(TestProviderError_(Error|Unwrap|ErrorsAs)|TestNewProviderError_StatusMapping|TestFailureTurnSynthesizer_ProviderErrorBodyKeywordsAreNotRecoverySignals|TestCanonicalMiddlewareLogsErrorMetadataWithoutRawText)$' ./internal/llm ./internal/core
exit 0
internal/llm: 1.024s
internal/core: 1.029s
```

On integrated commit `eff4ac3f0818575ec60eeb4d28ef459e7dc3c744`, the Claude stream-state, interleaving, terminal-reason, event-name, and usage fixtures were independently rerun:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^(TestClaudeStream|TestStreamClaude_)' ./internal/llm
exit 0
internal/llm: 1.039s
```

On source follow-up commit `9ad9973d22b3e0f01e70aa3abd870103d444479a`, the full targeted Claude stream suite, including the out-of-order text rejection, ordered-text acceptance, terminal semantics, event-name, and usage fixtures, was independently rerun:

```text
rtk env GOMAXPROCS=2 go test -p 2 -race -count=1 -run '^(TestClaudeStream|TestStreamClaude_)' ./internal/llm
exit 0
internal/llm: 1.035s
```

The six Assembly integration tests were not rerun in this review; they belong to the privacy lane. The root integration worktree has now picked `9ad9973` as `3082213`; the relevant Claude files are identical to the source commit used for the targeted race. Root's final integrated full-repository gate remains pending, so this report does not claim that gate passed.
