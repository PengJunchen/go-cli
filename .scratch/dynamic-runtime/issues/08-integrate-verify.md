# 08: 全部功能完成后的整体对抗验证与交付

**Publication:** unpublished-to-tfs

**Status:** verified-local

**User story:** 作为维护者，我需要对真实入口与资源合同统一验证，以便知道实际完成项和剩余风险。

**Goal:** 在所有功能完成后统一执行AC、race、多轮、leak、平台构建及性能对照，汇编可复现证据。

**What to build:** 在所有功能完成后统一执行AC、race、多轮、leak、平台构建及性能对照，汇编可复现证据。

**Blocked by:** 01: 确定性代际进入真实运行; 02: 标准Skill发现与按需正文进入模型; 03: MCP按服务器发现加载与受控调用; 04: 用户可见的能力选择刷新与失败恢复; 05: 公开SDK定制能力被真实CLI消费; 06: 可选子进程扩展的版本预算与退出; 07: 边缘档位与轻量构建保持完整CLI旅程

## Acceptance criteria

- [x] AC-08-1: 不在功能完成前运行测试；最终make verify与相关tag集成/E2E执行，失败修复后重验受影响范围。
- [x] AC-08-2: old run/new generation、取消等待、撤权、回退、长会话与owner关闭交错有真实mock证据。
- [x] AC-08-3: 逐票据记录实现与验证状态；未运行/不支持平台/真实edge与Rust对照缺口明确；quota<25%按执行策略停工归档。

## Executable verification

以下为复跑入口；本轮完整make verify、指定并行度的原E2E target与正式测量均已实际执行，全部任务临时构建/测试数据已实际删除，源码与持久结论保留供审阅。

- `make verify`
- `go test -race -count=1 ./internal/capability ./internal/core ./internal/cli ./internal/skill ./internal/mcp ./internal/extension ./internal/config ./internal/tui`
- `git diff --check`

## Current evidence

最后源 `logs/make-verify-final3.exit` 为0；日志记录8962 started / 8956 passed / 6 skipped / 0 failed，lint 0 issues，scan 466 files / 0 ERROR / 1866 WARN，log 10/10，leak 6/6。WARN比final2增加4项，只是新snapshot getter的test-only SCAN006，未改规则。

同源默认并行E2E final3出现一次既有GitDiff fixture第46行1秒启动标记超时（4877 started / 4870 passed / 6 skipped / 1 failed；Make exit 2）。原fixture与相关production文件和baseline SHA相同；安静条件下原预算 `-race -tags=e2e -count=5` current与baseline分别5/5通过。随后完整原Make target以 `GOFLAGS=-p=1 make test-e2e` 重跑final4 exit 0：4877 started / 4871 passed / 6 skipped / 0 failed（8包），未改源码、断言、timeout或gate。该成功是串行package命令结果，不替代或删除之前的并发失败。

AC-01-1 permutation与AC-05-2真实CLI SDK补证已进入最后源；其他owner/revoke/drain交错由完整gate覆盖。最终六build全部exit 0；新pilot10/10、正式5 warmup/30 measured×5 arm×2 scenario共350/350通过，350 tuple唯一、10 cohort完整；full/headless三规模交互诊断含alloc/B/op并均exit 0。结果与所有实测边界保存在results.md/measurements.json，主代理独立核对Go源manifest一致。实物edge、冷OS缓存、Rust、千能力规模与真实模型未测。

主代理已实际删除唯一任务临时目录并确认不存在；所有任务预览进程已退出，源码无构建产物。8张票据均verified-local；持久文档/JSON保留参数、数值、provenance与限制。可见配额剩余66%，25%暂停阈值未触发。原dev保持干净，独立源码worktree保留供review。

## Rollback

失败时保留证据和WIP，禁用未满足验收的新能力；不降低gate或修改保护文件。

## Rule mapping

Interface-First; Trace Logging; existing Mock contracts; go-cli-infra Go validation and verify rules. No protected-file changes.
