# Safe-read quarantine final review

**Verdict: ACCEPT** for source commit `b76686280f21a8f4bb4b144ae193c3f9bfaf227e`, directly atop accepted product commit `25b88ba890f59be488e2ea5defbf09186baf873d`. The isolated worktree was clean at review start and remained unchanged. I reviewed the frozen design [`quarantine-read-design.md`](../quarantine-read-design.md), SHA-256 `bf39d5b7ac5dbc99a31a6cfc28a01ea1ec2b80b604d28e5aba49c653ab7cc957`. No safe-read builder report was present in the iteration reports.

## Findings

- **Admission is tied to the real prepared reader.** `ScopedWorkspaceReadProofFor` accepts only the package-private prepared invocation marked eligible by concrete built-in `ReadTool` or `LSTool` registration. It requires the single `workspace_read` effect, joined inline completion, a non-empty canonical whitelist contained by the captured canonical workspace root, and an exact scope fingerprint. The opaque proof binds registration identity, schema and normalized argument digests, call ID, captured fence generations, workspace root, and whitelist roots; it can be minted once and claimed once ([`scoped_workspace_read.go`](../../../../internal/tools/scoped_workspace_read.go:29)).

- **The session sink rechecks the prepared intent before persisting it.** It rederives the workspace ID from the runtime root and matches the proof against the intent identity, effects, schema digest, argument digest, call ID, and root. It writes and syncs the session intent before asking the workspace index for a marker ([`run_commit.go`](../../../../internal/session/run_commit.go:286)). The core consumer dispatches only after this call succeeds. The index syncs and reloads its locked journal before admission, requires proofs for every action while unresolved markers exist, claims them once, and returns no permit for errors or `AlreadyExists` ([`workspace_index.go`](../../../../internal/session/workspace_index.go:240)).

- **No-intent denials stay outside the action authority.** The session validator accepts only matching `error` or `interrupted` receipts with no action ID and `not_dispatched` state; it rejects `unknown` receipts before writing or entering the index. Dispatch actions are constructed from `Intents`, so a denial cannot supply a proof or marker. The added session tests cover a valid known-error denial accompanying a scoped read and an unknown denial rejected with zero reader calls.

- **The workspace boundary is coherent across sessions.** The durable manager captures and canonicalizes the invocation CWD once, derives the domain-separated workspace ID from that same root, and carries both through the run lease and binding. The index rederives the ID from the runtime root before accepting a proof. Durable default Read/LS tools receive the captured root as their effective whitelist; configured sandbox paths remain the reader whitelist, and scopes outside the workspace fail proof issuance. Without a durable root, ordinary default read behavior remains unchanged.

- **I found no write admission path through this proof.** Proof matching requires exactly `workspace_read`; batches with a missing proof, mismatched root, composite effects, or a read mixed with a write fail while quarantine is active. Runtime roots and proofs are `json:"-"`; the proof has only private fields, and tests verify that JSON does not reveal its root, tool identity, call ID, or effect facts. A failed global marker sync grants no executor permit, and a retry observes `AlreadyExists` without replay.

## Validation

- `rtk go test -race ./internal/tools ./internal/core ./internal/session ./internal/cli` — passed, 3,289 tests across four packages.
- Focused safe-read and denial probes under race detection — passed, 7 tests across `internal/tools` and `internal/session`.
- `rtk go build ./...` — passed.
- `rtk go test -run '^$' ./...` — exited successfully; RTK reported no tests run.

## Limitations

The path whitelist and subsequent file open remain check-then-use operations. Concurrent symlink replacement can race those checks; the code and design do not claim an OS sandbox or atomic filesystem protection.

The focused tests cover unresolved cross-session quarantine, actual prepared read execution, duplicate and cloned proof denial, and global index sync failure. They do not separately exercise a safe read through recovery followed by resolving only its own marker, or inject a session `AppendAndSync` failure for this scoped-read case. The source ordering returns before core dispatch if session sync fails, but those two acceptance cases lack dedicated evidence here. The separate R2-REV-01 real builtin CLI subprocess SIGKILL/live-child matrix was not run; this review makes no child-process quiescence or production-runtime claim.
