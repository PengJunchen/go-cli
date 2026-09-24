# R2 Session 持久化 Seam 源码复审

状态：**批准进入 storage 实现**，限于本报告检查的 R2-BLD-01 类型、校验 helper 与接口 seam。此批准不代表 JSONL writer/reader、锁、树恢复、CLI 持久化接线、跨 session quarantine 或真实进程恢复已经实现或验收。

## 固定快照与验证

- 复审快照：`4a8d1380bdfc5107a13abb01dc27e41e24807852`（`fix(session): keep digest validation self-contained`），独立干净 worktree。
- schema 提交链：`baa6d69`、`b5092d9`、`ff1923c`、`230b979`、`221745e`、`df8945e`、`4a8d138`；本报告针对最终 `4a8d138`。底层基线为 `7c8b`（core `ee44750`）。
- 先用 `go test ./internal/session -list` 验证下列 10 个定向测试均被命中，再运行 `go test -race -count=1 ./internal/session`：通过，`ok ... 1.311s`。未运行全仓库测试。
- 保留的前置失败证据：在固定 `df8945e4edf36e0fa7e428b4bb933aad45443e6f` 快照，`go test -list` 编译失败：`durable_contract.go:221:8` 与 `:305:166` 引用的 `isHexDigest` 只存在于当时未提交的 `durable_jsonl.go`。`4a8d138` 将该 helper 放入已提交的 `durable_contract.go:311-321`；本次最终快照编译及 race 测试通过。该失败是已修复的提交边界问题，不能从历史验证记录中抹去，也不是最终快照的当前 blocker。

定向测试命中：`TestSessionEntryV1JSONRoundTrip`、`TestExecutionIdentityValidateRejectsIncompleteOrUnsupportedIdentity`、`TestVersion1AssistantAndFirstReceiptRequirePresentZeroCoordinates`、`TestSessionViewValidateRequiresMessageSourceAlignment`、`TestSessionViewValidateRequiresBranchUniqueSourcesAndNonemptyRevision`、`TestSessionViewCloneDoesNotAliasMessagePayload`、`TestSessionEntryCloneDoesNotAliasNestedToolArguments`、`TestPendingActionRefAllowsOnlyFullyUnreservedOpenOccurrence`、`TestValidatePendingActionDecisionsRequiresExactOrderedResults`、`TestContractInterfaceFixturesAreUsable`。

## 结论与闭环项

### R2S-01 — Session view provenance 约束完整，初审缺口已闭合

**状态：已关闭。** `SessionView.Validate` 要求物理 `SessionID` 和当前 `BranchID`，校验消息与来源 entry 一一对应、来源 entry ID 非空且唯一；非空历史必须带 durable `Revision`，新建空 view 可以暂时没有 revision（`internal/session/durable_contract.go:18-53`）。定向测试覆盖缺 branch、缺非空历史 revision、重复来源 ID、长度不一致及合法空 view（`durable_contract_test.go:206-256`）。这使 session 身份、branch 身份和 message provenance 不会被压扁成单一 history 数组。

### R2S-02 — 零值 transcript 坐标能区分“缺失”和“索引 0”

**状态：已关闭。** `AssistantTranscriptOrdinal` 与 `TranscriptIndex` 使用 `*uint64`，显式零值不会因 `omitempty` 消失（`internal/session/types.go:140-144`）。V1 坐标校验要求 user/assistant/receipt 的必要坐标存在，且 journal intent 不带 transcript index；V0 继续作为 legacy 记录通过（`internal/session/durable_contract.go:201-244`）。测试检查 JSON 字段存在、往返保留零、缺字段拒绝以及 journal-only intent 合法性（`durable_contract_test.go:123-204`）。

### R2S-03 — 复制边界覆盖 canonical message 与新增 schema payload

**状态：通过。** `SessionView.Clone` 复用 `core.CloneAgentMessages` 并复制 `SourceEntryIDs`（`durable_contract.go:55-62`）；`SessionEntry.clone` 通过同一 helper 深拷贝 protocol `ContentBlocks`、`Usage` 和嵌套 tool-call JSON 参数，并复制 execution/tool identity、effect set、typed resolution 和 pointer 坐标（`types.go:156-201`）。测试对嵌套 `map[string]any`/`[]any`、content、usage、tool identity、effect set、typed resolution 做别名检查（`durable_contract_test.go:88-101,258-310`）。该复制合同针对 JSON protocol 值；不能将它当作任意 Go 对象图的通用深拷贝器。

### R2S-04 — pending action authority seam 保持 occurrence identity 且失败关闭

**状态：通过。** `PendingActionRef.Validate` 要求 session/workspace/run/tool-call occurrence key 完整，并要求 reservation 的 `ActionBatchID`、`ActionID` 同时存在或同时为空（`durable_contract.go:83-107`）。decision 校验要求返回数量、顺序和完整 ref 精确匹配，拒绝重复 occurrence、重复 `ActionID`、未知 marker 状态，以及 unreserved occurrence 冒称 `Present`/`Resolved`；unreserved 只允许 `Absent` 或 `Unverifiable`，不会合成新的 `ActionID`（`durable_contract.go:151-199`）。测试覆盖缺失/额外/乱序/不匹配结果、重复 key/ID、未知状态和 unreserved 约束（`durable_contract_test.go:312-392`）。

### R2S-05 — writer/preparer API 足以承载 R2 的最小 session 生命周期

**状态：接口形状通过；具体 durability 与资源生命周期仍是实现门槛。** `DurableSessionWriter.AppendAndSync` 明确把 append、flush、sync 成功作为 ack 边界（`durable_contract.go:12-16`）。`SessionPreparer` 将 new/resume 统一为返回独立 `PreparedSession`；只有 prepare 成功后才切换 runtime，失败时释放新资源且不影响旧 handle。handle 提供深拷贝 view、绑定自身且 Close 后拒写的 writer、幂等 Close（`durable_contract.go:64-81`）。`RecoveryAuthority` 接收已验证的 typed resolution，再按完整 occurrence refs 查询 marker；接口契约禁止回调 session store，适合由 session 锁内验证、workspace index 锁内应用的分层（`durable_contract.go:123-149`）。这些接口在 core/session 的现有包方向内，没有引入新的泛化事件存储框架。

接口注释本身不能证明真实实现遵守承诺；`TestContractInterfaceFixturesAreUsable` 使用空 fake，只证明接口可实现与方法可接线（`durable_contract_test.go:394-446`）。下一阶段必须用实际 JSONL adapter 验证：先拿 session 独占锁再读/恢复；prepare 失败释放所获锁和 store，保留旧 handle；写入错误后 writer poisoned，Close 幂等并释放锁；batch sync 成功前不 ACK/dispatch；部分写入重开后按 entry ID 与 canonical payload 判定耐久前缀；不支持 flock 时失败关闭；权限、文件/父目录 sync 和 torn-tail 依 R2 合同处理。

## storage 实现必须保留的校验门槛

以下是本 seam 快照没有声称完成的 reader/writer 语义，不构成对接口设计的否决。R2-BLD-01 接线时必须为它们提供实现和回归证据：

- **R2S-GATE-01，完整 tool-call group closure。** `validateVersion1EntryCoordinates` 对 assistant 中每个 call 只检查非空 ID/name（`durable_contract.go:251-263`）；单行检查并不验证同一 assistant group 内 call ID 唯一、每个 call 恰有一个相符 receipt、receipt 不孤立、receipt 的 ActionID/ActionBatchID 与 intent 完全一致。恢复投影必须按 `(RunID, AssistantTranscriptOrdinal, ToolCallID)` 配对，拒绝重复/缺失/多余/冲突，不能跨 run 或跨已闭合 group 全局去重。
- **R2S-GATE-02，版本化解码必须调用校验。** `validateVersion1EntryCoordinates` 目前只有 DTO helper 与单测调用点（最终快照中 `rg` 未发现 JSONL reader/writer 调用点，因为 storage 实现尚未提交）。V1 解码须在 entry 进入树/投影前执行 schema 校验；若 execution row 含 `ExecutionIdentity`，应调用 `ExecutionIdentity.Validate` 拒绝无效形状。保留 V0 兼容读取，但不得让未知版本或完整 malformed 行变成可恢复历史。
- **R2S-GATE-03，durable cursor/revision 与 entry ID 映射。** `SessionView` 校验了来源 ID 唯一及 revision 非空，但 storage builder 仍须证明每条投影消息绑定到实际 durable `EntryID`，branch 指向 canonical shared prefix，revision 对应被锁定读取的文件状态；不能从 provider role 或临时 event 反推 durable 来源。
- **R2S-GATE-04，端到端 ACK/恢复。** 此次 seam 测试没有真实文件、故障注入、重开或进程崩溃。必须在 BLD-01/02 后按执行合同证明 append→flush→sync、partial batch、完整 JSON 无换行与 truncated JSON 尾分支、compaction boundary restore、跨锁 marker 恢复、无 provider/tool 自动重放和实际 CLI new/resume 选择。

## 最终判定

最终固定快照的类型、复制和 recovery-authority 接口已足以让 R2-BLD-01 继续实现；目前没有阻断该窄 seam 的缺陷。`df8945e` 的 helper 编译断层已由 `4a8d138` 修复，之前发现的 view 身份/唯一 provenance/非空 revision 和显式零值问题也已闭环。批准不覆盖 storage adapter、loader group closure、JSONL 原子性、CLI 接线、locking、分支/压缩恢复和 crash matrix；这些必须在实际实现固定 hash 上单独验收。
