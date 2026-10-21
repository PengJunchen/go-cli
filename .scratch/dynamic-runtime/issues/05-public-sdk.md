# 05: 公开SDK定制能力被真实CLI消费

**Publication:** unpublished-to-tfs

**Status:** verified-local

**User story:** 作为外部扩展作者，我需要无需internal包的SDK并让command/provider/hook/prompt注册真正生效，以便独立定制CLI。

**Goal:** 公开SDK和内部Adapter贯通extension注册、命令路由与模型resolver。

**What to build:** 公开SDK和内部Adapter贯通extension注册、命令路由与模型resolver。

**Blocked by:** 01: 确定性代际进入真实运行

## Acceptance criteria

- [x] AC-05-1: 外部示例不import internal；未知版本/能力与重复名称在执行前拒绝。
- [x] AC-05-2: 真实CLI扩展command可路由，provider在模型选择前可用；hooks/prompt按稳定计划消费。
- [x] AC-05-3: 内建路由优先；project extension沿用捕获与trust；.so更新明确restart-required。
- [x] AC-05-4: Manifest Requires/Provides在真实Assembly中验证并按拓扑Init，缺失/循环/重复provider在Init前拒绝；失败后清理已经Load/握手获得的资源。

## Executable verification

以下为复跑入口；本轮实际执行的完整 `make verify` 已覆盖对应race测试，外部consumer另有独立build/dependency证据。

- `go test -race -count=1 ./pkg/extensionapi/v1 ./internal/extension ./internal/cli`
- `go list -deps ./tests/external-extension`

## Verified evidence

完整gate `logs/make-verify-final3.log` exit 0；API golden、handshake/checksum拒绝、explicit extension provider的credential-free权限、project extension trust/capture、hook adapters、manifest拓扑/失败依赖/cleanup及真实Assembly拒绝无效图均通过。`upstream/public-sdk-consumer/result.json` 记录GOWORK=off、CGO=0的独立module build与依赖检查，0 internal依赖；原生模块热卸载不在支持范围。

AC-05-2 补证 `TestPublicSDKExtensionCommandAndProviderThroughCLIEntry` 实际构建只依赖public SDK的外部worker，经过持久trust grant与原loader，调用extension-hello及entrysdk/model并断言marker/握手/Wait后的artifact清理。default/headless的cmd/cli race分别2.661s/2.573s，两tag目标lint均0 issues（`runtime/final-entry-default-race-3.log`、`final-entry-headless-race.log`与对应lint日志）。首次真实provider消费发现成本归属原白名单只接受HTTP；窄修仅允许extension-process/extension-rpc精确组合，production正负例两tag race均通过。

该修复已进入最后源完整make verify final3并exit 0；完整E2E final4以GOFLAGS=-p=1运行原Make target并exit 0。前次默认并行E2E的既有GitDiff fixture超时及current/baseline各5次安静复验保留，不用成功重跑抹去失败。

## Rollback

保留旧extension Interface，禁用新增manifest/SDK投影后回到旧路径。

## Rule mapping

Interface-First; Trace Logging; existing Mock contracts; go-cli-infra Go validation and verify rules. No protected-file changes.
