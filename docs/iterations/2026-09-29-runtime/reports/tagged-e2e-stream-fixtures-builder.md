# Tagged streaming E2E fixture repair

**Baseline:** `33739e308ccf4cd858d03367e6be9f9c516bacec`  
**Branch:** `fix/tagged-e2e-stream-fixtures`

## Finding and change

The Phase20 and Phase39 failures came from streaming fixtures that closed without the terminal protocol data required by the current provider/core contract. Claude text events omitted block lifecycle and terminal `stop_reason` data; the Claude tool-use fixture omitted block stop and terminal stop reason; the OpenAI fixture sent `[DONE]` without a `finish_reason`; and the Phase39 mock closed its channel without a `Final` chunk.

The fixtures now emit complete Claude block/message sequences, an OpenAI terminal `finish_reason` before `[DONE]`, and a separate final chunk from the Phase39 mock. Assertions continue to check streamed content, exactly one Final chunk, the terminal reason, and fully assembled tool-call arguments. No production code changed.

Before the fixture edits, the targeted race runs reproduced all four failures: Claude content/final assertions failed, OpenAI and Claude tool-use streams lacked Final, and Phase39 returned `core: stream ended without a final chunk`.

## Validation

- `rtk env GOMAXPROCS=2 GOFLAGS='-p=2 -trimpath' go test -tags=e2e -race -count=5 ./tests/e2e_20260802 -run '^TestET_Phase20_Streaming_AC(2_ClaudeStream|3_OpenAIStream|6_ClaudeToolUse)$'` — passed.
- `rtk env GOMAXPROCS=2 GOFLAGS='-p=2 -trimpath' go test -tags=e2e -race -count=5 ./tests -run '^TestET_phase39_model_switching$'` — passed.
- `rtk git diff --check` — passed.

This repairs only the assigned Phase20 and Phase39 fixtures. The full `-tags=e2e ./tests/...` gate was not run here and is not claimed to pass.
