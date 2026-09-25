# 开发分支合并交接

2026-09-25，用户要求将当前全部修改合并到开发分支，并停止新开任务。本次按该指令收口，不继续推进原 R0–R8 计划，不将合并等同于迭代验收完成。

目标为本地 `develop`。合并源为 `codex/runtime-stabilization`，保留原审计分支与各隔离工作树；不推送远端。冻结时本地和远端均无 `develop` / `dev` 分支，因此从现有 `main` 建立 `develop` 后合并。

## 本次收口范围

- 保留此前已经整合的 Runtime、会话、工具、权限、Provider、ACP/MCP、UI、追踪和验证修改及审计证据。
- 整合当前目录有界捕获、15 个补充工具 schema、受限目录读取身份、TUI 视口、增量渲染原型、会话投影 DTO/adapter，以及 Assembly/ACP/REPL 关闭链路。
- 收录已完成的 Grep、Claude 原生工具协议、流关闭/熔断/循环检测设计与现有独立审查报告。设计文档不代表实现完成。
- R5 投影与 R8 视口/渲染原型尚未完成实际 app 接线；不宣称默认 TUI 已具备完整性能或持久化旅程。
- 新的流关闭 BUILD-1、Claude codec、Grep 实现、目录 NUL/display 修复和 cmd invocation 资源 owner 未开始写产品代码，按收口指令不继续启动。

## 合并时仍然存在的问题

| 范围 | 已知限制 / 未解决问题 | 证据 |
|---|---|---|
| 目录捕获 | Find 的 LF 分帧会拆开带换行的文件名，并丢弃全空白文件名；Read/Ls 批读取的 EOF 取消窗口仍需修复。 | [目录复审](reviews/r4-directory-capture-review.md) |
| 增量渲染原型 | pending grapheme 可突破行宽/字节上限；保留的短字符串仍可能持有整个输入的 backing allocation；最终 source-tail 截断可能切开组合字符。独立复审未接受此原型。 | [渲染复审](reviews/r8-stream-renderer-review.md) |
| 视口 | 修正后的 primitive 已获独立接受；尚无实际 app/PTY 集成证明，也没有本次重新运行的性能基准。 | [视口最终复审](reviews/r8-viewport-model-final-review.md) |
| 关闭链路 | Assembly 局部关闭与等待已增强；但 cmd 顶层仍可能在 Assembly 返回 pending 后提前结束 root span、关闭 exporter/logfile。最新 CLI 接线仅有 builder 验证，未再启动新的独立整体验证任务。 | [原关闭复审及局部记录](reviews/r7-assembly-close-review.md) |
| Claude 原生协议 | 请求未发送工具定义、历史工具消息编码及 Generate 的 tool_use 解码缺口仍未实现；设计还需明确响应 role 与 schema 对象根校验。原生 CLI gate 仍保持关闭。 | [协议设计复审](reviews/r3-native-claude-tools-design-review.md) |
| Grep | 单文件结果解析和全量收集后截断问题尚未实现修复；Go fallback 的 WalkDir 每目录全量排序也是独立内存限制。 | [Grep 设计](r4-grep-bounded-capture-design-handoff.md) |
| Runtime 后续 | 默认流包装链的实际 join、logical-call breaker、活跃循环检测、操作级重试/用量记账、生产持久化接线及完整重启验收仍未闭环。 | [执行台账](execution-status.md)、[R3 交接](r3-breaker-loop-handoff.md) |
| 受保护门禁 | 原 `protected-changes.patch` 已依用户授权应用；补充 `protected-changes-test-gate.patch` 未获授权，保持未应用。 | [门禁复审](reviews/r0-test-gate-wiring-review.md) |

原执行台账保留此前证据状态（14 done / 16 in_progress / 14 pending / 3 deferred），不因本次集中合并批量改为 done。R9 继续 deferred。

## 冻结提交与整合映射

| 修改 | 子分支冻结提交 | 整合提交 |
|---|---|---|
| 目录捕获 | `84dd36e`、`ebfc990` | `4ef0228`、`2290174` |
| 补充工具 schema | `732ba67` | `71a3f71` |
| scoped read | `fea56ea`、`5457d60`、`c4274af`、`73fbf6f` | `c05f937`、`dfd8fd9`、`70c4879`、`a3fb465` |
| TUI 视口及修正 | `e54688c`、`b02a48c` | `7c13811`、`b0ca00e` |
| 增量渲染原型 | `152269b` | `495fb88` |
| R5 投影设计与实现 | `25fea22`、`4ee0c4c`、`4deef5e`、`bb8890a` | `2b59f65`、`ad45281`、`52a918b`、`42e294d` |
| Assembly / ACP / REPL 关闭链 | `93d7fc5` 至 `5781dd8` 的十笔提交 | `7646b10` 至 `cbc9971` 的对应十笔提交 |
| R3 流关闭 / breaker / loop 设计 | `23b66de`、`be3c8c5` | `9b32705`、`7c5e816` |
| Grep 设计 | `1979390`、`553d8f7` | `622785f`、`72741f8` |
| Claude 原生工具协议设计 | `d04c3d6` | `74623b0` |

子代理均已停止且其交付工作树为 clean。相同设计的重复 cherry-pick 未重复引入；被后续提交取代的旧实现不覆盖最终实现。当前 R4 scoped-read 和 R5 DTO/adapter 有 builder 验证，冻结后没有再开启新的独立复审任务。

## 合并检查

待统一检查结束后补录固定代码提交、命令和实际结果。此前 `be08f80` 的完整 `make verify` 通过只证明此前快照，不能替代本次合并后检查。
