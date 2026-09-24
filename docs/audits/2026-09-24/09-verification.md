# 验证与测试覆盖审计

审计对象是 `go-cli` 在 `2026-09-24` 的干净基线 `40692e35e4815f0dff1c9f91b13905008f7221a4`。基线采样时 `git status --porcelain` 为空；执行前 `gofmt -l` 与 `gofmt -s -l` 均无输出。本验证 lane 只在审计目录写报告、inventory 和运行日志，没有修改产品源码或受保护配置。原始命令输出保存在 [`evidence/`](evidence/)，按 Git 跟踪文件制作的逐文件快照在 [`source-inventory.csv`](source-inventory.csv) 与 [`source-inventory.json`](source-inventory.json)。行数按文件物理行计数；inventory 记录基线 commit、路径、模块、种类、行数和 SHA-256。

## 执行结果

| 命令 | 结果 | 可支持的结论 |
|---|---|---|
| `make verify`，主机原始环境 | 失败，37 个 TUI ANSI 样式断言失败，停在 `check` 的 `test` 阶段；后续 scan/log/leak 没有由该次命令执行 | 原始日志 [`make-verify.log`](evidence/make-verify.log)。在该主机上 `NO_COLOR=1`、`TERM=dumb`；失败来自测试依赖 ANSI 的断言与颜色环境冲突。它是真实的测试治理失败，不能据此认定 TUI 产品功能错误。 |
| `make verify`，只清空 `NO_COLOR` 后重跑 | 测试阶段通过，随后 `scan` 失败（20 ERROR）；Make 因 scan 失败而未继续调用 test-log/test-leak | 原始日志 [`make-verify-no-color.log`](evidence/make-verify-no-color.log)。这证实完整 gate 仍不能在当前提交复现通过。 |
| `make test`，`NO_COLOR=` | 通过，所有默认标签的 `internal/...` 与 `tests/...` 包通过 `-race` | [`make-test-no-color.log`](evidence/make-test-no-color.log)。`make test` 不包括 `cmd/cli`。 |
| `go test -race -count=1 ./cmd/cli` | 通过 | [`go-test-cmd-cli.log`](evidence/go-test-cmd-cli.log)，补齐 Makefile 默认测试目标漏掉的一个命令包。 |
| `make test-cov-check`，`NO_COLOR=` | 通过，汇总覆盖率 84.2%，高于 65%；`internal/term` 为 0.0% | [`make-test-cov-check-no-color.log`](evidence/make-test-cov-check-no-color.log)。这是全仓聚合门槛，不代表每包达标。 |
| `make vet` / `golangci-lint` / build（`make verify` 的 check 阶段） | 均通过；lint 报 0 issues | 包含在上述 `make-verify.log` 和 `make-verify-no-color.log`。本地 Go 是 1.26.4，CI 声明 Go 1.24；本地缺少 `goimports`，`make fmt` 因此按配方提示跳过它。CI 安装 goimports，故本机没有验证该项。 |
| `make scan` | 失败，313 个生产 Go 文件中报 20 ERROR、1273 WARN | [`make-scan.log`](evidence/make-scan.log)。这些是规则命中数，不等同于产品缺陷数。 |
| `make test-log` / `make test-leak` | 两个目标均退出 0 | [`make-test-log.log`](evidence/make-test-log.log)、[`make-test-leak.log`](evidence/make-test-leak.log)。分别只筛选 `internal/verify` 中的 helper 自测；`internal/verify/cmd/scanner` 显示 `[no tests to run]`。 |
| `go test -race -count=1 -tags mock ./internal/mock/...` | 通过 | [`go-test-mock-tag.log`](evidence/go-test-mock-tag.log)。这组 build-tagged mock 测试不由默认 `make test` 执行。 |
| `go test -race -count=1 -tags e2e ./tests/...` | 主机原始环境失败于同一类 ANSI 断言；清空 `NO_COLOR` 后通过 | [`go-test-e2e-tag.log`](evidence/go-test-e2e-tag.log)、[`go-test-e2e-tag-no-color.log`](evidence/go-test-e2e-tag-no-color.log)。CI 中该 job 使用 Ubuntu 环境；本地结果说明颜色测试没有隔离用户环境变量。 |

原始 `make verify` 的 37 个失败函数集中在 `internal/tui` 与 `tests/e2e_20260802`。[`lipgloss_test_setup_test.go`](../../../internal/tui/lipgloss_test_setup_test.go#L11) 设置 `CLICOLOR_FORCE`、`COLORTERM` 和 Lipgloss profile，但没有清理优先级更高的 `NO_COLOR`。清空该变量后默认 race 套件和 `e2e` 标签套件均通过。应把测试隔离环境作为代码治理缺陷修复，同时保留一个显式测试生产行为尊重 `NO_COLOR` 的用例；不能在 CI 中全局禁用 `NO_COLOR` 来掩盖真实终端支持问题。

## 源码、测试目标与执行覆盖

快照共 810 个 tracked 文件：791 个 Go 文件（321 个非测试源文件、470 个 `_test.go`），Go 源码 71,649 行、Go 测试 123,593 行。`internal/` 有 320 个生产 Go 文件、420 个测试文件；`cmd/cli` 另有 1+1 个；`tests/` 有 49 个测试文件。另有 2 个非 Go 源文件、3 个 GitHub workflow。按模块拆分的文件数和源码/测试行数在 inventory 中逐项可回查；以下为当前审计重点模块摘要：

| 模块 | Go 源文件 / 行 | Go 测试文件 / 行 |
|---|---:|---:|
| `cmd/cli` | 1 / 148 | 1 / 179 |
| `internal/acp` | 8 / 2,471 | 12 / 4,226 |
| `internal/approval` | 11 / 1,683 | 12 / 2,151 |
| `internal/cli` | 37 / 11,791 | 49 / 12,017 |
| `internal/compaction` | 10 / 1,672 | 13 / 2,077 |
| `internal/config` | 13 / 3,273 | 23 / 4,091 |
| `internal/core` | 40 / 7,842 | 74 / 14,947 |
| `internal/extension` | 10 / 1,209 | 15 / 2,514 |
| `internal/llm` | 25 / 6,393 | 38 / 8,782 |
| `internal/mcp` | 6 / 2,003 | 8 / 2,637 |
| `internal/memory` | 3 / 641 | 3 / 948 |
| `internal/mock` | 15 / 2,219 | 17 / 2,545 |
| `internal/production` | 17 / 3,296 | 21 / 5,301 |
| `internal/session` | 16 / 3,220 | 23 / 5,027 |
| `internal/skill` | 5 / 1,004 | 6 / 1,658 |
| `internal/term` | 4 / 97 | 0 / 0 |
| `internal/tools` | 70 / 13,866 | 55 / 15,123 |
| `internal/tracing` | 9 / 1,577 | 11 / 2,292 |
| `internal/tui` | 13 / 4,418 | 31 / 5,942 |
| `internal/verify` | 7 / 2,540 | 8 / 4,328 |
| `internal/web` | 1 / 286 | 1 / 258 |
| `tests` | 0 / 0 | 49 / 26,550 |

源码逐文件审查覆盖了 `internal/verify` 的 7 个生产源文件和 8 个测试文件、`internal/mock` 的 15 个生产源文件和 17 个测试文件；也核对了根层 Makefile、3 个 workflow、项目说明/构建元数据与所有 tracked 路径。`tests/` 的 49 个文件均进入 inventory；本验证 lane 对其执行目标、build tags、skip 点和 `verify` 专项测试做了逐文件覆盖盘点，并重点审阅 `tests/e2e_20260802/09_verify_e2e_test.go` 与 Phase 48 过期用例。其余产品场景测试的断言语义需与其他源码审阅者的 manifest 合并，不能把本报告误读为对 49 个测试文件所有断言逐行语义审查。仓库没有 tracked `scripts/` 目录。

测试入口不一致：[`Makefile`](../../../Makefile#L16) 的 `test` 只跑 `./internal/... ./tests/...`，漏掉 `cmd/cli`；CI 的 PR workflow 用 `go test -race -count=1 ./...`，会包括它（[`pr-check.yml`](../../../.github/workflows/pr-check.yml#L62)）。默认测试还不包含 6 组 `mock` build-tag 测试；CI 的 `-tags e2e` 与默认测试也不是同一用例集。当前 `tests/` 有 21 个 e2e-tagged 文件；10 个文件有合计 18 个静态 `t.Skip`/`t.Skipf` 调用点，其中包含注明 pre-existing、旧 API/输出契约和 flaky timing 的跳过。跳过点在通过率统计中不应算作功能已验证。

## Verification 与 mock 的源码审查发现

### Gate 与扫描器

本地 `make verify` 定义为 `check scan test-log test-leak`；`check` 顺序执行会修改文件的 `fmt`，再跑 vet、lint、build 和默认 race tests（[`Makefile`](../../../Makefile#L77)）。因此 gate 失败时后续阶段不会运行，而 `fmt` 还是写入型命令；审计前确认 gofmt 无差异以保护基线。建议把 `fmt-fix` 与只读 `fmt-check` 分开，`verify` 只运行检查命令，并让 PR、本地和发布前 gate 共用同一个可执行入口。

扫描统计为 20 个 `SCAN-012` ERROR，以及 `SCAN-006` 1086、`SCAN-008` 103、`SCAN-013` 42、`SCAN-007` 27、`SCAN-004` 15 个 WARN。`SCAN-012` 的实现只查找同包接口和 `var _ Interface = ...` 编译断言，缺断言就报 ERROR；它没有检查接口实现是否存在，也不能区分必须默认构造的扩展点与有意要求调用方注入的接口（[`scanner_pkg_rules.go`](../../../internal/verify/scanner_pkg_rules.go#L535)）。所以这 20 条应逐条裁定规则是否适用，不能直接登记为 20 个产品 bug。

WARN 同样不适合不分上下文地当作缺陷：`SCAN-006` 只在当前 package 目录搜索生产引用，不查依赖方包，因而公共扩展 API 会被当作“只被测试使用”；`SCAN-008` 将任何没有 `slog.` 的生产文件都判为日志不足，值对象/纯转换函数也会命中；`SCAN-007` 按文件统计 `//nolint` 数量而不校验每项理由。`SCAN-004` 是对源码文本的宽泛子串扫描，例如把 URL 验证器内真实的私网网段常量 `10.0.0.0/8`、`0.0.0.0/8` 当成测试地址，也把产品中的 localhost 安全策略报成告警。`SCAN-013` 仅按命名模式推测接口位置，常把显式默认实现误报为设计问题。规则目前更适合做人工待办线索，精度尚不足以直接证明产品质量。

本地 `make scan` 对所有 20 ERROR 返回非零。扫描器中 SCAN-003/009 的 `exempt:all` 或规则级豁免可按源文本跳过整文件；各规则检查豁免的方式不一致，也没有中央登记、责任人或到期时间。CI 则用 shell `|| true` 吞掉扫描器退出码，又对 JSON 解析脚本追加 `|| true`，并无期限地忽略所有 `SCAN-012` ERROR；解析失败时也可能以空错误集打印通过（[`ci.yml`](../../../.github/workflows/ci.yml#L142)）。CI 还不处理 WARN，因此本地 gate 与 CI gate 的承诺不同。应将豁免改为可审查的规则/文件/责任人/原因/到期时间台账，扫描器启动失败或输出无效必须失败；在规则重新校准前，错误与告警分级应有明确适用条件。

另一个名为 `RunVerify` 的 API 并未被 Makefile/CI 调用，生产路径外只在 verify 测试中使用。它列有 36 项 V 规则，但仅 6 项 VQ/VG 规则按全目录 AST 标记给出 PASS/FAIL，其余 30 项明确返回 SKIP；测试也断言这些规则继续 SKIP（[`runner.go`](../../../internal/verify/runner.go#L170)、[`runner_test.go`](../../../internal/verify/runner_test.go#L206)）。VQ/VG 的“通过”来自目录内任意位置出现 `Register`、`unknown command`、`version`、`help`、`ctx.Done`、`WaitGroup` 等标记的组合，没有调用真实命令或验证取消传播（[`vqvg_rules.go`](../../../internal/verify/vqvg_rules.go#L47)）。`checkGoFiles` 也只检查 go.mod、lint 配置、Go 文件数以及全仓是否至少有一个测试文件，不是覆盖率验证（[`runner.go`](../../../internal/verify/runner.go#L208)）。应清楚标为静态启发式，不输出容易被误解的业务验收 PASS。

### 日志、goroutine 与 mock 的验证边界

`test-log` 只运行 `TestLogCapturer`，`test-leak` 只运行 `TestAssertNoGoroutineLeak|TestGoLeakChecker`，都在验证工具自己的包里筛选；这两个命令通过只说明 helper 的窄自测通过，不能单独证明生产组件日志字段或所有 goroutine 生命周期正确。全量测试确有多处调用 leak helper，需与这两项窄 gate 区分。泄漏 helper 用总 goroutine 数先行提前返回；当总数回落但泄漏线程替代了短暂线程时会漏报。堆栈只分配 16 KiB，数量很大时 `runtime.Stack` 可截断；排除规则按整段堆栈命中 known pattern（[`goroutine.go`](../../../internal/verify/goroutine.go#L26)）。建议优先用可控 goroutine/通道收敛的测试 fixture，并补并发 churn、截断与 deadline 场景。

日志捕获器通过替换全局 `slog.Default()` 工作，源码明确禁止与 `t.Parallel` 混用；`WithGroup` 将属性压成 dotted key，且在 `WithGroup` 之后回头给先前 `WithAttrs` 属性加 group 前缀，不等价于标准 slog handler 的作用域（[`logcapture.go`](../../../internal/verify/logcapture.go#L36)）。`matchLogEntry` 对 `map[string]any` 的值使用 `!=`，字段值若是 slice/map 等不可比较类型会 panic。建议将捕获器测试限制为测试 utility，并增加与标准 `slog.JSONHandler`/TextHandler 的属性和 group 对照用例。

`ConversationRunner` 直接调用 `Model.Generate` 并自行执行 MockToolServer，不经过 `core.Harness`/AgentLoop/工具中间件/审批/会话/压缩主链（[`conversation_runner.go`](../../../internal/mock/conversation_runner.go#L45)）。它创建的 `cli.invocation` 与 `llm.request` span 也是 runner 自己包裹出来；`AssertTraceComplete` 只检查这两个名称是否出现，span settle 到 2 秒超时会静默返回，断言没有核对父子关系与完整性。长对话生成器测试只检查模板轮数、tool call 数和确定性，不会把 100/200/500 轮真正跑过 Runtime、压缩和 trace 链（[`long_conversation_generator_test.go`](../../../internal/mock/long_conversation_generator_test.go#L13)）。

`MockToolServer.RegisterMockTool` 注册的 handler 只在自定义 `Execute` 被调用时执行；它返回的 `simpleToolDef.Execute` 始终返回固定 `"mock"`，而 `Execute` 不属于 `ToolRegistry` 接口（[`mock_tool_server.go`](../../../internal/mock/mock_tool_server.go#L68)）。因此现有 contract tool 测试直接调用 `MockToolServer.Execute`，不会测 registry 返回定义后的真实工具执行路径。LLM contract provider 表目前只有 mock 一行（[`contract_llm_test.go`](../../../internal/mock/contract_llm_test.go#L28)），只验证一条非空输出、已取消 context 和 stream 关闭；不能证明不同 provider 实现契约一致。MCP mock 的 Start/Stop 只切换未参与请求判定的状态，Connect/Disconnect 是 no-op、协议版本不做握手且 CallTool 忽略 context（[`mock_mcp_server.go`](../../../internal/mock/mock_mcp_server.go#L82)）。`mcp_contract_test.go` 的 in-memory JSON-RPC bridge 是更强的适配器测试，但上游仍是合成 peer，不是真 MCP server。

## Phase 48/49 计划落地对账

[`infra/tasks.json`](../../../../go-cli-infra/infra/tasks.json) 中 Phase 48/49 都是 `done`，分别列 25 与 27 项，子任务文件里每项也标 `done`。但 [`milestone.md`](../../../../go-cli-infra/infra/milestone.md#L666) 的 M51/M52 仍为 `PENDING`。Phase 48 的 48-10、48-18、48-25 与 Phase 49 的 49-27 验收条件都要求 `make verify` 成功且 `0 ERROR`（例如 [48-10](../../../../go-cli-infra/infra/tasks-phase-48.json#L1018)、[48-18](../../../../go-cli-infra/infra/tasks-phase-48.json#L1970)、[48-25](../../../../go-cli-infra/infra/tasks-phase-48.json#L2813)、[49-27](../../../../go-cli-infra/infra/tasks-phase-49.json#L3023)）。当前 commit 上原始环境 gate 先被 ANSI 测试挡住；隔离颜色环境后仍在 scanner 的 20 ERROR 处失败。因此静态 `done` 状态与可重放的验收证据不一致。状态本身不能反推相关产品改动没有实现；应先修正/校准 gate，按当前 commit 重跑并将命令、环境、日志和 exit code 附到验收项，再同步 milestone 状态。

## 建议的验证迭代

1. **I0：恢复确定性。** 让颜色断言显式控制 `NO_COLOR`，单独保留一个验证无颜色模式的产品测试；`make test` 改为覆盖 `./...`，格式检查只读，Pin goimports 版本，记录固定 Go 工具链。明确默认、`mock`、`e2e` 标签分别何时执行，并把 skip 点拆成已修复/有期限接受/删除的待办。
2. **I1：统一质量门。** CI 和本地调用同一入口与同一规则配置；CI scanner 检查其自身 exit code 与 JSON 完整性，不用全局规则豁免和 `|| true`。对 scanner 每条规则维护可重放正例/反例，并做误报/漏报审查；豁免必须逐项有 owner、理由、文件范围与失效日。先把 SCAN-012 定义改成明确的可验证要求，或降为需人工审查的建议。
3. **I2：将 test helpers 变成 runtime 证据。** 通过真实 Harness/AgentLoop 走 LLM→ToolRegistry→middleware/approval→session/compaction→trace 的测试场景；对长轮次真正执行并断言取消、失败、压缩、父子 span、上下文收敛和无泄漏。给 provider/tool/MCP 合约同时接真实适配器和可控协议 peer，避免只让 mock 通过自身契约。
4. **I3：任务状态闭环。** Phase/Milestone 完成依赖当前代码与固定环境中同一验证入口全绿；报告逐项区分已执行、未执行、因条件跳过与尚无实现。回写 M51/M52 状态，且每次规则/gate 变化更新验证版本与例外台账。

本轮无源码差异；测试原始失败和颜色隔离后的复跑日志均保留，不能用后一次通过覆盖前一次失败。

## 稳定发现 ID 索引

以下 ID 供迭代计划和跨审计台账引用；严重度表示验证体系对交付可信度的影响，不直接表示产品漏洞等级。

| ID | 优先级 | 稳定结论 | 证据 / 本文位置 | 计划映射方向 |
|---|---|---|---|---|
| VFY-01 | P1 | ANSI 断言受外部 `NO_COLOR` 污染；默认 `make verify` 在当前主机环境失败，隔离环境后测试通过。 | `evidence/make-verify.log`、`evidence/make-test-no-color.log`；“执行结果” | I0：隔离颜色测试环境，并单独保留 `NO_COLOR` 产品行为用例。 |
| VFY-02 | P2 | 本地 `make test` 漏掉 `cmd/cli`；CI PR 目标是 `./...`，本地与 CI 覆盖不一致。 | [Makefile](../../../Makefile#L17)、[pr-check.yml](../../../.github/workflows/pr-check.yml#L62)；“源码、测试目标与执行覆盖” | I0：让默认测试入口覆盖 `./...`。 |
| VFY-03 | P2 | `mock`、`e2e` build tags 不在默认测试中；`tests/` 有 21 个 e2e-tagged 文件及 18 个静态 skip 调用点，部分行为因此未验证或被跳过。 | `evidence/go-test-mock-tag.log`、`evidence/go-test-e2e-tag-no-color.log`；“源码、测试目标与执行覆盖” | I0：标清每类 tag 的 CI 入口并清理/登记跳过项。 |
| VFY-04 | P1 | Scanner 的 20 个 SCAN-012 ERROR 和大量 WARN 是启发式命中，存在适用性误报；不能按 findings 数直接当产品 bug。 | `evidence/make-scan.log`；[scanner_pkg_rules.go](../../../internal/verify/scanner_pkg_rules.go#L535)；“Gate 与扫描器” | I1：逐规则建立适用条件、正反例和可复核豁免台账，再决定阻断级别。 |
| VFY-05 | P1 | CI scanner 吞掉进程和 JSON 解析错误，且永久忽略整个 SCAN-012 规则；本地 `make scan` 与 CI 的判定不一致，可能假绿。 | [ci.yml](../../../.github/workflows/ci.yml#L142)、`evidence/make-scan.log`；“Gate 与扫描器” | I1：移除 `|| true` 与宽泛豁免，扫描器异常必须失败，并让 CI/本地共用 gate。 |
| VFY-06 | P2 | `RunVerify` 未接入 Makefile/CI；36 项中 6 项由全目录字符串/AST 标记推 PASS，30 项返回 SKIP，不构成 Runtime 验收。 | [runner.go](../../../internal/verify/runner.go#L129)、[vqvg_rules.go](../../../internal/verify/vqvg_rules.go#L47)；“Gate 与扫描器” | I1：把启发式显式标成静态提示，业务 PASS 改由可执行行为测试支持。 |
| VFY-07 | P2 | `test-log`/`test-leak` 仅筛选 verify helper 自测；全量测试虽调用 leak helper，专用目标自身不覆盖生产调用链，helper 也有计数/堆栈及 slog 分组语义边界。 | `evidence/make-test-log.log`、`evidence/make-test-leak.log`；[goroutine.go](../../../internal/verify/goroutine.go#L26)、[logcapture.go](../../../internal/verify/logcapture.go#L36)；“日志、goroutine 与 mock 的验证边界” | I2：在真实 Runtime 测试中断言日志、取消和 goroutine 收敛，并加强 helper 的边界用例。 |
| VFY-08 | P1 | `ConversationRunner` 和 mock contract 测试绕开 Harness/AgentLoop 等主链；ToolDefinition.Execute 与 MockToolServer.Execute 行为不同，mock 单独通过不足以证明真实集成。 | `evidence/go-test-mock-tag.log`；[conversation_runner.go](../../../internal/mock/conversation_runner.go#L45)、[mock_tool_server.go](../../../internal/mock/mock_tool_server.go#L68)、[contract_llm_test.go](../../../internal/mock/contract_llm_test.go#L28)；“日志、goroutine 与 mock 的验证边界” | I2：对接真实 Harness、registry、middleware、session/compaction 与 adapter，跑长轮次并断言跨层不变量。 |
| VFY-09 | P1 | Phase 48/49 JSON 与全部子任务标 done，但要求 `make verify 0 ERROR` 的验收在当前 HEAD 无法复现；Milestone M51/M52 仍 pending。 | `evidence/make-verify.log`、`evidence/make-verify-no-color.log`；“Phase 48/49 计划落地对账” | I3：修复 gate 后以当前 commit 的日志/exit code 重验，随后同步 task 与 milestone 状态。 |
| VFY-10 | P2 | `make verify` 经 `fmt` 写入源码；本机缺少 goimports 因配方跳过，而 CI 安装 `@latest`，格式门存在副作用和工具版本漂移。 | [Makefile](../../../Makefile#L55)、[ci.yml](../../../.github/workflows/ci.yml#L42)；“Gate 与扫描器” | I0：拆分只读格式检查与显式修复命令，固定 goimports 版本。 |
| VFY-11 | P2 | Docker 构建固定产出 `GOARCH=amd64` 二进制，但运行阶段 Alpine 没有显式平台约束。面向 ARM64 的构建目标可能得到 ARM64 runtime image 搭配 amd64 binary；默认 amd64 路径不受此发现影响。 | 静态检查 [Dockerfile](../../../Dockerfile#L13)、[Dockerfile](../../../Dockerfile#L19)；未执行 `docker build`/Buildx，也未验证 ARM64 运行行为。 | R8：声明当前支持并验证的平台；若仅支持 amd64，文档化并拒绝/约束不匹配目标；若承诺 ARM64，再让二进制架构与目标平台一致并对每个支持目标做 smoke。`TARGETARCH` 可作为实现选项，不代表必须扩展平台承诺。 |
