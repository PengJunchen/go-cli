# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `None`
- Magpie session: `None`
- Run nonce: `None`
- Race detector clean: `None`
- Evidence class: real go-cli binary + real PTY + requests transparently forwarded to signed-in Magpie Codex. No model responses are synthesized.
- UI evidence: actual PTY bytes rendered by xterm.js in headless Chromium; screenshots are not native Terminal captures.
- Credential handling: isolated CLI HOME, dummy local API sentinel, no stored Authorization headers, request bodies, or full response bodies.

## Checks


## Screenshots


## Provider proof

Route and usage audit rows are in `run-report.json`; upstream headers and response IDs were captured only as non-secret metadata.

## Failure

```text
Traceback (most recent call last):
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1277, in main
    case_report = run_live(args.gateway, args.model, run_dir, evidence, args.max_tokens, args.timeout, args.smoke_only)
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1074, in run_live
    report["turns"].append(wait_turn(session, bridge, label, evidence, report["approvals"], report["screenshots"], timeout))
                           ~~~~~~~~~^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 785, in wait_turn
    raise TimeoutError(f"turn {label} did not become idle in {timeout}s; screen={session.screen()[-1200:]}")
TimeoutError: turn inspect-fixtures did not become idle in 300.0s; screen=so remember this early fact for a later question after context compaction: LIVE_MEMO [receipt: success] RY_247ABD3D8F47=cobalt. Do not repeat the memory fact yet and do not use tools. [tool call_dde453be4235494f9c0ae96287a2d1d6] [assistant] [source output not truncated] LIVE_TEXT_OK_247ABD3D8F47 [receipt: success] [assistant · live] I’ll list the workspace, search for both fact markers, and read the matching files u sing read-only tools. [assistant · live] core: tool completion did not prove local resource quiescence: core: tool completion did not prove local resource quiescence codex/gpt-6.1-sol | turn #2 | interact | chat Tokens: 27256/0 (0%) | Cost: unknown Session ended. error: interactive: operation failed: operation failed: interactive: owner publication did not match the joined durable run (quiescent=true outcome_known=false run_match es=true session_matches=true owner_generation_present=true) Hint: Try again. If the problem persists, run `go-cli doctor` to diagnose common issues or contact support.

```
