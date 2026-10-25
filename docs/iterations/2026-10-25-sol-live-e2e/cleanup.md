# Owned-resource cleanup

Cleanup completed on 2026-10-25, with exit zero and an independent read-only postcheck. Root stopped the two owned frontends after the final bridge closed and all owned CLI sessions exited. A scope-bound observation checked the fixed release binaries, five temporary workspaces, CWD/directory handles and recorded child processes. The shared Magpie service was outside the cleanup scope.

Five real release-CLI revokes ran after that stop observation. These were cleanup commands, without model calls, and are separate from the UI evaluation. Root reviewed the exact final deletion set and plan before execution.

| Item | Result |
| --- | --- |
| Actual CLI revokes | 5, all exited zero |
| Exact owned trust tombstones removed | 5; owned keys remaining: 0 |
| Owned temporary-workspace authority journals | 5 archived, hash-checked and removed |
| Original authority files | 58 preserved |
| Known authority files retained | 7: five stable workspace locks and the checkout journal/lock pair |
| Unattributed authority files | 4 retained; bodies not interpreted |
| Ignored checkout directories | 2 retained, containing 3 files |
| Hard-monitored personal paths | 8 content/permission/absence states preserved; one time-only observation |

The shared trust file and its stable lock remain. The executor checked semantic equality for every unrelated trust record while removing only the approved own tombstones under that lock. The postcheck independently confirmed the five keys were absent. No unrelated key, shared store or directory was removed or truncated.

There are 69 authority files after cleanup: 58 original files, seven known retained files and four unattributed files. This is deliberately not a claim that the authority store was emptied. Both `cmd/cli/.go-cli/` and `internal/cli/.go-cli/` remain because there was insufficient evidence to attribute every byte exclusively to this task. File times alone did not authorize deletion.

Archive file data, directory entries and partial receipts were made durable before each source unlink; source parents and receipt replacements were synchronized immediately afterward. The postcheck matched all five archive copies opaquely, confirmed the intended sources were absent, and verified the seven known retained files and four unattributed files were unchanged. It did not interpret journal records or publish raw archives, trust keys, personal hashes or inode values.

All 1,253 Go files plus the module files retained source aggregate `7886f9740952df3ebff7558eb41e2849b8c37f51f9ed9ddd6eabec3069f42554`. Both release binaries and all five frozen harness files were unchanged. No Go gate or model run was repeated for cleanup.

The machine-readable safe result is [cleanup-summary.json](cleanup-summary.json). The cleanup owner's required evidence was exported safely. After every evidence owner finished, root removed the exact canonical private task directory and independently confirmed its absence at 2026-10-25T08:45:14.291795+00:00. Its release binaries, Go caches, temporary files, original PTY/JSONL data and private cleanup archive were removed. Source, reviewed screenshots, safe evidence and architecture documents remain. The final directory removal was performed by root, separately from the cleanup owner's checks. Live scenario conclusions remain in the separate [evaluation report](report.md).
