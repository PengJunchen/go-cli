# 03: MCP按服务器发现加载与受控调用

**Publication:** unpublished-to-tfs

**Status:** verified-local

**User story:** 作为有多个MCP服务器的用户，我需要选中后才连接并投影工具，以便减少启动等待和schema体积。

**Goal:** 真实CLI lazy模式从server目录选择、Connect/ListTools到prepared dispatch和有界关闭。

**What to build:** 真实CLI lazy模式从server目录选择、Connect/ListTools到prepared dispatch和有界关闭。

**Blocked by:** None (can start immediately)

## Acceptance criteria

- [x] AC-03-1: lazy启动零服务器连接；同一服务器并发激活单飞，只连接所选服务器。
- [x] AC-03-2: 发现不绕过现有trust/approval；工具调用复用prepared dispatch；名称冲突与连接失败不破坏旧代。
- [x] AC-03-3: 既有initialize/ListTools协议向后兼容，分页有cursor循环与资源上限；server/discover仅是新版本研究边界，本轮不冒充2026协议全量实现。
- [x] AC-03-4: 无初始source也保留发现入口；刷新加入首个服务器后，同run发现使下一request声明其schema，再走prepared调用。stdio成功setup的context结束不终止已发布worker，显式关闭才结束owner。

## Executable verification

以下为复跑入口；本轮实际执行的完整 `make verify` 已覆盖对应race测试。

- `go test -race -count=1 ./internal/mcp ./internal/cli ./internal/tools -run "MCP|Deferred|Progressive"`

## Verified evidence

完整 `logs/make-verify-final3.log`（exit 0）包含 `TestProgressiveMCPActivationRefreshesNextRequestSchema`、`TestProgressiveMCPRejectsOversizedCandidateBeforePublication` 两子例、`TestNamedMCPRefreshRejectsChangedProjectGrantBeforeReconnect`、`TestProgressiveRuntimeCanDiscoverFirstCapabilitiesAfterEmptyStartup`、`TestReviewLazyMCPRemainsCallableAcrossOwnedRuns`、`TestMCPGenerationCandidateFailureRetainsAndReplacementJoinsPreparedCalls`、`TestMCPAdditionPreservesPreparedSiblingAuthority`、`TestToolPaginationUsesCursorAndRejectsCycles`、`TestOwnedStdioPublishedProcessSurvivesSetupContextCancellation` 通过。锁内单飞与按server选择经静态接线复核；本轮协议支持范围保持现有协商版本，未实现新协议server/discover。

更早 `spec/logs/mcp-atomicity-final.log` 非race小集通过（0.954s），保留为局部修复过程证据，最终结论以完整gate为准。

## Rollback

mcp.loading=eager回到现有source generation路径；关闭激活owner并保留配置。

## Rule mapping

Interface-First; Trace Logging; existing Mock contracts; go-cli-infra Go validation and verify rules. No protected-file changes.
