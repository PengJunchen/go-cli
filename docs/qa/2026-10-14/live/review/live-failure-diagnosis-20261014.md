# Live-run failure diagnosis — 2026-10-14

This is an independent diagnostic note, not a live acceptance result. Neither inspected run produced an acceptable end-to-end pass or a PPTX for visual review.

## Scoped run `20261014-153502-05113d31`: loop-detector false positive

The run used source revision `f827a91e3782cf1d787b65b5da4b9ba6fc86d74d` and the recorded race binary SHA-256 `09f52c4e67e69eba81389c8c8b841a2b2b8cd92157672d71eab943f474b66dcb`. Four provider responses for `inspect-fixtures` completed with HTTP 200, distinct response IDs, `finish_reason=tool_calls`, and terminal `[DONE]` markers. Their distinct proposed calls were:

| Provider request | Tool call | Argument SHA-256 |
| --- | --- | --- |
| 2 | `ls` (`call_e1fdc9493362465db15d1f3460c1af79`) | `4ae486c3a48f8dc732af672b138b438a1d96960304cc334d46bbc2687d169cbb` |
| 3 | `grep` (`call_00a5fbe294a64939a3e35c36d031c74a`) | `7589d883a50a3aece3a6d2ec2ee4f1b70fb1cd363ac06e61bba96d8462117206` |
| 4 | `read scan-a.txt` (`call_b753f40fc0e147ed9133f5a9384efdf0`) | `548c04169c68c826e72832657b8ac7ba10b8ef973abf041c69ec8329014de383` |
| 5 | `read nested/scan-b.txt` (`call_70e5c45b84b84a7683fe8912ae521cd2`) | `b75ba111d750bfe190b83c25ca5e276935725b7d8e74afcaef94a1fa4612c118` |

The first assistant tool response (request 2, `ls`) included a non-empty plan and was executed successfully. The following assistant responses for request 3 (`grep`) and request 4 (`read scan-a.txt`) had empty text and both tool calls were executed with success receipts. Request 5 proposed the distinct `read nested/scan-b.txt` call but was rejected by the loop detector before it was written as a tool row or executed. The middleware in `internal/llm/middleware_loopdetect.go` records only `resp.Content` for non-streaming responses and concatenates only `chunk.Content` in the stream path. It therefore counts the three successive empty textual bodies for requests 3–5 as the same response, despite their distinct tool names, call IDs, arguments, and provider response IDs. The next user turn's separate write proposal (request 6) also had empty text and was rejected at count 4 before the expected approval prompt. The PTY reports count 3 at turn 2 and count 4 at turn 3, consistent with the content-only history persisting across turns. This run does not show repeated model/tool behavior; it shows the loop key omits structured tool calls.

Keep the loop guard and its repeated-output negative case. Add structured response identity to the loop key, or handle tool-call-only responses separately while retaining same-tool-call repetition detection. Regression coverage should include three empty-content responses with different calls (allowed), same assistant text with different calls (allowed), and genuinely identical assistant/tool-call responses reaching the configured threshold (still rejected).

The current uncommitted loop-detector diff uses a length-prefixed SHA-256 fingerprint over response text and ordered tool names/JSON arguments, excluding provider-generated call IDs. The inspected provider stream contract places the completed tool-call list on the final chunk. New tests cover distinct empty-content calls, same prose with different calls, and repeated same-action calls despite changing IDs; the existing repeated-text negatives remain. Targeted `internal/llm` tests for those cases passed. This is a source-diff review only; full package/race checks and a fresh live replay remain outstanding.

Evidence: `../terminal/20261014-153502-05113d31/run-report.json`, `../terminal/20261014-153502-05113d31/pty-session.ansi`, and `../workspaces/20261014-153502-05113d31/session-store/sessions/s_0dc680cc8fbaa7de3606272abd7ade9f.jsonl`.

## Run `20261014-152358-63990f30`: provider timeout followed by transcript identity rejection

The second provider request did not complete: its Magpie route settled as canceled/499 after the CLI stream-idle timeout, with no completed status, finish reason, or `[DONE]`. The CLI log records `turn.end status=failed canceled=false`; the durable session has a terminal checkpoint for that run with `run_outcome=cancelled`. The UI then surfaced `tui: transcript refresh identity does not match the active app` and exited. There is no PPTX or tool execution from this run.

The source has two different classifiers for the likely same wrapped provider error. `internal/core/run_commit_consumer.go:1016` maps any error matching `context.Canceled` or `context.DeadlineExceeded` to a canceled durable terminal outcome. The run owner maps an error to canceled only when its outer run context was itself canceled and the run error matches that context; a provider-local idle deadline can leave that outer context active and yield owner outcome `error`. For a synced publication, `internal/tui/transcript_control.go:176` requires a quiescent, known observation and an exact match between the observed owner outcome and the durable terminal checkpoint. The evidence (failed/non-canceled turn plus canceled terminal row) is consistent with this outcome disagreement being the rejected predicate. The persisted report does not capture owner publication/observation values or a rejection reason, so the exact failed predicate is not directly proven.

Minimal regression: run a durable turn with a provider that returns a wrapped request-context deadline while the parent/owner context remains live; join the owner, project the stored terminal row, and assert owner status and terminal outcome agree and the TUI refresh acknowledgement succeeds. Pair it with explicit cancellation of the parent context and assert both representations remain `cancelled`. A sanitized rejection reason (for example, `synced_terminal_outcome_mismatch`) would make future failures attributable without exposing prompts or credentials. Keep the synced-publication equality check fail-closed.

Evidence: `../terminal/20261014-152358-63990f30/run-report.json`, `../terminal/20261014-152358-63990f30/pty-session.ansi`, `../workspaces/20261014-152358-63990f30/session-store/sessions/s_61671dcaaf4893f300e43e8d45432c51.jsonl`, and that run's `.go-cli/logs/go-cli.log`.

## Default empty-whitelist completion fix review

Read-only review of the current `internal/tools/trusted_registry.go` diff found that only reviewed concrete `*GrepTool` and `*FindTool` registrations gain the internal completion-collection mode when the whitelist is empty or invalid. Those registrations retain `EffectUnknown`, an empty scope fingerprint, and the ordinary `RegistrationSourceToolRegistry` provenance. The private mode is not a publishable completion proof; the prepared dispatcher accepts a final proof only after the actual synchronous no-process attestation or managed process lifecycle signal joins with the executor return. Thus the change separates resource completion evidence from path authorization. Targeted tests passed:

```text
rtk proxy go test ./internal/tools ./internal/core -run 'TestPreparedDefaultUnscopedSearchReportsConcreteCompletion|TestRunCommitConsumerResolvesSearchAfterCompletionProof|TestUnscopedAndInvalidReadersKeepTheirExistingRegistrationMetadata|TestUnreviewedToolNamedGrepDoesNotReceiveSearchCompletionMode' -count=1
```

This is a source-level review of the uncommitted diff, not a full default-config live run or a final acceptance finding.
