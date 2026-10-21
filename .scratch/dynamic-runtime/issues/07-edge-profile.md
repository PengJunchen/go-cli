# 07: 边缘档位与轻量构建保持完整CLI旅程

**Publication:** unpublished-to-tfs

**Status:** verified-local

**User story:** 作为边缘设备用户，我需要更少启动资源和有界负载，以便低配设备仍能交互。

**Goal:** edge档位限制catalog/prompt/并发/连接预算；headless构建避免无用TUI链接。

**What to build:** edge档位限制catalog/prompt/并发/连接预算；headless构建避免无用TUI链接。

**Blocked by:** 02: 标准Skill发现与按需正文进入模型; 03: MCP按服务器发现加载与受控调用; 04: 用户可见的能力选择刷新与失败恢复

## Acceptance criteria

- [x] AC-07-1: edge档位使用progressive Skill+lazy MCP且可配置上限；耗尽返回稳定错误；默认兼容档位不变。
- [x] AC-07-2: headless prompt配置→Assembly→模型→工具→close贯通；审批公共DTO避免TUI依赖。
- [x] AC-07-3: 记录标准/轻量二进制体积、冷暖基准、allocs、prompt/schema字节与平台构建限制；不伪造edge实机性能。

## Executable verification

以下为复跑入口；完整make verify/E2E与正式测量均已实际执行，精确命令、参数和不同口径见结果文件；不声称这里列出的每个宽泛benchmark regex均被原样执行。

- `go test -race -count=1 ./internal/config ./internal/cli ./internal/tui`
- `go test -run "^$" -bench "Assembly|Capabilit|Skill|MCP|Prompt" -benchmem ./internal/cli ./internal/skill ./internal/core`
- `go build ./cmd/cli`

## Verified evidence

[results.md](../../../docs/research/iteration-2026-10-22/results.md) 与 [measurements.json](../../../docs/research/iteration-2026-10-22/measurements.json) 保存最终数据、参数、二进制/源manifest provenance。六个同Go版本、CGO0、固定flags产物build均exit 0：headless darwin/arm64为14,213,890B，current full-UI为22,651,666B；Linux amd64/arm64与Windows amd64只作交叉构建，不是目标设备运行验证。主代理独立核对1233个Go及go.mod/sum文件hash与实测manifest一致。

原始entry-loader→可信临时project→edge→Harness在default/headless两tag race均通过：read/edit/read/final四次请求、结果文件42、未选MCP零请求、Skill正文未披露；4 Skills/1 MCP/131072B预算生效，改为1024B在模型dispatch前拒绝。独立import graph为224包，无Bubble Tea/Glamour/Lip Gloss/Chroma/stdlib plugin；SDK worker/MCP仍可用。

新隔离pilot为10/10；正式五arm、两个场景，每cohort5 warmup+30 measured，共350个唯一调用全部exit 0/无timeout/通过（50 warmup、300 measured、10 cohort）。full/headless交互benchmark各覆盖1k/5k/10k历史规模，均exit 0并记录B/op和allocs/op。两实现功能与历史body不同，不能计算精确等价UI倍率。JSON另存prompt/schema与RSS采样口径，不把schema重编码字节当wire字节/token，不把单父进程RSS当全树。

“冷/暖”这里只记录新进程/新目录与预热协议：OS cache未flush，未建立冷OS缓存或已常驻daemon的等价对照。实物edge设备、edge profile性能、Rust、千能力规模、真实模型/网络质量与token成本均未测；Go性能arm采用default compatibility profile，headless只是build tags。上述未测项不因AC勾选而变为已验证。

## Rollback

选择default档位和标准构建恢复旧入口；不修改保护Makefile/CI。

## Rule mapping

Interface-First; Trace Logging; existing Mock contracts; go-cli-infra Go validation and verify rules. No protected-file changes.
