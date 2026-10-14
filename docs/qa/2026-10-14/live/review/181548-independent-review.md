# Independent review: live run 20261014-181548-4087fce4

## Result

This is a **failed partial live run**, not an end-to-end pass. Dot completed the opening text/tool, approval, policy-rejection, write/edit, and long-Bash/follow-up scenes through real `codex/gpt-6.1-sol` inferences. The run stopped during `pptx-create` when the upstream response was cancelled after an idle timeout. No PPTX was created; presentation edit, context compaction, cancellation recovery, and the new-session fence were not reached. Keep this run as a failure and do not use it as deck or compaction evidence.

The saved report’s terminal error is `stream idle exceeded 1m30s: context deadline exceeded` on inference 19. Magpie route `1791960021360` ended status 499/cancelled after 122002 ms. The bridge observed an incomplete streaming response with no finish reason or `[DONE]`, zero usage, and no completed route. A partial `write` call fragment in that cancelled response has no accepted tool result, receipt, or file effect. The target PPTX is absent from the workspace.

## Run and evidence identity

- Run: `20261014-181548-4087fce4`; Dot session: `go-cli-live-20261014-73b8c3fda07b`.
- Model requested and upstream-served on completed turns: `codex/gpt-6.1-sol`; Magpie’s route model field is normalized as `gpt-6.1-sol`.
- Source revision: `49dbf9755d860b877edd45ed9a73161ab5ebbb8f`; staged and unstaged source snapshots at build were empty and remained empty.
- Built race binary: `docs/qa/2026-10-14/live/workspaces/20261014-181548-4087fce4/bin/go-cli-race`; SHA-256 `4ac09868598015fccc50a16ef90e6c4284d91707240314ad3a0329dece355218`.
- Final run-report SHA-256: `808a4cbac3abcee2b832d6069b323bd6332576150ef5bf062a451972bb9f913f`.
- PTY ANSI stream SHA-256: `de828efd4e6684d7456e1916aa2446120c0b72b5c908c825b75d1c5c09013f84`.
- Fixture SHA-256: `42e3aa59884639bc983ef65dc93903b5110f4b1eb32c770a7ece203a59f75846`.
- Working directory and HOME were run-specific. The default acceptance case had no `sandbox.allowed_paths`; this measures application policy and does not establish OS sandboxing.
- Screenshots were xterm.js renders of the live PTY stream, not native Terminal captures.

## Independently checked provider evidence

The bridge recorded 19 inference attempts, each with a distinct response ID. I joined each bridge response ID to its embedded per-request Magpie route snapshot and the Magpie usage row by exact `response_id` and `route_id`; the exact IDs agreed. The first 18 requests completed with HTTP 200, `codex` provider identity, the requested/served model match, a finish reason and terminal stream marker. Their Magpie route records were done/status 200, and upstream prompt/output counts reconciled to Magpie uncached plus cached input and output counts. The 19th request is the cancelled timeout described above and is correctly excluded from completed-inference evidence.

The aggregate route endpoint snapshot contains only its latest route, the cancelled route. Earlier completed routes are independently represented by their exact per-request `routeSnapshot` entries in the saved bridge evidence; do not mistake the latest-only aggregate list for missing earlier routes. The runner’s `providerEvidence.inference` projection does not enforce response-ID uniqueness itself and omits some `userMarkers`; the raw bridge request rows do preserve those fields. Future acceptance should validate uniqueness and prompt/receipt linkage against the raw bridge rows, not infer it from the lossy projection.

## Scenes with direct evidence

| Scene | Independent result | Evidence |
| --- | --- | --- |
| Plain text and fixture inspection | PASS for the executed scene. Five completed inspection inferences include `ls`, literal `grep`, and two `read` calls; receipts carry both exact fixture facts. Default unscoped `grep` retained zero scope and unknown effect, while canonical reads/listing used workspace scope. | `run-report.json`; `inspect-fixtures-1-approval-allow-page-1.png` |
| Human denial | PASS for the observed refusal. The write approval shows all six detail rows reviewed; the exact target was absent before and remained absent after, and the next provider request carried the denied receipt. | `run-report.json`; `03-deny-approval-deny-page-1.png` |
| Default Python policy rejection | PASS on raw evidence. The single requested Python Bash call was explicitly reviewed and approved once at the application UI, then rejected by the default tool policy before process start. Its durable receipt is an error with quiescent resource state and `inline_pipeline_joined` proof; the nonce marker stayed absent before and after. The actual assistant response said `python3` was blacklisted and did not write the marker, then a later ordinary no-tool request succeeded. The report’s explanation boolean is a lexical false negative: its allowed-phrase list omits `is blacklisted`. | `run-report.json`; `default-policy-reject-python-1-approval-allow-page-1.png`; `pptx-create-terminal-error.png` |
| Approved write and edit | PASS for the effects observed. A new nonce file was created with the requested value, then the same file was edited to the requested replacement; both actions have later receipts and durable quiescent completion proofs. | `run-report.json`; `04-allow-approval-allow-page-1.png`; `04-edit-approval-allow-page-1.png` |
| 20-second Bash and queued follow-up | PASS on raw evidence; the report’s two derived follow-up checks are false negatives. The running screenshot contains the busy footer while `.started` exists and `.finished` is absent. The follow-up was entered and submitted in that interval. Raw bridge inference 17 contains the exact uppercase follow-up marker and the completed Bash receipt, then calls `read`; inference 18 contains the marker and both the Bash and read receipts. The durable Bash resource is quiescent/successful and the later read returns the exact finish-file content. The check compares against the lowercase `nonce` while the prompt and raw bridge marker use uppercase `nonce_upper`; rows 17 and 18 directly demonstrate delivery and receipt linkage. | `run-report.json`; `06-long-bash-running.png`; `06-followup-input-ready.png`; `06-followup-input-filled.png`; `07-followup-submitted.png`; `08-long-bash-followup-complete.png` |
| PPTX and subsequent edit | NOT RUN. Fixture was generated, but no valid completed PPTX-create response or deck exists. No independent structural or visual deck review is possible for this run. | `presentation-fixture.json`; `pptx-create-terminal-error.png`; workspace inventory in `run-report.json` |
| Production compaction / recall | NOT RUN. | No production compaction record for this run. |
| Escape cancellation / explicit new session | NOT RUN. | No cancellation-recovery evidence for this run. |

## Acceptance implications

The real CLI and upstream protocol performed useful, independently attributable work before the failure. That partial evidence does not satisfy the requested full live matrix. The next clean run must produce a completed PPTX creation response and successful edit in a separate turn, then demonstrate durable production compaction and recall, and finish the cancellation/new-physical-session checks. Fix the runner’s case-sensitive nonce comparison and explanation phrase detector so accurately delivered work is not reported as absent; retaining `userMarkers` in the per-request projection would also make later independent audits stronger. Keep the failed attempt and its cancelled Magpie route separate from retries.
