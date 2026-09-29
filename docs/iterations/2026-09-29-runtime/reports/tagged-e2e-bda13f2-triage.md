# Tagged E2E gate failure at bda13f2 product source

The required `-tags=e2e ./tests/...` gate is **red** despite the passing default `make verify`. The repository JSON test gate ran with race and `-count=1` and exited 1: 786 test functions started, 743 passed, 30 failed, and 13 skipped across two packages. An independent direct JSON-event run on the same product source also exited 1 with 31 failures; the extra failure was the intermittent `TestE2E_Phase46_StdioAdapterNoGoroutineLeak`. The direct event log is `/tmp/go-cli-tagged-e2e-bda13f2.jsonl` (local diagnostic, not a release artifact). Do not treat either run as passing evidence.

| Group | Failing tests | Observed boundary | Next check |
|---|---:|---|---|
| Phase22 production/all-capabilities | 20 | Shared Assembly fixture fails before the behavior under test: configured OpenAI endpoint has an invalid port. | Correct fixture to a valid local endpoint without bypassing provider validation, then rerun these tests. |
| Phase19 approval/Plan Mode | 4 | Two legacy opaque wrappers are rejected by prepared dispatch; model-facing PlanMode tool no longer exists by product policy, while the user command remains. | Rework fixtures to test supported prepared tools/user mode control; preserve fail-closed behavior. |
| Phase20 provider streams | 3 | Claude fixtures omit the current block/terminal sequence; OpenAI fixture has no terminal finish reason. No Final chunk is observed. | Supply protocol-valid mock events and retain Final assertions. |
| Phase39 model switch | 1 | Mock stream ends without a Final chunk and CoreLoop reports that exact error. | Make the mock emit a truthful terminal chunk; keep CoreLoop's rejection. |
| Phase21 TUI source check | 1 | Test opens module import path as a filesystem path and gets `no such file or directory`. | Resolve source from the package/repository location and preserve width assertion. |
| Phase23 subagent inbox | 1 | Sends while the subagent state is idle; the current lifecycle rejects it. | Establish a running owner in the fixture or assert explicit idle rejection, according to the product contract. |
| Phase46 ACP stdio | intermittent 1 | One direct run timed out with adapter writer/cleanup goroutines still present; first gate run did not fail this test. | Repeat in isolation and inspect lifecycle/cleanup; do not dismiss as a harmless flake. |

These are observations from real failing runs, not accepted fixes. Some tests are stale relative to deliberately tightened contracts, but each must be repaired to preserve a meaningful acceptance test. The required gate must pass on the final fixed source commit.
