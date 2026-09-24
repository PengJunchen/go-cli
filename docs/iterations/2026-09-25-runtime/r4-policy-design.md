# R4 权限与工具边界预设计

状态：R4-DES-01 设计已获独立审查批准；R4-BLD-01 approval/Plan 实现与 R4-EFFECT-01 registry/effect 接线正在进行，尚未通过实现审查或完整 gate。D-01 保持当前 `exit_plan_mode` 行为。R1 已完成；R3 相关协议/schema 验收仍是相应实现范围的前置。

## 当前实证

- `internal/tools/registry.go` 允许同名替换，Get 返回当时的真实 definition；全局 Version 递增，但 approval 的 key 仅有 mode/name/args hash。替换执行器后旧授权可以匹配新执行器。
- `internal/tools/middleware_registry.go` 在 Get 包装 Execute，List 返回原定义。实际执行必须始终通过受控 Get 路径；List 不能暴露可直接执行而绕过审批的原定义：应返回保留完整接口的受控 wrapper，或提供只读 descriptor 列表；优先选择与现有 API 兼容的 wrapper。
- `internal/core/plan_mode.go` 的名称白名单是 R0 临时止血。一个叫 read 的扩展不因此拥有只读身份。
- `internal/approval/middleware.go` 仅拒绝 classification == Deny，未识别的枚举可落入执行；session key、effectiveClassifier、record 多次读取当前模式，切换期间不保证同一决策快照。
- `wrappedToolDef` 只暴露 ToolDefinition，可能丢失 streaming/schema/capability 等可选接口。不能为恢复 streaming 而直接绕过 wrapper。

## 效果与信任边界

工具效果至少区分 workspace_read、workspace_write、git_mutation、network_read、network_write、external_effect、control、unknown。效果允许集合，不能把复合工具压缩成最低风险项。Bash/RemoteBash 是任意执行能力；cwd allowlist 不是文件系统沙箱，不能根据命令名称或简单字符串检查推导只读。

声明不是授权：效果及执行器身份必须由可信注册边界产生并绑定具体 definition。第三方声明 read 只能作为描述，不能自行获得第一方 read 的默认授权。MCP/扩展/自定义工具没有可信审查契约时为 unknown；用户可明确批准一次执行，但 Plan Mode 下不得凭一次普通工具批准越过禁止变更的边界。不要在模型可写 Args 中携带权威身份。

最小身份包括注册来源、具体执行器实例/注册代次、能力摘要、规范化工作目录和远端 endpoint scope；动态替换必须使旧授权失效。凭据不能进入身份明文或审计 key。默认先使用进程内注册代次与执行器绑定，跨进程授权只在能够验证稳定能力版本时复用，否则重新询问；不伪造持久身份。

一次 Get 必须取得 definition 与对应身份的同一不可变快照，直到该次调用完成。审批后才重新 Get 同名工具会产生 TOCTOU，应禁止。等待审批期间注册、approval-policy epoch 或 Plan 代次变化，旧 invocation 必须在 dispatch 前失效；当前 invocation 不得 rebase、静默重评或自动弹出第二次审批。新的用户调用才可基于新快照重新准备。最终短临界区同时校验注册/权限/Plan Mode 代次并标记 dispatch committed；从这一点起固定执行同一 definition，后来替换不能偷换执行器。锁不跨用户等待或实际执行持有。工具 wrapper 在调度边界注入身份，所有嵌套 wrapper 必须保留它。

## 决策与缓存

每次审批捕获 mode、approval-policy epoch、Plan Mode 状态及代次、classifier、executor identity/注册代次和 canonical args hash 的同一快照。缓存查询、实际分类、回调和审计使用这份快照。approval-policy epoch 是独立的可撤销授权修订号；正向 memoization grant 的增加不推进它。审批等待期间任何相关权限、Plan Mode 或注册代次变化，都在最终 dispatch commit 前检测；失效时拒绝当前 invocation，不得使用过期批准、rebase 旧 permit、静默重评或自动发起第二次审批。尤其不能先通过 inactive Plan wrapper，再在审批等待时进入 Plan Mode 后继续执行 mutation。锁不跨用户等待或执行持有。

仅合法 Allow 可以调用 next；非法 classification、未知 PermissionMode、nil resolver result、身份缺失均失败关闭，并给出明确错误。Allow Once 不写任何可复用缓存；Always Allow 的 UI 应说明当前作用域，其 key 包含上述真实身份。Deny 的记忆期限必须明确，不将用户单次拒绝固化为跨会话永久拒绝。

正向 grant 与撤销修订分开发布，避免并行 `AlwaysAllow` 准备互相作废。`DispatchFenceSnapshot.Approval` 是独立 approval-policy epoch。一个授权范围内新增的正向缓存 grant 使用窄操作 `PublishApprovalGrant(expected DispatchFenceSnapshot, update func()) (DispatchFenceSnapshot, error)`：`expected` 必须提供，并在同一共享 fence 下精确比较 Registry、Approval、Plan 三个值；更新回调只做短暂内存发布，不执行 I/O。成功时不推进任何 generation，返回当前 post-grant tuple（没有并发撤销时可与 expected 完全相同）。`AlwaysAllow` 的当前调用许可绑定此返回 tuple；缓存项 key 必须同时包含可信 executor identity、canonical argument digest、registry generation、Plan generation 和 approval-policy epoch。这样 A、B 两个已准备调用都获得 `AlwaysAllow` 时，A 发布正向 grant 不会使 B 的 expected tuple 陈旧，二者仍各有一次执行许可。

任何可能撤销或重定义既有授权的状态变化继续经共享 fence 的普通 `Publish` 推进对应 generation：mode/policy/classifier 替换、每次显式 Deny 决策发布、撤销状态写入、grant 清除/删除/替换/重载推进 Approval epoch；单次拒绝不因此被持久化为永久 deny；工具注册替换推进 Registry generation；Plan 转换推进 Plan generation。它们仍会使旧缓存 key 与旧 prepared permit 失效。若 `PublishApprovalGrant` 因完整 expected tuple 已变化而失败，当前 invocation 失败关闭，不将旧 permit 绑定到新 tuple，也不自动重开审批。

Plan Mode 是独立的变更限制，优先于 auto_full。已验证的第一方只读工具及原先明确支持的只读网络工具可继续使用；workspace/git/network mutation、external_effect、unknown 被拒绝。控制工具逐个定义规则，不能因为归入 control 就全部放行。子代理继承同一限制，不能创建权限更高的 registry 或审批路径。Plan 激活若晚于 dispatch commit，无法撤销已经发生的副作用；界面必须显示仍在执行/清理的动作，不能把模式切换显示为进程已停止，取消与资源收敛由 R1 owner 保证。

D-01 模型退出 Plan Mode：已向用户请求产品决策，尚无回复。现有行为由 R0 保留，R4 在此项决策到达前不能自行变更或声称完整验收。推荐模型提出退出请求、用户明确确认后才恢复变更能力；实现是否采用以用户答复为准。

APR-003 Bash：保留现有明确授权模式的含义，但任意命令不能自动降格为 read。interactive/TUI/static/auto/deny 都应走同一效果与身份判定；auto_full 的显式授权不绕过 Plan Mode、无效参数和运行隔离。

## Schema、路径和输出

模型看到的 schema 与 executor 验证来自同一描述或有逐项对照测试。注册清单测试枚举所有实际默认内置工具；unknown/schema 缺失必须标记而非默认为任意对象。非法参数在副作用发生前拒绝。

文件工具的声明相对路径范围需要规范化根路径、解析已存在父路径 symlink，并在创建不存在的最终文件时验证其真实父目录。对抗并发 symlink 改换若无平台级原子路径机制，必须记录剩余 TOCTOU 限制，不能称为安全沙箱。Bash cwd 检查与此文件工具契约分别测试。

grep/find/ls/read/remote capture 在收集源限制字节/条数，不能先无界积累再截断；取消返回 context error，非成功空结果。无 web search provider 必须明确 unavailable，不能返回 mock 成功。R1 PendingExecutionError 需要原样保留，不被包装成普通可重试失败。

## 验收和拆分

1. 身份与效果 seam：同名恶意替换、注册代次改变、原 definition 快照、子代理继承、可选接口经 wrapper 保留；先红后绿。
2. 审批：unknown 枚举拒绝、Allow Once/Always Allow、并行 AlwaysAllow A/B 正向 grant 不推进 epoch 且两许可均可执行、PublishApprovalGrant 必须精确匹配全 tuple、mode/revoke/clear/delete/deny/reload/classifier、Plan 与 registry 更新仍使旧 permit 失效；等待审批时发生变更后当前请求不得 rebase 或二次弹窗。List 执行不绕过、dispatch commit 前后受控竞争、真实 CLI/TUI 路径一致。
3. 工具完整性：默认注册表效果/schema 全枚举，非法参数零副作用、父路径 symlink、bounded capture、取消与无 provider 明确失败。
4. 独立审计后运行 tools/approval/core/cli 相关 race 包；用 HTTP fixture 验证 endpoint 身份和 MCP 替换不会复用授权。

实际测试名称和执行证据由 builder 登记；本文件不把未运行的测试当证据。不修改保护规则、不添加名称豁免、不为了通过门禁伪造接口实现。
