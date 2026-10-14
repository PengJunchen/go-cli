# Approval and tool execution: binary E2E evidence

**Evidence class:** real CLI binary E2E against a local mock HTTP provider
**Baseline revision:** `develop` at `e77e8058c51e5370a2ecc3eacb5fce0419829bcf`
**Scope:** tool execution, approval modes, recoverable tool errors, and durable session resume

## Setup and method

`tests/e2e_binary_tools_test.go` builds a race-instrumented `./cmd/cli` and invokes that binary in isolated temporary home and work directories. The harness fails if the child reports `DATA RACE`. The binary loads its ordinary YAML config and sends OpenAI-compatible chat-completion requests to a local `httptest` server. The server returns scripted tool calls, records subsequent model requests, and verifies that tool results are sent back to the model. File assertions observe actual side effects in the isolated work directory. No external provider credentials or services are used.

The baseline revision identifies the comparison point; the binary was built from the validation working tree, which also contained the changes under review.

## Acceptance evidence

`TestRealBinaryPromptToolApprovalMatrixAndErrorRecovery` passed for `read`, `write`, `edit`, and `bash` on this macOS host:

- Explicit `--approve auto` executes each call; read output reaches the provider, and write/edit/bash side effects match the scripted arguments.
- Explicit `--approve deny` returns a tool result to the provider and leaves write/edit/bash targets unchanged; read leaves its pre-existing fixture unchanged.
- With the default approval mode and an allowed-path scope, a read succeeds. A non-TTY write is denied and creates no file.
- An edit with a nonmatching old string returns a recoverable error to the model. A corrected follow-up edit then succeeds, and the model receives both tool results.

`TestRealBinaryPromptResumesDurableSession` also passed. The first invocation writes the session record; a second binary process resumes the returned session ID, preserves that ID, and sends prior user/assistant context together with the new user turn to the provider.

## Commands and results

Both targeted runs passed from the repository root:

```text
rtk env GOCACHE=/private/tmp/dot-go-cli-e2e-20261014-gocache GOMODCACHE=/private/tmp/dot-go-cli-e2e-20261014-gomod go test ./tests -run 'TestRealBinaryPrompt' -count=1 -v
PASS: package tests, 8.200s

rtk env GOCACHE=/private/tmp/dot-go-cli-e2e-20261014-gocache GOMODCACHE=/private/tmp/dot-go-cli-e2e-20261014-gomod go test -race ./tests -run 'TestRealBinaryPrompt' -count=1
PASS: package tests, 58.167s (parent test process and spawned CLI binaries race-instrumented)
```

The repository's documented policy is deny-first with explicit approval (`go-cli-infra/design/03-go-cli-roadmap.md`, line 11). The observed non-TTY default write denial is consistent with that policy; this test does not change the existing `--approve auto` or `--approve deny` behavior.

## Limits and interpretation

This verifies the real CLI binary, tool middleware, local filesystem effects, provider protocol round trips, and session persistence/resume. The LLM is scripted, so this does not measure model quality or validate a live provider. Bash is skipped on Windows; on this host it is exercised with `--no-sandbox`, so the test proves approval gating and command side effects rather than OS sandbox isolation.

The earlier approval-bypass suspicion came from a stale `4203619` checkout and is withdrawn. It was not reproduced on this `develop` revision; no approval production code was changed for this evidence.
