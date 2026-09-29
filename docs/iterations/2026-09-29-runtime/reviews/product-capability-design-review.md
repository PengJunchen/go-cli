# R8 产品能力设计复核

状态：**Frozen ACCEPT**。这是对产品选择和静态源码合同的独立复核，不代表 Runtime 已实现或通过测试。复核基线为当前 checkout `ea8721d`；未运行测试、build、PTY、Docker 或协议 peer 验收。

## 复核结论

修订后的 `product-capability-design.md` 与 R8 的 selected/degraded 合同相容。手动 Memory 是产品 owner 对 9 月 25 日“保留 bounded automatic injection”建议的明确 supersession：它关闭默认 extractor 与 prompt injection，同时保留手工 CRUD/search 和 JSONL 正常重启回读。该选择没有被表述为因为 selector 尚未实现，也没有引入自动模式开关。

R8 要求 supported 有默认入口 integration/restart 证据、partial 在操作前显明限制、unavailable 不以库 API 或 package 存在冒充能力，并要求显式调用未支持操作返回可理解的 unsupported 结果（`r8-capability-design.md:61-80`）。修订稿补上了 `/help` 静态能力边界说明和对应命令/工具/配置错误/用户文档一致性的门槛。该说明写入现有 `HelpHandler`，不被用作 runtime registry 或第二套能力事实来源；`/tools` 仍以实际 registry 为准。

## 关键源码核对

- 入口 discovery 事实准确：`HelpHandler` 只列 slash handler 名称/描述（`internal/cli/slash_handlers.go:32-43`）；`/tools` 列实际 tool registry（`:144-164`）；`/config` 只显示少量配置摘要（`:450-478`）。`doctor` 的实现是环境、配置、工具和连接诊断（`internal/cli/doctor.go:74-93,293-343,424-457,531-569`），不装配 Agent。修订稿没有把这些入口描述成已有 capability status 系统。
- Memory 风险描述与默认链吻合：assembly 从 `FileMemoryStore.List` 构建传入 system prompt 的完整 `Memories` 列表，并配置 `LLMMemoryExtractor`（`internal/cli/assemble.go:1470-1513`）；REPL turn 持久化后异步调用 extractor（`internal/cli/repl_session.go:964-1068`）。因此 manual-only 必须真实移除这两条默认行为，并将 `/memory` 描述更新为人工管理。当前 `bf11439` 已包含文件层 append/delete 修复：完整短写后 `Sync` 才发布 index、确认 rollback 失败返回 typed poison、1 MiB 行上限、rename 后 reopen 失败保留新 index（`internal/memory/file_store.go:171-226,254-279,393-496,517-545`）。poison gate 阻止后续 `Add/Delete`，但 `Get/List/Search` 仍读取故障前的内存 index（`file_store.go:227-252,281-285,465-478`; tests: `file_store_failure_test.go:107-153`）；因此查询结果不能证明 unknown 的磁盘记录不存在。`file_store_failure_test.go` 有相应 fault tests，本审查没有执行它们，也没有 CLI 子进程正常重启证据。多进程或断电耐久不在承诺内。
- Session 选择将候选与当前行为分清：当前 REPL 解析的是 `--resume` bool（`internal/cli/repl_session.go:192-245`），assembly 以 `session.store_path` 构造 JSONL store（`internal/cli/assemble.go:1639-1641`）；`durable_cli_session.go` 明确尚未接到 prompt/REPL（文件注释 13-16）。精确物理 ID resume 可作独立审核后的目标，不代表当前 checkout 已支持；分支/fork/snapshot/migration 边界没有被扩大。
- Remote TUI fail-closed 依据成立：remote 模式只把 `ACPStreamAdapter` 的 SSE 输出接给 TUI（`internal/cli/repl_session.go:790-803`），每个 turn 仍由本地 `TurnRunner.RunTurn` 执行（`:764-777`）。无提交/会话/审批 dispatch 的 adapter 不足以支持 remote TUI。
- Serve 决策必要且 source-faithful：当前默认 `--approve` 是 `ask`（`internal/cli/serve_cmd.go:74`），token 可在 assembly 前生成/写文件（`:124-144`），assembly 失败和无配置都会进入 echo handler（`:151-204`）；HTTP 路由有 `/connect`、`/send`、`/disconnect`、`/stream`、`/events` 而无 approval answer route（`internal/acp/http_server.go:326-330`）。所以必须先装配成功，再产生 token/listener side effect；Ask 需在 side effect 前明确拒绝，并更新 flag help，避免 `default ask` 的旧承诺。
- Unified-only 要求修复真实配置矛盾：当前 `TUIConfig` 注释称空值默认 `auto`，validator 还接受 `split`/`auto`（`internal/config/types.go:541-543`, `internal/config/validator.go:54,124-125`），而 renderer 仅有 unified diff。Remote 模式也在当前 config 类型中可设（`types.go:545-550`），未被 validator 拒绝；配置必须在 provider/tool assembly 前 fail closed。
- Cost 与 Plan Mode 边界没有把库能力误算成 CLI 能力：assembly 调用 `NewCostTracker(nil)` 但不设 limit（`internal/cli/assemble.go:1223-1226`）；stream 路径没有实际 usage 记录（`internal/production/llm_integration.go:129-134`），`/cost` 只打印数值 total（`internal/cli/slash_handlers.go:53-68`）。Hard budget 可以本轮 unsupported，但 R3 每 operation 的 known actual-usage subtotal 和 unknown-usage attempt count 仍是必需工作；unknown usage/price 不可记成 0。Plan policy 的 `PermissionAutoFull` 分支直接允许调用（`internal/approval/prepared_policy.go:338-340`），因此只从 trusted allowlist 移除 mode tools 不充分。修订稿现在要求从 model-facing registry 移除，或在现有 prepared-policy seam 对这两个调用无条件 deny、且早于 `auto_full`。
- MCP config-generation reload 仍是独立 pending 设计（`mcp-generation-design.md`）；当前 per-client reloader 不构成原子配置 generation 公开承诺。修订稿及 help description gate 保持该边界。
- Darwin PTY、Linux/Windows、Docker 范围均按证据分层表述； transcript viewport primitive 不冒充默认 REPL 已接线。竞品比较所引 [Pi terminal](https://pi.dev/docs/latest/usage)、[Pi sessions](https://pi.dev/docs/latest/sessions)、[OpenCode TUI](https://opencode.ai/docs/tui/) 与 [OpenCode permissions](https://opencode.ai/v2/docs/permissions) 官方页面在复核日的说明支持文档中描述的功能范围；比较不作为新增 go-cli 功能验收。

## 实施边界

当前源码仍不满足这些目标：没有 `/help` capability-boundary 段、manual-only Memory、remote TUI 早期拒绝、Serve fail-before-side-effect、unified-only config validation 或 Plan Mode model-call deny。JSONL store fault handling 已有当前源码和 test cases，但本次没有执行测试，且真实 CLI 正常退出/重启 journey 仍待验收。因此本结论只冻结产品选择并接受文档合同；R8 builder 和 reviewer 必须按目标 checkout 做运行与入口验收，不能引用本审查当作通过证据。R5 app wiring 与 R5/R6/R7 前置 review 仍是独立 gate。
