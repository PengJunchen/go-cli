# Runtime 恢复计划

日期：2026-09-29
基线：恢复集成分支 `codex/runtime-repair-20260929`，从 `develop` 的 `17ab7b7` 建立；产品源码基线 clean。本文件只新增恢复规划，不修改产品源码或原进度台账。
范围：恢复原 R0–R8 未闭环项的实现与验证；不推送，不改 `go-cli-infra` 受保护规则，不在此文档里替代代码实现。

本计划以 [`2026-09-25-runtime/execution-tasks.json`](../2026-09-25-runtime/execution-tasks.json) 的原 task ID、依赖和可执行验收为准，并结合当前源码和既有 handoff/review。`execution-status.md` 是历史进度快照，不能证明当前分支已完成旧记录中的验证。隔离 worktree 的提交只有整合到恢复分支且通过独立复审后，才允许由台账唯一维护者更新为 `done`；新计划和单测通过本身都不构成闭环。

原任务标题与旧 handoff 是整阶段验收义务，不等于每个旧列出的缺陷现在仍存在。下面把已接受并整合的 source slices 当作基线复用；只有当前核对确认的断点才派新 builder。未有当前证据的原 task 继续 open 供后续验收，不据旧状态重复实现或断言源码仍坏。

## 当前并行所有权

恢复分支上已安排的四个 builder 切片继续按原边界执行，不重复分派：

| 已启动切片 | 原 task / 范围 | 合并前边界 |
|---|---|---|
| directory records | `R4-BLD-02`，目录记录捕获 | commit `c3e5848` 首个 P2 修复在独立完整 tools race 中被拒绝：旧测试尚未适配 NUL framing 和 fd argv。builder 正修正断言并补 glob/case/hidden/ignore/root/exit contract；baseline 已有 pattern `--` 分隔，只保留保护回归，不宣称新增 injection 修复。 |
| renderer fix | `R8-BLD-01`，AppendChunk 渲染器修复 | 只接受 renderer source/tests/benchmarks；app/viewport 接线等此切片被接受。viewport primitive 已在基线集成，不重写。 |
| Claude tools | `R3-BLD-01`，Claude 工具协议 | 只处理 native request/history/response tool-use 往返与约定测试；不把一个 provider 切片当作 R3 完成。 |
| stream join / BUILD-1 | `R3-BLD-02` 的关闭链先决切片 | 先证明所有受影响的 stream reader 等待底层 source close；后续 breaker/detector 依赖此语义。 |
| durable CLI | `R2-BLD-01` / `R2-BLD-02`，pending-lease conformance 后做真实 prompt/REPL 接线 | `9258330` pre-owner lease implementation 已集成；不重做 core lease，测试通过后只接真实 durable session。 |
| invocation-owner | `R7-BLD-02` 相邻的 CLI invocation 生命周期切片 | design 已经 root 两轮 review 接受，Builder 正在独立 worktree 实现；仅该 owner 修改 `cmd/cli/main.go`。含 sealed extractor/private pending type、防 `errors.Join` 假 Done、多 pending 等待、真实 main pending exit 和 exporter owner-finished 生命周期。 |

恢复基线已包含 viewport primitive（`7c13811` / `b0ca00e`，final review ACCEPT）以及 R5 canonical DTO/projection 基础。当前 primitive 的 `SetRows`/`sameRows` 会扫描完整 loaded projection；`BubbleteaApp.Send` 仍无返回值，消息队列满时会静默丢弃。baseline-seams review 未发现 R7/R4/R5 的 P1，确认上述是 UI 后续实际源缺口；另确认 MCP HotReloader 在 Close 开始到 async owner 设置 `closing` 之间可 late Reload 一次（P2），纳入 adapter lifecycle 修复。

`c3e5848` 的 directory slice 独立审查为 REJECT，后续修正需重新独立接受；Grep handoff 及其 managed-command writer 前提接受后才启 builder。`baseline-seams-review` 已接受项留作基线，不因 phase-level R7/R4/R5 仍 open 而重开。

## 未闭环原 task、类型、依赖和验收

任务状态沿用原台账的 `pending` / `in_progress`；“局部已接受”只表示其 handoff/review 记录的窄切片，不代表整个 task 通过。

### R0：默认门禁与独立放行审查

| 原 task | 缺口类型与依赖 | 闭环验收 |
|---|---|---|
| `R0-PROTECTED-GATE-01` | 门禁接线尚未完成；依赖 `R0-BLD-04`。受保护 `Makefile`、CI、rules 文件在该任务的授权门槛通过前不得修改。 | 展示并取得针对确切 diff 的显式批准；整合后从 clean commit 运行默认 Makefile/CI 等价 gate，留存命令、环境、SHA、完整日志和退出码；逐项处理 scanner source finding，不允许靠忽略或降级变绿。 |
| `R0-PROTECTED-REV-01` | 尚无独立放行审查；依赖 `R0-SOURCE-REV-01`、`R0-PROTECTED-GATE-01`。 | 确认 exact patch 有显式批准；在 clean integrated commit 复跑默认门禁并检查完整日志、测试注册与 scanner finding；拒绝 allowlist、吞错或降级 severity。未满足审批时保持待办，不阻塞不相关源代码修复。 |

### R2：耐久会话、恢复和真实 CLI 接线（主路径）

| 原 task | 缺口类型与依赖 | 闭环验收 |
|---|---|---|
| `R2-BLD-01` | 原 task 验收包含物理 session identity、tree/branch、snapshot、compaction provenance、lock-before-read、恢复权限和 clone/sequence。依赖 `R2-DES-01`、`R1-REV-01`。基线已有多项独立接受的 identity/snapshot/resume-sync/core-binding slices；`9258330` 已实现 pre-owner lease capture/release/transfer。不要重写这些实现。 | 保留原 task 的恢复/身份用例并核验它们覆盖当前集成点；当前 durable builder 先为 Harness 补 accepted-run pending-lease regression conformance（baseline race pass），再做真实 CLI 接线。端到端验收仍需实际 CLI 子进程精确 resume 和同一 session/branch 的恢复；只报告新增缺口的 RED/GREEN。 |
| `R2-BLD-02` | 原 task 验收含 intent/result semantic commit、append/flush/sync error、unknown outcome 和 torn-tail 恢复；依赖 `R2-DES-01`、`R2-BLD-01`、`R1-REV-01`、`R4-EFFECT-01`。workspace index、managed helper、GitDiff/Write/Edit completion 等已有 accepted slices 继续复用；R4 effect seam 不重开 builder。当前明确未接通的是 durable helper/真实 RunCommit sink 到默认 prompt/REPL 与重启旅程。 | 真实 prompt/REPL：配置持久目录时使用 durable manager/真实 sink，prepare/recover 在首次 provider dispatch 前，exact-ID resume，不做 legacy 第二写；每次 run lease 由 core owner 持有；从同一 `PreparedSession` canonical snapshot hydrate/refresh；输出写失败仍 drain/join；unknown/cleanup-pending 不启下一 turn；完整 Assembly Close 错误可见。用 loopback provider、隔离 session/config roots 做创建→工具 side effect→受控中断→exact resume，证明副作用不重放、terminal/receipt 只一次。原 writer-failure/child-crash/safe-read tests 是必须对照的 acceptance；已接受的 matching slices 不重做。 |
| `R2-REV-01` | 持久恢复最终复审；依赖 `R2-BLD-01`、`R2-BLD-02`、`R1-REV-01`、`R4-EFFECT-01`。 | 核对 task 中全部 crash matrix 用例，并运行 session/CLI 完整 race suite；同一 session 两次恢复无重复 side effect、sequence 稳定且每 run 只有一个 terminal projection；监督真实 child：parent 退出后冲突写被阻断、安全读按 trusted effect 放行，只在确认 child exit 或显式人工 reconciliation 后解除 quarantine。 |

R2 生产接线的手工验收矩阵见 [`r2-cli-production-wiring-handoff.md`](../2026-09-25-runtime/r2-cli-production-wiring-handoff.md)：prompt/REPL 真正创建或 exact-resume durable session、canonical history/tree 刷新、无 legacy transcript 双写、输出 writer 出错时继续 drain/join、unknown/cleanup-pending 不启动下一 turn、Assembly Close 错误可见。此项是现有 helper 到默认命令路径的功能接线，不得用新增设计文档、helper 单测或“session API 已有”代替。

### R3：provider 协议、operation retry、stream 终结、compaction 与 loop

| 原 task | 缺口类型与依赖 | 闭环验收 |
|---|---|---|
| `R3-DES-01` | provider 能力、协议版本、retry 边界、成本语义和 tool-group invariant 仍需定稿；依赖 `R1-REV-01`、`R2-REV-01`。 | 在实现前声明支持的 native provider/version；unsupported protocol 必须在发送凭证和 prompt 前拒绝；明确 stream usage undercount 为缺陷，并明确 PRD-04 hard budget 是否属于本次承诺。 |
| `R3-BLD-01` | resolver/catalog 已有 accepted source slices；当前 active Claude tools slice 属于原 task 的余项。依赖 `R1-REV-01`。 | 只完成当前声明支持的协议：Claude request tool schema、assistant tool history/results、Generate/Stream `tool_use` parity 与 malformed terminal 不 dispatch；httptest 检查真实 body/endpoint/result/error。原 task 仍需核对跨入口 factory、unsupported protocol、system/image 输入和 composer；已接受 resolver 不重做。 |
| `R3-BLD-02` | 原 task 汇集 retry、breaker、active loop、attempt/cost accounting、body bounds、sanitizer 与 cache privacy。local retry 次数/typed classification、body bounds 已是窄切片；当前 lane 是 stream join，后续未闭环为 logical-call breaker、真实 prepared-attempt loop termination、operation-level usage/cost/accounting。依赖 `R3-DES-01`、`R1-REV-01`。 | BUILD-1：受影响 reader 等底层 close 后才关闭输出/进入 retry-failover；cancel/error/cap/post-final/setup-error 下不丢 close/join。BUILD-2：logical-call ticket 等 terminal+join 结算；mid-stream error 不 replay，open 零 dispatch，caller cancel neutral，timeout/local abort failure，guard denial 不反转 provider success。BUILD-3：trusted frozen attempt（identity+digest）在 approval/executor 前由真实 proxy 和 durable consumer 同一路径观测；threshold attempt 不执行、停止后续 provider 请求，R1 live/durable terminal cause 一致。独立 cache-key mini-slice：当前 `toolCacheKey` 生成 `name+raw JSON`，marshal failure 回落为裸 `name`；非 nil injected cache 使用不含 raw args 的稳定 key，marshal failure bypass Get/Set 并执行工具，不能不同 args alias；默认 cache 保持 disabled，不增加产品决定。核对 `TestToolCacheDoesNotLogRawArgs` 覆盖 key secret 不泄露和 failure bypass。最终命名 retry/network/cancel、breaker、loop、stream usage/cost、sanitizer tests 走实际 assembled middleware chain。 |
| `R3-BLD-03` | tool-call-group compaction 已有独立接受的内存层 slice；当前剩余聚焦 provider-facing configured summarizer、budget-before-first-call 与 durable `CommitCompaction`/snapshot projection 接线。依赖 `R3-DES-01`、`R2-REV-01`。 | `TestContextBudgetCheckedBeforeFirstProviderCall`、configured summary model、non-file tool output、memory search 单/双文件和 CJK cases；compact/restore 后真实 request 合法，tool call/result 成组，durable snapshot 的 compaction provenance 一致。复用已接受 group validation，不重写。 |
| `R3-REV-01` | CLI edge 最终复审；依赖 `R3-BLD-01`、`R3-BLD-02`、`R3-BLD-03`。 | 先确认命名测试实际注册，再运行 llm/core/compaction 完整 race suites；以本地 fake provider trace 默认 CLI 和所有启用 native path，证明无不完整 tool call、stream 错误不呈现成功且成本/attempt 语义正确。 |

BUILD-1、2、3 的边界和具体文件见 [`r3-breaker-loop-handoff.md`](../2026-09-25-runtime/r3-breaker-loop-handoff.md)。BUILD-2 依赖 BUILD-1；BUILD-3 改 `prepared_registry` / `run_commit_consumer` / `loop` 的重叠区时与 R2 及 core close 切片错峰。该 handoff 有独立设计 review，后续实现按其冻结的 BUILD-2/3 合约继续，不等待与这些切片无关的整个 `R3-DES-01` task；R3-BLD-03 的 durable compaction 仍依赖 R2-REV 和具体 compaction design。R3-BLD-03 可与不重叠的 provider middleware slice 并行，但不可与同一 `internal/core/loop.go` 的 detector 改动并行。

### R4：effect 身份、permission 与工具边界

| 原 task | 缺口类型与依赖 | 闭环验收 |
|---|---|---|
| `R4-EFFECT-01` | 原 task 是 R2 恢复所需的 trusted registration identity/effect 先决条件，无前置依赖。prepared-dispatch、registry/proxy 与 policy source slices 已集成且各有独立接受记录；复用，不从头派 builder。 | R2 复用已接受的 Assembly/prepared registry identity/effect evidence，不新开 builder。全 R4 phase 的 bundle/default gate 或 `R4-REV-01` pending 不得阻塞独立接受的 slice；剩余 phase acceptance 留给 R4 closure。 |
| `R4-BLD-01` | 原 task 的 permission/effect phase acceptance；依赖 `R4-DES-01`、`R1-REV-01`、`R0-BLD-05`、`R4-EFFECT-01`。prepared approval-policy 独立 slice 已集成接受；本计划不把它重做。 | 对剩余 task tests 核验 unknown fail-closed、cache 绑定 executor version、Plan Mode side-effect 遍历、read-only network policy、多入口 approval 一致性、model Plan exit；枚举 registry effect 并覆盖 subagent。先按当前集成代码找到未过用例再开 builder。 |
| `R4-BLD-02` | 原 task 覆盖 schema、path scope、capture、取消与工具接线；当前 directory-records builder 已运行，另有 Grep bounded-capture 尚未实现。accepted path-whitelist/read/schema slices 复用。原依赖 `R4-DES-01`、`R1-REV-01`、`R3-REV-01` 用于完整 task/review；不阻止相互独立的窄实现先行。 | 完成目录 lane 的 LF/NUL/display/EOF cancellation 复审；单独完成 `internal/tools/grep.go` + tests 的 JSON framed incremental capture：真实 rg 单文件 `line:content`、colon/LF path、CRLF/final-CR、Go 与 rg 共用 top-K/bytes cap、超限后 drain/join、malformed frame typed error、cancellation/pending no fallback、argv/env/path validation；运行 `internal/tools` race 与 handoff 列出的 approval/core/CLI 集成 gate。schema/path/remote Bash 等剩余原 task 验收仍留在 phase checklist。 |
| `R4-REV-01` | 从 model schema 到 executor 的权限独立审查；依赖 `R4-BLD-01`、`R4-BLD-02`。 | exact-name tests + tools/approval/core race；每种 effect 经真实 registry、approval middleware 和 execution seam，unknown 被阻断。 |

### R5：TUI transcript、HITL 与输入控制

| 原 task | 缺口类型与依赖 | 闭环验收 |
|---|---|---|
| `R5-DES-01` | 原 task 仍是共享 transcript/control journey 的 design prerequisite；依赖 `R1-REV-01`、`R2-REV-01`。accepted DTO/adapter 与部分 keyed projection work 复用，不因 whole phase review 未结束回滚独立 slice。 | baseline DTO/adapter review 对照 `CanonicalTranscriptSnapshot` 的 session/branch/source EntryID/action metadata 和 typed compaction；锁定 live-only overlay、event gap、`done` status、backpressure 和 durable terminal 的边界。每个 durable row 可追到 selected `PublishedView`；active worker/uncommitted terminal 不可显示 durable done。 |
| `R5-BLD-01` | 原 task 含 approval/ask_user/event projection/transcript 与 default path；当前明确的 app gaps 是 durable snapshot/overlay 接线及 void `Send` queue overflow 静默丢事件。依赖 `R5-DES-01`。 | 通过真实 Bubble Tea model 和 PTY：approval requests 按 ID 不丢、done 不重放文本、tool args/pre-tool metadata 保留、selected branch 恢复；Send overflow 可观察并把 provisional view 标为 incomplete，关键 snapshot/status 可可靠恢复，不得因事件丢失报成功。 |
| `R5-BLD-02` | 原 task 的 editor/caret/key/cancel acceptance；已有 approval/editor interaction slice 已独立接受，不能假定原描述的每个问题仍在。依赖 `R5-DES-01`、`R1-REV-01`。 | 在当前代码确认未覆盖项后跑 caret/help binding/approval cancel/stop-wait tests；PTY 输入序列与共享 runtime contract 状态一致。 |
| `R5-REV-01` | TUI 端到端审查；依赖 `R5-BLD-01`、`R5-BLD-02`。 | 确认测试注册；完整 TUI/core/session race 与 PTY；小、大、并发 approval 均不丢 request、不重复答案，取消后只有 durable terminal 才关闭 run。 |

默认 durable hydration、restart/resume、same-handle snapshot refresh 必须等 R2 接通；当前已有 R5 projection handoff 明确要求 `PublishedView` source IDs、typed compaction、provisional/durable overlay、event gap 与 status 独立。R2 负责 `assemble.go`/durable session ownership 与 `repl_session.go` 的 durable binding；R5 随后负责 transcript projection 与 TUI。两方不得同时编辑这些共享 CLI 文件。可先做纯 TUI 不 mint identity 的展示规则；不允许用伪造 DTO 测试声称完成生产接线。

R5 后续两片按以下 prerequisite 和 owner 落地：

1. **Snapshot / provisional overlay（`R5-BLD-01`）。** 依赖真实 durable CLI prepare/bind/terminal-refresh seam 可用，以及 baseline canonical DTO/adapter 独立 review 通过。R2 完成 `repl_session.go` durable ownership 后交接该文件；R5 使用 `CanonicalTranscriptSnapshot` 初始化和刷新 TUI keyed durable rows。Live bridge event 仅为 provisional overlay；snapshot reconcile 按 session/branch/EntryID/occurrence 替换同一 provisional row，不追加第二份答案。`done` 只更新 status；event gap、snapshot error、terminal sync failure 不显示 complete/saved。覆盖 selected-branch source IDs、typed compaction、open journal 不冒充 PublishedView、重复 ToolCallID、nested args、same-handle refresh、unknown/cancel 和真实 restart PTY。
2. **Send/backpressure（`R5-BLD-01` 独立小切片）。** `BubbleteaApp.Send` / model queue 当前无返回值且队列满会丢消息。把 overflow 变为调用方可观察的 error/result，并使 provisional projection 标 incomplete；durable snapshot、terminal/status、approval 决策等关键状态不得依赖 best-effort queue 静默到达，需从可靠状态/snapshot 源可恢复。对应 `TestTUISendReportsBackpressure` 与 `TestTUIEventGapMarksProvisionalViewIncomplete`；队列须保持有界。

### R6：CLI trust、ACP/MCP/Web adapter

| 原 task | 缺口类型与依赖 | 闭环验收 |
|---|---|---|
| `R6-DES-01` | protocol version、entrypoint identity/auth/session/stdio/lifecycle 支持面未定；依赖 `R1-REV-01`、`R2-REV-01`、`R4-REV-01`。 | 对每个真实入口冻结 wire version 与 trust/revoke/approval 行为；prototype Web 或 remote approval 未完成者标 unsupported。 |
| `R6-BLD-01` | 原 task 的 config/trust/auth/CLI 输出 acceptance；scoped auth persistence/private-file slices 已有独立结论。依赖 `R6-DES-01`、`R3-REV-01`。 | 先以当前代码审计未覆盖项，再核验 provider credentials factory、config revoke、restart trust、invalid-config help、prompt args/failure span/token permissions；隔离 home 的真实 CLI subprocess stdout 可解析。 |
| `R6-BLD-02` | 原 task 的 ACP/MCP/Web lifecycle 与 wire acceptance；若单个 adapter slice 已接受则复用，不能仅因完整 R6 review pending 重做。依赖 `R6-DES-01`、`R4-REV-01`、`R2-REV-01`。当前 baseline-seams review 明确了 MCP HotReloader close-admission P2：`closing` 由 async owner 后设，Close 已开始后仍可能 late Reload 一次。 | 对仍声明 shipped 的路径做 task 指定 ACP event isolation/dispatch bound/remote approval、stdio cancellation/write serialize、MCP atomic reload/error/name/joins、Web authenticated journey；以声明版本本地 peer 验证。此 P2 要求 Close invocation 先线性化关闭 admission，close owner 启动与 join 仍 only-once；用 barrier-controlled race 证明 Close 开始后 Reload 不会发布新 generation/tool definition，也不吞原 owner 完成错误。 |
| `R6-REV-01` | 每条真实 CLI/service 入口统一契约终审；依赖 `R6-BLD-01`、`R6-BLD-02`。 | CLI race 与 prompt/serve/ACP/stdio/Web/MCP fixture journey；验证 wildcard auth 与 credential scope 负例。 |

### R7：扩展、skill、trace/audit 生命周期

| 原 task | 缺口类型与依赖 | 闭环验收 |
|---|---|---|
| `R7-DES-01` | 扩展/skill API、hook action、shutdown 与 exporter/privacy 承诺未定；依赖 `R1-REV-01`、`R4-REV-01`、`R6-REV-01`。 | 只声明有匹配 collector/broker 测试的 exporter 协议；默认 trace/audit 不保存原始 prompt、secret 或无界 tool args。 |
| `R7-BLD-01` | 原 task 的 extension/skill lifecycle acceptance；现有 extension-core/MCP HotReloader/core-wrapper 有接受的窄切片；依赖 `R7-DES-01`。不将原 task 长列表都宣判为当前缺陷。 | 对照当前 integration 核验 hook 顺序、init/stop 并发、waiter/owner cancellation、immutable observer、skill limits/XML/args 与 remote unsupported；真实 extension load→init→hook/tool→shutdown。 |
| `R7-BLD-02` | 原 task 的 tracer/redaction/span/exporter/metrics/audit acceptance；已有 telemetry/async exporter 部分接受。依赖 `R7-DES-01`、`R1-REV-01`。当前 invocation-owner design 单列，root review 后实现，不混入其他 builder。 | 对照剩余 tests：single parent/redaction/error status/shutdown/batch failure/injective labels/audit append/unavailable stats；内存/文件 exporter + failed collector，不能留高 cardinality/secret。 |
| `R7-REV-01` | plugin lifecycle 和 telemetry 独立复核；依赖 `R7-BLD-01`、`R7-BLD-02`。 | exact tests + extension/skill/tracing/approval race；production assembly 注入 init/reconnect/exporter/audit failure 并核对可观察结果。 |

### R8：viewport、render、capability 和平台声明

| 原 task | 缺口类型与依赖 | 闭环验收 |
|---|---|---|
| `R8-DES-01` | terminal/accessibility/render matrix 与 image/task/memory/web/branch 产品承诺未定；依赖 `R5-REV-01`、`R6-REV-01`、`R7-REV-01`。 | 每个承诺 capability 对应默认入口 smoke test 和 durable 行为；未承诺项明确 unsupported/deferred。 |
| `R8-BLD-01` | viewport primitive 已集成且独立接受；剩余是 loaded-projection app/accordion 接线、loaded-scope search/keybindings、实际 body height、stream renderer append 接口和平台验收。原 phase 依赖 `R8-DES-01`、`R5-REV-01`；以下 bounded app slice 可按已批准 handoff 推进，但 phase task 仍保持 partial。 | 保持当前 split layout/per-pane anchors；search 仅搜完整 loaded projection 并显式标出 scope，不等待 R2 paging、不声称 full-history search。Standalone 未关联 event 可用 private process-local display key 只作 viewport anchor/selection，不参与 association/reconciliation/persistence 或 durable ID 显示。`SetRows` 仅用于 snapshot 全量替换；live tail 用 stable-key 窄 row update/revision seam 更新一行并只失效 bounded cache；若 primitive 缺此 seam 只加最小接口，不可每 chunk 全量 `SetRows`/`sameRows`。stream renderer 使用独立接受的 `AppendChunk`，不能反复 render accumulated prefix。以实际 app `Update` + `View` 验证 10,000 rows、1 MiB/1,000 chunks、动态 body height、search/keybindings、resize 与两 pane anchor/focus/input/usage footer；另做测量与隔离配置 PTY。 |
| `R8-BLD-02` | 被选 capability 尚未实现或明确关闭；依赖 `R8-DES-01`。 | 对被选 image/task/memory 功能完成默认 CLI/restart 流程；对未选项在真实入口稳定报告 unsupported；memory write failure/delete reopen 一致性。Docker target 只按声明验证，不能推定 ARM64。 |
| `R8-REV-01` | TUI、capability、证据包最终审查；依赖 `R8-BLD-01`、`R8-BLD-02`。 | exact tests 注册后执行包 race、PTY、能力重启流程及 local/CI-equivalent gates；记录平台/emulator/env；renderer benchmark、R5 input PTY 或旧 viewport unit 各自不能替代完整验收。 |

R1 在原任务集没有未闭环 task，是上述多个阶段的已完成前置；R9 仍 deferred，不纳入本次恢复范围。

**R8 bounded app builder 的文件和顺序：** renderer fix builder 独占 `internal/tui/streaming_markdown.go` 与其 tests/benchmarks，接受后释放文件；viewport app builder 再改 `internal/tui/app.go`、`accordion.go`，仅在需要时给已接受的 `transcript_viewport.go` 加窄 row revision 更新 seam。snapshot `SetRows` 可 O(rows)，但每个 live chunk 的 Update/View 不得重扫全表或重绘历史前缀。内容高度按当前真实 footer/status/input/todo/modal 行计算。viewport 两 pane 各自保持 anchor/follow-tail；跨 split threshold resize 保持所选 row 与其 pane anchor。benchmark 必须经实际 app Update/View 路径；primitive-only benchmark 不代表 app 成本。具体 contract 以 [`r8-viewport-handoff.md`](../2026-09-25-runtime/r8-viewport-handoff.md) 为准。

## 执行序列与并行边界

### 近期窄切片调度表

| Builder slice | 原 task | 可开工条件与文件所有权 | 验收交付 |
|---|---|---|---|
| durable CLI | `R2-BLD-01` / `R2-BLD-02` | 当前已开工；先加 `Harness` pending-lease conformance，复用 `9258330`，然后独占 `internal/cli/assemble.go`、`prompt.go`、`repl_session.go` durable seam。 | configured prompt/REPL 真实 durable session + exact resume + same-handle refresh + owner close + child-process restart/no replay。 |
| directory records | `R4-BLD-02` | 当前 worktree 已开工；不与另一个 tools lane 改同文件。 | captured path/display/NUL/EOF cancellation tests + affected race; 仅该 slice 可接受。 |
| renderer fix | `R8-BLD-01` | 当前 worktree 已开工；独占 `internal/tui/streaming_markdown.go` 与 renderer tests/benchmarks。 | AppendChunk 边界正确且输入/保留内存有界；独立接受后交接 app 文件。 |
| Claude tools | `R3-BLD-01` | 当前 worktree 已开工；独占 `internal/llm/native.go` 及对应 tests。 | local HTTP request/history/tool-use Generate + Stream parity 和 malformed tool no-dispatch。 |
| stream join BUILD-1 | `R3-BLD-02` | 当前 worktree 已开工；只改 stream wrappers/readers 和其测试，避免后续 BUILD-2 共改。 | cancel/error/cap/setup-error 等真实 controlled source 关闭前不提前结束或 retry；llm/core/cli race。 |
| invocation owner | `R7-BLD-02` 相邻子片 | design 已两轮 root review 接受；Builder 独立 worktree 实现，独占 `cmd/cli/main.go` owner/finalizer seam。 | sealed extractor/private pending type 防 `errors.Join` 假 Done；多个 pending completion 都等待；真实 main 在 pending 时不假报 quiescent；持 root log/exporter 直到真实 owner finished。 |
| directory capture follow-up | `R4-BLD-02` | `c3e5848` 独立复审拒绝，后续修正由同 lane 完成再审。 | 修正原测试的 NUL/argv 合同，并补 fd glob/case/hidden/ignore/root/exit 合同；保留 baseline 的 `--` 选项保护，不将其列为新缺陷。不批量关闭 R4。 |
| Grep bounded capture | `R4-BLD-02` | Grep design 已就绪待 root review；directory 首补丁被拒绝，等待修正独立接受。root 接受 design 且 managed-command writer hook 可用后再开独立 implementation worktree，仅拥有 `internal/tools/grep.go` 与 grep tests。 | handoff 的 JSON framing、single-file/colon/LF paths、shared output cap、drain/join、typed protocol/capture errors、cancellation/pending no fallback。 |
| injected cache-key privacy | `R3-BLD-02` | durable builder 交还 `assemble.go` 后，或由 root 确认无冲突的窄 edit window；只改 `toolCacheKey` callsite/helper/tests。 | raw args 不出现在 injected cache key；marshal fail bypasses cache，执行工具且不同 args 不 alias；nil/default cache 仍不启用。 |
| breaker logical-call ticket | `R3-BLD-02` | BUILD-1 accepted；独占 `internal/production/circuit.go` 与 `internal/cli/output_guard_adapter.go` breaker 部分。 | assembled httptest setup retry/failover 仅一 ticket；等 terminal+join settle；open 零 dispatch、正确中立 cancel/error 分类。 |
| active prepared-attempt detector | `R3-BLD-02` | 采用已接受 R4 frozen trusted identity/effect slice；BUILD-1 accepted；与 R2/core-close 错峰改 `prepared_registry`、`run_commit_consumer`、`loop`。 | 普通 proxy + durable consumer 同一 prepared attempt 观测；threshold 不执行，停止后续 provider，R1 live/durable first-cause 一致。 |
| operation usage/cost accounting | `R3-BLD-02` | 在 R3 contract 锁定 per-dispatch identity/usage semantics、R2/R1 run ownership facts 可复用后，与 breaker 状态机分开做；指定单一 LLM/core owner 避免拼出多种 truth。 | Generate/Stream/continuation/failover 真实 fixture 的 call/usage/cost 对齐；missing usage 为 unknown，不计作零或丢失；无重复记账。 |
| durable compaction | `R3-BLD-03` | R2 prepared-session commit/consumer 已集成后，独占 `internal/core/run_commit_consumer.go` compaction consumer seam 与 compaction/session tests。 | configured summarizer 与首请求 budget preflight；真实 durable CommitCompaction 前后 snapshot/restore 保留 provenance 与 tool-call/result pairing。 |
| R5 snapshot/overlay | `R5-BLD-01` | durable CLI handoff + baseline DTO/adapter review 后；R2 释放 `repl_session.go` 再由 R5 改 transcript adapter/TUI files。 | durable `PublishedView` source IDs 对齐；live provisional row 被同 occurrence snapshot reconcile，不双写/不伪报 done。 |
| R5 Send backpressure | `R5-BLD-01` | 可并行于 durable CLI；先取得 `internal/tui/app.go`/bridge queue 文件 owner，不与 R8 app 同改。 | bounded queue overflow 返回可观察结果并标 incomplete；关键 snapshot/status 可恢复，无 silent success。 |
| R8 viewport app integration | `R8-BLD-01` | R5 stable loaded-row/snapshot overlay seam ready，且 renderer fix 已独立接受；之后独占 `internal/tui/app.go`、`accordion.go`，只加窄 viewport row-revision seam。 | actual Update/View 采用 loaded-only scope、split/per-pane anchor、ephemeral display key；每个 chunk 单 row update 而不是全量 `SetRows`，PTY + app workload/bench。 |
| MCP HotReloader close admission | `R6-BLD-02` / `R7-BLD-01` 生命周期交界 | baseline-seams review 已给 P2；只改 `assembly_lifecycle.go` 的 typed MCP resources admission fence、对应测试，以及 `newAssembleState` 的一个引用赋值，不重写 HotReloader。 | Close invocation 先同步关闭 Reload admission；barrier race 验证 Close 开始后没有 late generation/tool publication，only-one owner 与真实 owner completion/join 保持有效。 |

表中 R2 identity/tree/snapshot 与 R4 prepared effect 已有 integrated accepted slices，不列为新 builder；`R2-BLD-01` 继续 open 只因 composite acceptance/conformance 尚未全闭环。原 task 的 phase-level review 只决定 phase closure，不能阻断以上已接受设计和具备独立验收的 implementation slice。

1. **收束当前 owner，不开重复 lane。** 四个 builder（`R4-BLD-02` directory records、`R8-BLD-01` renderer fix、`R3-BLD-01` Claude tools、`R3-BLD-02` stream BUILD-1）各自交付红绿测试/精确 diff/报告；durable builder 对 `9258330` 只加 Harness pending-lease conformance 后完成 `R2-BLD-02` 真实 CLI wiring。独立 review/整合按 slice 做，不因 `R4-REV-01`/`R8-REV-01` 等整阶段 pending 拒绝已接受实现。`invocation-owner` design 已经 root 两轮 review 接受，Builder 正在独立 worktree 按已接受设计实现；复审后把其证据映射回 R7 原 task。
2. **可并行的下一批窄 slice。** Grep bounded capture 归 `R4-BLD-02`，在 directory lane 的 writer-targeted managed helper 被接受后开始；严格按 `r4-grep-bounded-capture-design-handoff.md`，不改目录 lane ownership。R3 `toolCacheKey` privacy 小修归 `R3-BLD-02`，等 durable builder 释放 `assemble.go` 再做，或由 root 明确窄文件切分；只处理 injected cache key / marshal-failure bypass，default cache 保持 disabled。两者分别是 tools 与 CLI 边界，可不等待 R3/R4 全 phase review。
3. **R2 durable CLI/recovery 收口。** 不重新实现已有 session identity/tree/snapshot, R4 effect 或 pre-owner lease。完成 Harness pending-lease regression 后，R2 owner 接通 durable manager → prompt/REPL → 同一 PreparedSession snapshot → core RunCommit lease → Assembly Close；运行 loopback-provider 创建/工具/中断/exact-resume restart journey。独立 R2 reviewer 看真实 row/index、无重放、错误传播和 active-child quarantine，再记 `R2-REV-01`。与这条链需要共享 `assemble.go`/`repl_session.go` 的任务错峰。
4. **R3 后续能力按语义依赖排序。** stream BUILD-1 接受后才做 BUILD-2 logical breaker ticket；live loop detector 复用已接受 R4 frozen identity/effect，需覆盖 ordinary proxy + durable consumer 和 first-cause terminal。operation-level attempt/usage/cost 独立于 breaker settle 规则验证真实 multi-provider/stream paths；不把现存 Generate usage 或 local retry count 当成完成。`R3-BLD-03` 在 R2 durable consumer/snapshot 连接后接 configured summarizer、budget preflight 与 durable compaction provenance；最后统一按原 R3-BLD01/02/03 acceptance 做 `R3-REV-01`。
5. **R5 snapshot/UI 与 R8 viewport app。** R5 snapshot/overlay 等 durable CLI seam 和 baseline DTO/adapter review；`Send` backpressure 可作为独立有界 R5-BLD01 slice。完成 R5 live/durable reconciliation 后再做完整 R5 PTY。R8 renderer app integration 等 AppendChunk renderer 修复被接受，并复用稳定 loaded projection；按已批准 bounded handoff 保持 split/per-pane anchor、loaded-only search scope 和 private display key，不等 paging。它只产生 R8-BLD-01 partial evidence；R8 capability design/review 与 `R8-BLD-02` 仍按原依赖走。
6. **后续 phase closure。** R4-BLD01/R4-BLD02、R6 design/builder/review、R7 lifecycle/telemetry review 按 task.json DAG 推进；每个 narrow accepted slice 可独立合入。baseline review 已定位 MCP HotReloader Close-entry admission race，按表中窄 P2 修复推进。对无法当前源码确认的 task 先由 owner 做 source audit/测试注册检查，再派修复，不能把旧 ledger 的 defect description 当作当前 RED。
7. **全量集成门禁。** 集成 source 后固定恢复分支 SHA，跑受影响完整 race 和最终 `make verify`，保留失败快照和日志。R0 protected gate 单独满足 exact approval；任何未跑的 PTY、重启、平台、性能或真实 peer 项仍明确标为未验证。

**共享文件锁定：** durable builder 当前拥有 `internal/cli/assemble.go`、`prompt.go`、`repl_session.go` 的 durable seam；在其提交并 handoff 前，Grep/cache/R5 不碰这些文件。Grep 仅在 `internal/tools/grep.go` 和 grep tests/handoff 范围工作；cache mini-slice 之后取得 `assemble.go` 明确时间窗。R3 detector、R2 recovery 与 Assembly-close 若改 `internal/core/loop.go` / `run_commit_consumer.go` 错峰或单 owner。R5 snapshot/overlay 先完成 `internal/tui/bridge.go`/DTO app seam；R8 随后独占 `app.go`、`accordion.go` 与最小 viewport revision API；renderer 文件由单独 renderer owner 持有至独立接受。任何“同包”不是允许并行编辑同文件的依据。

## 台账更新规则

- 仅在 root 提供已整合 SHA、独立 review 结论、命名测试/包 race/PTY/重启等实际日志后更新 `execution-status.md`；按 task 分开记录源码正确性、接线、独立审查、全量门禁和 live/平台证据。
- 单个 accepted slice 保留为 partial；所有原验收全部通过才允许 `done`。失败结果追加，不覆盖历史失败。
- 任何设计、原型、fake sink、静态检查或 builder 自报结果均不等于 durable recovery、生产 CLI 连接、R5/R8 app integration 或 R0 release gate 已通过。
