# Scoped Grep/Find completion proof diagnosis

Date: 2026-10-14  
Repository revision at reproduction: `e049` (`/Users/yinyuepeng/projects/go-cli`)

## Real-provider failure

The real PTY run at `docs/qa/2026-10-14/live/workspaces/20261014-150122-7ef3b912` completed an initial text turn, then a turn that ran `ls` and Grep. The Grep tool returned a normal completion receipt with zero matches. The durable run then stopped with `ErrRunCommitQuiescenceUnproven`; owner publication had `quiescent=true`, `outcome_known=false`, and matching run/session/generation identifiers. There was no subsequent upstream model request or file read. The relevant artifacts are:

- `docs/qa/2026-10-14/live/terminal/20261014-150122-7ef3b912/evidence.md`
- `docs/qa/2026-10-14/live/workspaces/20261014-150122-7ef3b912/traces/session-fe448e94817e8c31983727d51b97fc8b.jsonl`
- `docs/qa/2026-10-14/live/workspaces/20261014-150122-7ef3b912/session-store/sessions/s_d043d74c093f0ba9b5f1fea72ec72fb7.jsonl`

Magpie recorded three successful upstream completions before the failure, all routed to the signed-in Codex provider: request IDs `chatcmpl-resp_0b63e6003497dbed016acf28d6ae80819a983b0dda434b7085`, `chatcmpl-resp_01fe955f7c0d84bc016acf28deb7d8819aa8cfccc8e1ad7d86`, and `chatcmpl-resp_0e38fe22b41a7ca2016acf28e55f7c81989d5d2cf0a1f42731`. The last completion preceded the Grep receipt; no fourth request followed it. This is evidence of a real-provider failure, not a passing end-to-end result.

## Cause

Trusted scoped Grep and Find registrations carried a workspace whitelist but no prepared-completion mode. Their managed helper completion reporter was installed only for `CompletionOwnedProcessGroupJoined`, so the helper's positive join signal never reached the prepared invocation. The pure-Go fallback also returned without an explicit synchronous-completion attestation. The durable consumer correctly refused to infer local-resource quiescence from a tool receipt alone.

## Fix

Trusted, validly scoped Grep and Find registrations now enter a private `completionScopedSearchJoined` dispatch mode. It is a registration/dispatch mode only and is rejected by the final-proof validator and generic completion reporter. The dispatcher accepts one concrete attestation:

- `CompletionOwnedProcessGroupJoined` after the existing managed helper lifecycle reports its joined/closed result; or
- `CompletionInlinePipelineJoined` when Grep or Find actually used the synchronous no-helper path and its execution has returned.

The final signal publishes only that concrete proof after the full tool pipeline returns. Missing, pending, duplicate, mixed, hybrid-as-proof, failed, timeout, and forced-pipe-close cases remain unproven. Core durable guard logic was not changed.

## Regression evidence

The tests were first added as red regressions. Before the producer fix:

- `rtk proxy go test ./internal/tools -run 'TestPreparedScopedSearch' -count=1` failed for Grep/Find on both the no-helper and helper paths because their completion signal had no `Done` proof.
- `rtk proxy go test ./internal/core -run TestRunCommitConsumerResolvesScopedSearchAfterCompletionProof -count=1` failed all four durable-consumer cases with `ErrRunCommitQuiescenceUnproven`.

After the fix, these passed:

- Focused tools and core regression tests for both concrete paths, missing proof, pending proof, mixed/duplicate reports, failed execution, timeout, and forced pipe closure.
- `rtk proxy go test ./internal/tools ./internal/core -count=1`
- `rtk proxy go test -race ./internal/tools -run 'TestPreparedScopedSearch|TestPreparedCompletionScopedSearch|TestPreparedScopedGrep(ForcedPipeClose|TimeoutWithEscapedWriter)' -count=1`
- `rtk proxy go test -race ./internal/core -run 'TestRunCommitConsumer(ResolvesScopedSearchAfterCompletionProof|StopsAfterPreparedActionWithoutCompletionProof)' -count=1`
- `rtk proxy git diff --check`

The race checks above were rerun after adding the negative case that passes `completionScopedSearchJoined` as a final report; it is rejected. The new durable-consumer test uses the local fake model for the regression boundary. The production fix still requires the harness owner's fresh real-provider rerun before the original live workflow can be called verified.

## Post-fix live replay (inconclusive)

The harness rebuilt with the fix and started a new real CLI run, `20261014-152358-63990f30`. Its first text inference completed, but the next streamed inference was canceled before any tool dispatch: Magpie later recorded HTTP 499 for request `chatcmpl-resp_05e21a58ab5e51bc016acf2e294e58819bbb9e0461d8b48c70`, with zero input/output tokens. The CLI reported that its 10-second stream-idle deadline expired; the terminal then reported a transcript identity mismatch. This run did not reach Grep or Find, so it neither verifies nor contradicts the completion-proof fix.

Run evidence:

- `docs/qa/2026-10-14/live/terminal/20261014-152358-63990f30/run-report.json`
- `docs/qa/2026-10-14/live/workspaces/20261014-152358-63990f30/traces/session-b8c7f64ee076434d6f27920e2158425b.jsonl`
- `docs/qa/2026-10-14/live/workspaces/20261014-152358-63990f30/workspace/.go-cli/logs/go-cli.log`

A fresh live run that reaches scoped Grep/Find is still required for real-provider verification.

## Default unscoped registration gap and correction

A fresh real CLI run on the scoped-only fix (`f827a91e3782cf1d787b65b5da4b9ba6fc86d74d`, run `20261014-153101-ef080409`) reached a different, narrower reproduction. The first three real model requests returned HTTP 200. The approved `ls` call received a completion receipt and was followed by a model request that issued a Grep call. After Grep returned successfully, the durable owner again published `quiescent=true` with `outcome_known=false`; no later provider request or read followed. The session record shows that this Grep was registered through `tool_registry` with an empty scope fingerprint and `unknown` effect, and its action row had `cleanup_pending` with no completion proof. The preceding `ls` row was `builtin_tool` with an inline proof. This isolated the missing path: default `RegisterDefaults` without `WithRegisteredPathWhitelist` sends Grep/Find through the trusted-reader registrar, whose empty-whitelist branch fell back to ordinary `Register` before assigning the search completion mode.

Artifacts:

- `docs/qa/2026-10-14/live/terminal/20261014-153101-ef080409/run-report.json`
- `docs/qa/2026-10-14/live/terminal/20261014-153101-ef080409/evidence.md`
- `docs/qa/2026-10-14/live/workspaces/20261014-153101-ef080409/traces/session-daece38fa363e37ed0d311b6c48eeedc.jsonl`
- `docs/qa/2026-10-14/live/workspaces/20261014-153101-ef080409/session-store/sessions/s_91349dcd0d382ef0c63dc37d787c1227.jsonl`
- `docs/qa/2026-10-14/live/workspaces/20261014-153101-ef080409/workspace/.go-cli/logs/go-cli.log`

The regression tests were run red before changing production code. All four prepared default-registration cases (Grep/Find × no-helper fallback/managed helper) had no completion signal, and all four corresponding durable consumer cases failed with `ErrRunCommitQuiescenceUnproven`. Registration assertions also showed that the default and invalid-whitelist Grep/Find registrations had no completion mode.

The correction assigns the private search dispatch mode in the concrete Grep/Find type cases before the empty/invalid-whitelist branch. That branch now retains `tool_registry` provenance, unknown effects, and an empty scope while recording only the completion mode. A valid workspace whitelist still follows the existing scoped registration path and can mint the workspace-read effect. Unknown custom tools remain rejected by the trusted-reader registrar; ordinary custom registrations, including one named `grep`, receive no search completion mode.

After the correction, the focused tools tests for default metadata, the unknown-name negative case, and the four prepared helper/fallback paths passed. The durable consumer test passed for scoped and default-unscoped registrations across Grep/Find and helper/fallback paths. This is still source-level regression evidence: a fresh real-provider replay that reaches the now-correct default registration path is pending.

Validation after the default-registration correction:

- `rtk proxy go test ./internal/tools -run 'TestUnscopedAndInvalidReadersKeepTheirExistingRegistrationMetadata|TestUnreviewedToolNamedGrepDoesNotReceiveSearchCompletionMode|TestPreparedDefaultUnscopedSearchReportsConcreteCompletion' -count=1`
- `rtk proxy go test ./internal/core -run 'TestRunCommitConsumer(ResolvesSearchAfterCompletionProof|StopsAfterPreparedActionWithoutCompletionProof)' -count=1`
- `rtk proxy go test ./internal/tools ./internal/core -count=1`
- `rtk proxy go test -race ./internal/tools -run 'TestUnscopedAndInvalidReadersKeepTheirExistingRegistrationMetadata|TestUnreviewedToolNamedGrepDoesNotReceiveSearchCompletionMode|TestPreparedDefaultUnscopedSearchReportsConcreteCompletion' -count=1`
- `rtk proxy go test -race ./internal/core -run 'TestRunCommitConsumer(ResolvesSearchAfterCompletionProof|StopsAfterPreparedActionWithoutCompletionProof)' -count=1`
- `rtk proxy git diff --check`
