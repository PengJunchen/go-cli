# Spec review

Request: validate the real Dot CLI/TUI and tool effects with a deterministic mock LLM, exercise long tasks, repair defects, and preserve evidence. Review began at `e77e8058`; upstream `1cd1951` and `4d1497b` were retained. Current tree `6aa6f93` includes product fixes `2244627` and `89df724` plus fixture updates.

## Coverage

- **Terminal:** [`terminal/evidence.md`](terminal/evidence.md) records 36/36 checks, 12 screenshots, and three raw PTY transcripts covering startup, Chinese streaming, approval deny/allow, long Bash, follow-up, and completion. xterm.js renders real binary PTY bytes.
- **Tools and sessions:** [`tools/approval-binary-e2e.md`](tools/approval-binary-e2e.md) covers real-binary read/write/edit/bash auto and deny, default permissions, error correction, actual receipts and file effects, and session resume in a second process.
- **Long tasks:** [`longtask/evidence.md`](longtask/evidence.md) records 120 local HTTP turns with compaction, follow-up in the next provider request after queue acknowledgment, and Escape cancellation. The focused race run confirmed no delayed side effect after five seconds, no automatic follow-up, then a successful fresh prompt.
- **Product fixes:** `2244627` preserves spaces in follow-up and steering input. `89df724` fixes cancelled legacy TUI lifecycle misreporting while retaining turn/event joins. Both have regression evidence; the focused run on `6aa6f93` returns from Escape in 18 ms.

## Status and limits

An earlier tagged run on `2244627` reproduced the Escape lifecycle error (`[quitting...]` before `[interrupted]`); `89df724` fixes it. Both final gates on stable commit `6aa6f93` pass:

- `make verify`: exit 0; 8,713 started, 8,707 passed, 0 failed, 6 skipped across 28 packages; lint 0 issues; scan 0 errors / 1,803 warnings; log 10/10 and leak 6/6.
- `make test-e2e`: exit 0; 4,787 started, 4,781 passed, 0 failed, 6 skipped across 7 packages.

The mixed-source intermediate verify run was disqualified and archived as [`verify-intermediate-startup-red.json`](gates/verify-intermediate-startup-red.json); it is excluded from the final results above.

The provider is a local deterministic mock, so no live-model quality claim is made. Runtime evidence is macOS; Linux was cross-compiled but not run. Headless Chrome captures xterm.js, not native Terminal. Bash E2E uses `--no-sandbox`, so OS sandbox enforcement is outside this evidence.

**Spec acceptance passes for the stated scope on `6aa6f93`.** The integrated tree has two scoped product fixes and no unrelated feature addition. See the [full report](README.md) for the plan, screenshot gallery, gate records, and evidence limits.
