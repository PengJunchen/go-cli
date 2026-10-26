# Live Magpie terminal E2E evidence

- Status: **failed**
- Model: `codex/gpt-6.1-sol`
- Magpie session: `go-cli-live-20261014-a2a708962b5c`
- Run nonce: `a2a708962b5c`
- Race detector clean: `None`
- Evidence class: real go-cli binary + real PTY + requests transparently forwarded to signed-in Magpie Codex. No model responses are synthesized.
- UI evidence: actual PTY bytes rendered by xterm.js in headless Chromium; screenshots are not native Terminal captures.
- Credential handling: isolated CLI HOME, dummy local API sentinel, no stored Authorization headers, request bodies, or full response bodies.

## Checks

- FAIL: live PTY produced actual Chat Completions inference requests

## Screenshots


## Provider proof

Route and usage audit rows are in `run-report.json`; upstream headers and response IDs were captured only as non-secret metadata.

## Failure

```text
Traceback (most recent call last):
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 1500, in run_live
    session = LivePTY(binary, home, cwd, bridge, node, model, max_tokens)
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 553, in __init__
    child_env = self.child_env()
  File "/Users/yinyuepeng/projects/go-cli/scripts/e2e_live/run.py", line 588, in child_env
    "GO_CLI_PROVIDER_MODEL": self.model,
                             ^^^^^^^^^^
AttributeError: 'LivePTY' object has no attribute 'model'

```
