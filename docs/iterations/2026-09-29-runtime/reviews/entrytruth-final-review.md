# Independent review: runtime entrypoint truth

## Verdict

**PASS for the scoped entrypoint requirements at 061246bf16b73d276a67c72c6d43c9adf7e48a55.** I found no blocking gap in the requested serve, remote TUI, diff-style, ownership, or safety behavior.

The review target was clean at that exact HEAD. Its product change is 77b7c948e095085f482e5a0227a2f1a87b328e35 over 1144e1ebc9f339a04d502f0bdca04069c9d30ff5; the governing design was read from frozen commit 25577ec5b3876439216ae2423014d8719efed1db.

## Evidence

- serve rejects the implicit zero-value ask and explicit --approve ask immediately after argument parsing, with UnsupportedServeApprovalError wrapping ErrServeRemoteApprovalUnsupported. Its error directs callers to auto or deny. The test checks default, explicit, and --no-auth ask cases; it verifies the assembly factory is not called, an existing token file is unchanged, and no listening message appears (internal/cli/serve_cmd.go:94-106, internal/cli/serve_cmd_test.go:66-135).
- Runtime admission is fail-closed. A non-*config.Config, assembly error, or nil dispatcher returns an error before token generation/write or server startup. Assembly errors are joined with cleanup errors, preserving the original cause. The HTTP server is registered with the assembly close owner before startup; later token, registration, and start failures close the assembly (internal/cli/serve_cmd.go:137-174, internal/cli/serve_cmd.go:192-238). Tests cover missing runtime, real assembly failure with the private hook-command canary, and a factory returning a partially built assembly plus an error; the partial owner is closed and the original error remains discoverable (internal/cli/serve_cmd_test.go:572-754).
- The successful explicit-deny test invokes the production AssembleAgent factory and starts the server on loopback. The handler receives the assembled dispatcher and event bus; the default CLI path cannot construct a nil-dispatcher echo handler (internal/cli/serve_cmd.go:177-184, internal/cli/serve_cmd_test.go:143-176). The fixture constructs an OpenAI-compatible model object with a test key and unreachable local URL; assembly/startup do not make a provider request (internal/cli/assemble_test.go:18-31). This is assembly/startup evidence, not live provider dispatch or ACP peer interoperability evidence. The standalone ACP HTTP library behavior was not changed.
- Remote TUI rejection is the first operation in interactiveCmd.Run, before REPLSession construction or setup, and returns a typed unsupported error. Its test also verifies no session file was created (internal/cli/interactive.go:86-89, internal/cli/interactive_remote_test.go:13-36). The ordinary local prompt path is unchanged by this guard.
- Loaded defaults set tui.diff_style to unified; the validator allows only empty (omitted) and unified, while split and auto fail validation (internal/config/loader.go:298, internal/config/validator.go:54, internal/config/validator.go:124-125). Config tests cover the loaded default and both rejected values (internal/config/tui_config_test.go:10-18, internal/config/tui_config_test.go:34-61).
- I ran the user-facing serve --help against this checkout. It displays “approval mode: auto|deny (ask is unsupported; choose explicitly)” and does not append “(default ask)”. Existing auto-authentication and non-loopback security checks remain in place and are covered by the serve and TLS tests.

## Verification

- rtk go test -race -count=1 ./internal/cli ./internal/config — passed; 1,175 tests across both packages.
- rtk proxy go run ./cmd/cli serve --help — printed the updated flag help. The CLI currently returns its usage-error exit status for this help request; the displayed approval-mode guidance is truthful.

No product files were changed for this review. The scoped race run and help smoke do not establish full make verify, a live provider conversation, remote approval round-trip, or official ACP conformance.
