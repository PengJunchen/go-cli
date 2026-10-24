# Interactive 测试入口存储隔离覆盖表

范围：当前 worktree 的 `internal/cli/*_test.go` 中全部 `newInteractiveCmd(` 调用，逐项追到实际配置和 editor 来源；没有扩成全仓库行为审计。源码穷举实际为 **33 个调用位置、31 个测试函数**，另见下方 4 处直接 struct 构造。早先的“40 处”不是当前源码计数，不能据此虚记覆盖。此表为只读来源审查；最新统一双 tag 验收结果由日志另记，不把静态结论当作执行通过。

仅检查三种个人路径回退：Memory、History、production Audit。Session 模式、权限、Git、sandbox 和 Audit.Enabled 均保留原值；不声称本表审计了其他持久化系统。没有读取、截断或重写个人文件。

| 标记 | 已核对的来源 |
|---|---|
| P | `newPromptConfig(t, url)`：三路径均显式位于该测试临时目录。 |
| D | `durablePromptConfig(t, ...)` 继承 P；所有真实 REPL 均注入测试 editor，Session 路径按原场景保持。旧 durable HOME helper 不在本次改动范围。 |
| E | `newE2ETestConfig(t, url)`：三路径显式临时；已移除该 helper 的 HOME 覆盖，不改变工作目录或 sandbox。 |
| M | 既有 `newMemoryTestConfig` 专门以临时 HOME 验证默认 Memory 路径；History 同样落在这个临时 HOME，Audit 未启用。保留这些目的明确的旧 fixture，不新增 HOME 覆盖。 |
| A | `newAssembleTestConfig(t)`：三路径显式位于 owned 工作目录。 |
| N / X | 不执行 Run，或在 storage setup 之前拒绝；不是以空路径运行真实 editor。 |

| 测试函数 | 全部调用位置 | 来源 | 语义核对 |
|---|---|---|---|
| `TestPendingInput_REPLIntegration` | `command_loader_test.go:314` | P | 默认 editor；保留原 Commands.Dir。 |
| `TestInteractiveDurableSessionReconcilesUnknownActionThroughSlashRoute` | `durable_cli_reconciliation_test.go:153` | D | 注入 scriptedLineEditor；原 recovery/session 语义保留。 |
| `TestInteractiveDurableSessionKeepsOnePhysicalSessionAcrossTurns` | `durable_cli_wiring_test.go:243` | D | 注入 scriptedLineEditor；原 session 路径保留。 |
| `TestInteractiveWithoutSessionStoreReportsSessionCommandsUnsupported` | `durable_cli_wiring_test.go:311` | D | 注入 scriptedLineEditor；Session.StorePath 继续为空。 |
| `TestInteractiveConfiguredStoreRejectsBooleanResume` | `durable_cli_wiring_test.go:340` | D | 注入 editor；旧 --resume 在 setup 前被拒绝。 |
| `TestInteractiveDurableResumeUsesExactPhysicalSessionAfterRestart` | `durable_cli_wiring_test.go:360`, `durable_cli_wiring_test.go:371` | D | 两个 physical invocation 共用原 cfg/session；均注入 editor。 |
| `TestInteractiveDurableHistoryMutatorsAreUnsupportedAndReadOnlySlashStillWorks` | `durable_cli_wiring_test.go:418` | D | 注入 scriptedLineEditor；原 durable 路由断言保留。 |
| `TestE2E_ContextCancellationViaInteractive` | `interactive_e2e_advanced_test.go:45` | E | 默认 editor；取消场景。 |
| `TestE2E_SessionPersistenceViaInteractive` | `interactive_e2e_advanced_test.go:81` | E | 默认 editor；已有显式 Session.StorePath 不变。 |
| `TestE2E_CompactionViaInteractive` | `interactive_e2e_advanced_test.go:146` | E | 默认 editor；compaction 场景。 |
| `TestE2E_MultiTurnConversationViaInteractive` | `interactive_e2e_advanced_test.go:189` | E | 默认 editor；多轮对话。 |
| `TestE2E_EOFExitsCleanly` | `interactive_e2e_advanced_test.go:221` | E | 默认 editor；EOF 关闭。 |
| `TestE2E_NonTTYApprovalDenied` | `interactive_e2e_tools_test.go:116` | E | 默认 editor；原 Git.WorkDir/sandbox/approval 不变。 |
| `TestE2E_SlashCommandsViaInteractive` | `interactive_e2e_tools_test.go:148` | E | 默认 editor；slash 输入。 |
| `TestE2E_ErrorRecoveryViaInteractive` | `interactive_e2e_tools_test.go:192` | E | 默认 editor；provider 错误恢复。 |
| `TestInteractiveCmd_RunConversation` | `interactive_extra_test.go:401` | P | 默认 editor；原 provider 与对话断言保留。 |
| `TestInteractiveCmd_MaxTokensFlag` | `interactive_extra_test.go:428`, `interactive_extra_test.go:437` | P | 两个子例均经 makeCfg(t)，保留 1000/0 flag 语义。 |
| `TestInteractiveMemoryIsManualOnlyByDefault` | `interactive_memory_test.go:207` | M | 默认 editor；验证已有 memory 不被自动注入/提取。 |
| `TestInteractiveMultipleTurnsDoNotExtractMemoryByDefault` | `interactive_memory_test.go:228` | M | 默认 editor；验证多轮不自动写 memory。 |
| `TestMemoryRequestBodiesAreValidJSON` | `interactive_memory_test.go:244` | M | 默认 editor；验证 memory 请求形状。 |
| `TestInteractiveMemoryStoreIsManualOnlyWhenNoTurnsRun` | `interactive_memory_test.go:259` | M | 默认 editor；验证手动 memory CRUD。 |
| `TestInteractiveRunReturnsTTYInputSetupErrorAfterCleanup` | `interactive_read_error_test.go:21` | E | 注入失败 editor；仍使用隔离配置。 |
| `TestInteractiveRemoteModeRejectedBeforeSessionSetup` | `interactive_remote_test.go:21` | A | newAssembleTestConfig 三路径显式；remote 在 session/editor setup 前拒绝。 |
| `TestInteractiveCmd_Metadata` | `interactive_test.go:24` | N | 只检查命令 metadata，不调用 Run/Assembly/editor。 |
| `TestInteractiveCmd_ExitCommand` | `interactive_test.go:43` | P | 默认 editor，即使只有 exit 也不回退个人 history。 |
| `TestInteractiveCmd_EmptyInput` | `interactive_test.go:65` | P | 默认 editor；空行与 exit 的原语义保留。 |
| `TestInteractiveCmd_FlagParsing` | `interactive_test.go:77` | X | 未知 flag 在 Assembly/editor/storage setup 前返回 UsageError。 |
| `TestPhase39_REPLSessionCompleteTurn` | `phase39_e2e_test.go:282` | M | 默认 editor；原 memory 默认路径断言保留。 |
| `TestDurableREPLProviderLocalTimeoutRefreshesAndContinues` | `repl_local_timeout_test.go:102` | D | 注入 scriptedLineEditor；原 timeout/session 语义保留。 |
| `TestDurableREPLBashPolicyRejectionIsKnownAndNextTurnContinues` | `repl_policy_rejection_test.go:74` | D | 注入 scriptedLineEditor；原 sandbox/session 语义保留。 |
| `TestDurableREPLUnknownUserCancellationRequiresExplicitNewSession` | `repl_unknown_cancel_recovery_test.go:119` | D | 注入 gated editor；原 unknown/recovery 语义保留。 |

四处直接 `&interactiveCmd{...}` 均只调用 `handleSlashCommand`，没有启动 Run、Assembly 或 editor：`slash_test.go` 的 `newTestCmd`、`TestSlashCommandsTableDriven`，`r4_plan_shell_hook_test.go` 的 `r4SetPlanModeViaSlash`，以及 `r1_assembly_stream_test.go` 的 `TestAutoFullModelCannotChangePlanModeButUserSlashCan`。实际模型/工具 Assembly fixture 自身的隔离不以这条“不启动 editor”的事实代替。

额外授权的 cmd 入口只核对三个既有场景：`TestEdgeEntryLoaderReadEditRead` 与 `TestPublicSDKExtensionCommandAndProviderThroughCLIEntry` 的项目配置明确设置 owned 三路径，继续保持原空 Session.StorePath；`TestDefaultInteractiveTraceContainsNoPrompt` 保持原临时 Session/History，只补同一 workDir 的 Memory/Audit 路径。均不改变 Audit.Enabled 或正式 binary。

最后一次遗漏为 `TestInteractiveCmd_MaxTokensFlag` 的局部 makeCfg：两个只输入 exit 的子例仍会触发 History.Save，即使内容被去重也会改 mtime。本次只使 makeCfg(t) 复用 P，保留全部 flag 和原断言。此前 p2 因 history mtime-only 变化被中止；不会把中止的 gate 或此前 targeted 结果改记为最终整体验收。
