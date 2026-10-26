# Independent live evidence review: `20261014-190417-930a46d9`

**Overall live case: FAIL.** The PPTX and context-compaction subcases pass independently, and the cancelled run remains unknown and fenced. The final recovery assertion failed because the runner inspected session JSONL rows before a newly created empty session file could appear; it returned before running the required successful turn in that session. A generated session ID alone does not satisfy the complete recovery case.

## Frozen inputs

- Run report: `terminal/20261014-190417-930a46d9/run-report.json`, SHA-256 `46f097757ecc940e97413c6083b47878123ab4df32f561a05c3c91f17b2a2a02`.
- Source revision: `ec267df999405a03a09ed5485256aee6cc133aae`.
- Race binary: `workspaces/20261014-190417-930a46d9/bin/go-cli-race`, SHA-256 `09650e64b03c1341f516265e7d1f086945247a594b2e8259ec0acfbf71ec662c`.
- Fixture nonce: `A18BD3BD0108`; isolated provider/session/workspace artifacts remain under the matching `workspaces/` directory.
- The independent per-response join and compaction checks are saved in `190417-independent-run-review.json`; the verifier is `scripts/e2e_live_review/review_live_evidence.py`. These checks intentionally use response IDs, route IDs, session IDs, and token counts only; they do not capture provider bodies or credentials.

## Provider and tool evidence

I independently rejoined all 35 local inference rows to the 35 Magpie usage rows by response ID. All 35 response IDs, route IDs, and local sequence numbers are unique. Each row has an exact response-ID/route-ID/session usage match, a matching per-request route snapshot, provider `codex`, model `gpt-6.1-sol` corresponding to the requested `codex/gpt-6.1-sol`, and HTTP status 200. Input accounting matches `uncached input + cache_read`; output accounting matches provider usage. No upstream request-ID field is populated in this report, so the correlation claim rests on the exact response ID plus route ID and session, not an invented request ID.

There are 34 streamed interactive turns with terminal stream completion and one separately classified nonstream JSON response at local sequence 30. Sequence 30 is the internal summary-provider request during pressure turn 4; the actual pressure-4 user turn is a separate streamed request at sequence 31 with its own marker and response ID. It is not counted as a missing interactive response. All interactive turns with configured marker expectations contain those markers in their current request.

The captured PTY and durable session store agree on the key tool cases. The denied `write` call targets `blocked_a18bd3bd0108.txt`; that file is absent. The exact `python3` Bash attempt receives a policy error and `python_policy_a18bd3bd0108.txt` is absent. The allowed write and edit produce the exact expected `approved_a18bd3bd0108.txt` content. The 20-second Bash created both long-task markers; the later model-requested read returned `long_a18bd3bd0108.finished`. The queued follow-up is visible after the running interval, and the CLI completed with exit status 0.

For cancellation, the genuine Escape-driven Bash action has an owned-process join and quiescent resources, but its durable receipt and terminal outcome remain `unknown`; `cancel_a18bd3bd0108.started` exists while the delayed `.finished` marker is absent. The ordinary post-cancel prompt displays the product fence notice, produced no durable user row or provider request, and did not change the old unknown state.

## Context compaction and recall: subcase PASS

The session store contains a production `compaction` row (`runcommit_791c8b0…`, sequence 120) and the matching production trace reports strategy `summary`, 61 items reduced to 10, 108,395 tokens before the strategy, and 33,344 tokens compacted. The one summary entry is bound through receipt `runcommit_419eb8e5…`, sequence 106, the successful `large-context.txt` Read receipt. I independently compared the workspace source to that exact durable receipt: both are 273,673 bytes with SHA-256 `1f5d59857eed679c5edd3f6ee04c903b73cca4fb6a6eedc0a503f55bada4c5b7`. Both first and last source markers occur in the receipt and in that same persisted summary. The summary also retains `LIVE_MEMORY_A18BD3BD0108=cobalt`.

After compaction, the later real provider request contains the latest prompt marker and retained canary; the durable assistant reply is exactly `LIVE_MEMORY_A18BD3BD0108=cobalt`. The source fixture has 3,800 repeated rows plus the two boundary lines. The summary accurately says the displayed middle output was truncated and does not claim it independently counted those rows.

## PPTX: subcase PASS

The separate [PPTX review](190417-pptx-independent-review.md) records the OOXML comparison, full-size visual inspection of all four independent renders, and byte-for-byte equality between the final workspace deck and frozen v2. Model-tool lineage is supported by distinct creation/edit user turns, their real provider response/tool-call metadata and durable receipts, and the byte-identical helper snapshots. This subcase does not override the failed final recovery requirement.

## Failed final recovery check and harness diagnosis

The explicit `/session new` command did start a distinct physical session: the live PTY screenshot `terminal/20261014-190417-930a46d9/13-session-new-recovery.png` shows the new ID `s_6ccd1a50a4ee1ba5dc79c90ef48cb677`, different from old session `s_af96a55bbf9465c0e081cb229e28b2fd`. The frozen report's one failed check is `explicit /session new selected a different physical session`. The runner's old check inferred file existence from `physical_session_ids()`, which discovers session IDs only by parsing JSONL rows. The new session JSONL was empty until its first user turn; the immediate row scan therefore saw only the old ID. A later read confirms the new, empty JSONL file exists.

That race caused the runner to return before its `after-cancel-fresh-prompt` step. There is no durable user row, successful provider response, or successful terminal for a post-recovery turn in the new session. Thus `/session new` itself worked, but the requested end-to-end recovery remains unverified and the attempt stays failed. A bounded check for the named empty JSONL file after the acknowledgement, followed by the existing fresh-turn assertions against the durable user row/provider response/success terminal, is the appropriate correction. Do not promote this failed attempt to pass after the harness change; rerun the complete flow on the pinned clean source.
