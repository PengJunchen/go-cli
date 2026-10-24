# 02: 标准Skill发现与按需正文进入模型

**Publication:** unpublished-to-tfs

**Status:** verified-local

**User story:** 作为Skill生态使用者，我需要标准SKILL.md包和旧格式都可搜索与按需加载，以便安装更多技能不撑大启动上下文。

**Goal:** 从真实Assembly发现索引，经模型可见搜索/加载或slash选择，使所选正文到达模型。

**What to build:** 从真实Assembly发现索引，经模型可见搜索/加载或slash选择，使所选正文到达模型。

**Blocked by:** None (can start immediately)

## Acceptance criteria

- [x] AC-02-1: 标准SKILL.md元数据与正文分离；references/scripts/assets不被注册为技能；legacy格式兼容。
- [x] AC-02-2: 项目仅消费已验证捕获字节，信任撤销立即阻止执行；路径逃逸/symlink/限额越界明确失败。
- [x] AC-02-3: 搜索稳定有界；正文不在初始prompt；显式选择影响后续模型上下文，加载失败可诊断。

## Executable verification

以下为复跑入口；本轮实际执行的完整 `make verify` 已覆盖对应race测试。

- `go test -race -count=1 ./internal/skill ./internal/cli -run "Skill|Progressive"`

## Verified evidence

`logs/make-verify-final3.log`（exit 0）包含 `TestStandardCatalogDiscoversOnlyEntryPointAndLoadsLazily`、`TestCapturedStandardCatalogOwnsBytesAndRejectsDuplicates`、`TestProgressiveSkillInstructionsReachRealLoopOnlyAfterLoad`、`TestProjectSkillLoadReportUsesVerifiedCapturedBytes`、`TestProjectSkillToolStaysRevokedUntilFreshAssembly`、`TestSkillLoaderHonorsLimitsAndCancel`、`TestWalkSkillDirectoryRejectsConcurrentAncestorSymlinkSwap`、`TestSkillLoadSourcesRejectsUncleanAndDuplicatePathsWithoutPartialDefinitions` 通过。未读取的用户正文仍不属于metadata digest；实际 `skill_load` 内容摘要有单独receipt/session回归。

## Rollback

禁用progressive保持现有YAML deferred路径；不修改用户技能包。

## Rule mapping

Interface-First; Trace Logging; existing Mock contracts; go-cli-infra Go validation and verify rules. No protected-file changes.
