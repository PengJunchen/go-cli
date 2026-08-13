# DSH 对齐可行性评估

> 设计文档 #16 · 状态: Draft · 日期: 2026-08-14

## 1. 背景

DSH (Design Pattern System for Hybrid-agents) 是一套面向混合智能体的设计范式体系。
本文档评估 go-cli 向 DSH 五个核心范式对齐的可行性，为长期演进提供决策依据。

评估范围:
1. 事件溯源会话日志
2. 可逆注册效应（Registry disposer）
3. Pre-step 拦截事件
4. Agent 生命周期状态机
5. Inbox 双队列

每个范式包含：现状分析、改造方案、影响面、风险评估、建议优先级。

---

## 2. 范式一：事件溯源会话日志

### 2.1 现状分析

go-cli 当前采用 **消息存储** 模式而非事件存储：

| 层 | 实现 | 持久化 |
|---|------|--------|
| SessionStore | `MemoryStore` / `JSONLSessionStore` | JSONL 追加写入 |
| SessionTree | `DefaultSessionTree` | 分支元数据 |
| EventStream | `EventStreamImpl` (chan AgentEvent, cap=256) | **不持久化** |
| EventBus | `MemoryEventBus` (fan-out) | **不持久化** |

关键文件:
- `internal/session/types.go` — `SessionEntry`, `SessionStore`, `SessionTree` 接口
- `internal/session/jsonl.go` — JSONL 追加存储
- `internal/session/tree.go` — `DefaultSessionTree` 分支树
- `internal/core/eventstream.go` — 临时事件流
- `internal/core/types.go` — `AgentEvent`, `AgentMessage`

**问题**: `EventStream` 捕获了细粒度事件（message, tool_call, tool_result, tool_output, error, done, status, user, token_usage），但这些事件在 turn 结束后丢弃。只有最终 user/assistant 消息被持久化到 SessionStore。无法回放完整执行过程，无法调试 tool call 中间状态。

### 2.2 改造方案

**阶段一：事件日志持久化**

1. 新增 `EventLog` 接口（`internal/session/eventlog.go`）:
   ```go
   type EventLog interface {
       Append(ctx context.Context, event core.AgentEvent) error
       Replay(ctx context.Context, turnID string) ([]core.AgentEvent, error)
       Close() error
   }
   ```

2. 实现 `JSONLEventLog`：追加写入 JSONL 文件，每 turn 一个文件
   - 路径: `~/.go-cli/sessions/<session-id>/events/<turn-id>.jsonl`
   - 格式: 每行一个 `AgentEvent` JSON

3. 在 `LoopAgent.Run` 中，每个事件 emit 时同时写入 EventLog
4. `EventStream` 保持不变，作为实时消费通道

**阶段二：消息投影（deriveMessages）**

1. 新增 `deriveMessages(events []AgentEvent) []AgentMessage` 投影函数
2. `DefaultContextManager.BuildContext` 可选择从 EventLog 重建消息
3. 兼容模式：保留现有 SessionStore 作为快照，EventLog 作为完整源

**阶段三：事件回放**

1. 新增 `ReplayCommand` CLI 命令：`go-cli replay <session-id> <turn-id>`
2. 读取 EventLog，按时间戳回放事件到 TUI

### 2.3 影响面

| 模块 | 文件 | 改动量 |
|------|------|--------|
| session | `internal/session/eventlog.go` (新) | ~150 行 |
| session | `internal/session/jsonl.go` | ~30 行（复用写入逻辑） |
| core | `internal/core/loop.go` | ~20 行（事件双写） |
| core | `internal/core/types.go` | ~10 行（Event 接口扩展） |
| cli | `internal/cli/repl_session.go` | ~15 行（EventLog 初始化） |
| cli | `internal/cli/replay.go` (新) | ~100 行（回放命令） |
| session | `internal/session/context.go` | ~30 行（投影函数） |

### 2.4 风险评估: 中

- **性能**: 双写增加 I/O 开销，但 JSONL 追加写入成本可控（<1ms/event）
- **存储**: 事件日志体积大于消息存储（约 3-5x），需要定期清理策略
- **兼容性**: 现有 SessionStore 接口不变，EventLog 是增量添加
- **一致性**: 双写存在短暂不一致窗口（EventLog 写成功但 SessionStore 失败）

### 2.5 建议优先级: P2

事件溯源是长期调试和回放能力的基础，但当前消息存储已满足基本需求。建议在 Phase 40-41 引入。

---

## 3. 范式二：可逆注册效应（Registry Disposer）

### 3.1 现状分析

go-cli 有 10+ 个 Registry 类型，**没有任何 Register 方法返回 disposer**：

| Registry | Register 签名 | 清理机制 |
|----------|-------------|---------|
| `DefaultToolRegistry` | `Register(ctx, def) error` | 无 |
| `SlashCommandRegistry` | `Register(h) error` | 无 |
| `CommandRegistry` | `Register(cmd) error` | 无 |
| `MCPClientRegistry` | `Register(name, client) error` | 无 |
| `ExtensionRegistry` | `RegisterTool(ctx, t) error` | 无 |
| `SkillRegistry` | `Register(ctx, def) error` | `Unregister(ctx, name) error` |
| `ModelMiddlewareChain` | `Register(mw) error` | 无 |
| `MentionExpander` | `SetResolver(typ, r)` | 无 |

关键文件:
- `internal/tools/registry.go` — `DefaultToolRegistry`
- `internal/cli/slash_registry.go` — `SlashCommandRegistry`
- `internal/cli/registry.go` — `CommandRegistry`
- `internal/skill/skill_registry.go` — `SkillRegistry`（唯一有 Unregister）
- `internal/cli/assemble.go` — `assembleState.cleanupList`（独立清理列表）

**当前清理方式**: `assembleState` 在 `assemble.go` 中维护一个 `cleanupList []func()`，在 `runCleanup()` 中 LIFO 执行。`AgentAssembly` 通过 `Cleanup func()` 字段暴露该清理入口。这与 Registry 完全解耦——注册时不产生清理函数，清理逻辑手动添加。

### 3.2 改造方案

**目标**: `Register` 返回 `func()` disposer，调用者持有并在卸载时执行。

**阶段一：接口变更**

```go
// Before
func (r *DefaultToolRegistry) Register(ctx context.Context, def ToolDefinition) error

// After
func (r *DefaultToolRegistry) Register(ctx context.Context, def ToolDefinition) (func(), error)
```

**阶段二：逐个 Registry 改造**

优先改造有运行时动态注册需求的：
1. `MCPClientRegistry` — MCP 服务器运行时连接/断开
2. `ExtensionRegistry` — 扩展热加载/卸载
3. `SkillRegistry` — 已有 Unregister，改为返回 disposer 包装
4. `DefaultToolRegistry` — 工具动态注册

低优先级（静态注册，启动时一次性）：
5. `SlashCommandRegistry` — 启动时一次性注册
6. `CommandRegistry` — 同上
7. `ModelMiddlewareChain` — 同上

**阶段三：集成到 cleanupList**

`assembleState.cleanupList` 自动收集 disposer：
```go
disposer, err := registry.Register(ctx, def)
if err != nil { ... }
s.cleanupList = append(s.cleanupList, disposer)
```

### 3.3 影响面

| 模块 | 文件 | 改动量 |
|------|------|--------|
| tools | `internal/tools/registry.go` | ~20 行 |
| cli | `internal/cli/slash_registry.go` | ~10 行 |
| cli | `internal/cli/registry.go` | ~10 行 |
| mcp | `internal/mcp/registry.go` | ~15 行 |
| extension | `internal/extension/registry.go` | ~20 行 |
| skill | `internal/skill/skill_registry.go` | ~15 行 |
| llm | `internal/llm/middleware_chain.go` | ~10 行 |
| cli | `internal/cli/assemble.go` | ~15 行（cleanupList 集成） |
| 所有调用点 | 约 20+ 处 | 每处 +3 行（接收 disposer） |

### 3.4 风险评估: 高

- **API 破坏性**: 所有 `Register` 调用者签名变更，影响 20+ 调用点
- **并发安全**: disposer 调用需确保不在 Register/Get 并发进行时执行
- **幂等性**: disposer 多次调用应安全（idempotent）
- **顺序依赖**: LIFO 清理顺序需保证（已由 cleanupList 保证）
- **测试**: 所有 mock registry 需同步修改

### 3.5 建议优先级: P3

当前 cleanupList 机制虽不优雅但功能完整。disposer 模式的核心价值在动态注册场景（MCP/Extension 热卸载），可先针对这两个 Registry 试点。

---

## 4. 范式三：Pre-step 拦截事件

### 4.1 现状分析

go-cli 已有 **五层拦截机制**，但缺乏统一的 pre-step 事件管线：

| 层 | 接口 | 拦截点 | 阻塞能力 |
|----|------|--------|---------|
| LifecycleHook | `BeforeToolCall`, `BeforeRun` | 工具调用前 / Run 前 | ✅ 返回 error 阻塞 |
| ShellHook | `PreToolUse` | 工具调用前 | ✅ allow=false 阻塞 |
| ToolMiddleware | `WrapToolCall` | 工具执行包装 | ✅ 可短路 |
| ModelMiddleware | `WrapModel` | LLM 调用包装 | ✅ 可短路 |
| Middleware | `Wrap` (AgentLoop) | 整个 Run 包装 | ✅ 可短路 |

关键文件:
- `internal/core/extensions.go` — `Hook`, `LifecycleHook`, `Middleware` 接口
- `internal/core/hook.go` — `HookChain` 链式执行
- `internal/core/hook_system.go` — `HookSystem`, `ShellHook`, `HookManager`
- `internal/core/middleware.go` — `ModelMiddleware`, `ToolMiddleware`
- `internal/tools/middleware_registry.go` — `MiddlewareToolRegistry`
- `internal/approval/middleware.go` — `ApprovalMiddleware`

**现状评估**: 拦截能力已经相当完整。`LifecycleHook.BeforeToolCall` 通过 `HookChain` 已实现 pre-step 拦截，`ApprovalMiddleware` 实现了 HITL 审批拦截。Shell hooks 支持外部脚本拦截。

**缺失**:
1. 没有统一的事件总线将 pre-step 拦截事件广播给外部观察者
2. 拦截决策（allow/deny）不作为事件持久化
3. 无法在不修改代码的情况下动态注入拦截规则（仅 ShellHook 支持）

### 4.2 改造方案

**方案: Pre-step 事件广播**

不需要重构现有拦截架构，而是在 `HookChain.BeforeToolCall` 中增加事件广播：

1. 新增 `PreStepEvent` 类型：
   ```go
   type PreStepEvent struct {
       Step     string         // "tool_call" | "model_call" | "run"
       ToolName string         // 工具名（仅 tool_call）
       Args     map[string]any // 参数
       Decision string         // "allow" | "deny" | "modify"
       Reason   string         // 拦截原因
       Timestamp time.Time
   }
   ```

2. `HookChain` 在执行 `BeforeToolCall` 后，将结果广播到 `EventBus`
3. 外部观察者（日志、SSE、审计）可订阅 pre-step 事件
4. 动态拦截规则通过 `HookSystem` 的外部脚本机制实现

**不推荐**: 引入新的拦截管线层。现有 5 层已足够，增加层级会提高复杂度且无明确收益。

### 4.3 影响面

| 模块 | 文件 | 改动量 |
|------|------|--------|
| core | `internal/core/hook.go` | ~30 行（事件广播） |
| core | `internal/core/types.go` | ~15 行（PreStepEvent 类型） |
| core | `internal/core/eventbus.go` | ~5 行（topic 注册） |

### 4.4 风险评估: 低

- 增量添加，不修改现有拦截逻辑
- 事件广播是 fire-and-forget，不影响执行流程
- 唯一风险是 EventBus 发布阻塞（已通过非阻塞 Publish 缓解）

### 4.5 建议优先级: P3

现有拦截机制功能完整，pre-step 事件广播是 nice-to-have 的可观测性增强。

---

## 5. 范式四：Agent 生命周期状态机

### 5.1 现状分析

go-cli **没有形式化的状态机**。生命周期通过隐式机制管理：

| 状态 | 管理机制 | 文件 |
|------|---------|------|
| Idle → Running | `RunSlotGuard.ClaimRun` | `internal/core/run_slot.go` |
| Running → Paused | `LoopAgent.Pause()` (channel) | `internal/core/loop.go:164` |
| Paused → Running | `LoopAgent.Resume()` (close channel) | `internal/core/loop.go:178` |
| Running → Cancelled | `context.CancelFunc` | `internal/core/turnrunner.go` |
| Running → Done | `LoopAgent.Run` return | `internal/core/loop.go:414` |
| Running → Steered | `steerCh chan string` (非阻塞) | `internal/core/loop.go:436` |

关键文件:
- `internal/core/interfaces.go` — `AgentLoop`, `Agent`, `Harness`, `TurnRunner`
- `internal/core/loop.go` — `LoopAgent` (ReAct loop + Pause/Resume)
- `internal/core/agent.go` — `AgentImpl` (有状态包装)
- `internal/core/turnrunner.go` — `EinoTurnRunner` (turn 生命周期)
- `internal/core/run_slot.go` — `RunSlotGuard` (并发控制)

**问题**:
1. 状态转换无显式记录，难以审计
2. 非法状态转换无防护（如已 Cancelled 后再 Pause）
3. Pause/Resume 基于 channel，无法查询当前状态
4. `EinoTurnRunner.running` map 记录运行中 turn，但无状态枚举

### 5.2 改造方案

**方案: 引入 AgentState 枚举 + 状态转换守卫**

1. 定义状态枚举：
   ```go
   type AgentState int
   const (
       StateIdle AgentState = iota
       StateRunning
       StatePaused
       StateCancelling
       StateDone
       StateError
   )
   ```

2. 在 `LoopAgent` 中增加 `state atomic.Int32` + `stateMu`:
   ```go
   func (l *LoopAgent) State() AgentState
   func (l *LoopAgent) transitionTo(from, to AgentState) error // 守卫
   ```

3. 状态转换规则:
   - Idle → Running (Run 开始)
   - Running → Paused (Pause 调用)
   - Paused → Running (Resume 调用)
   - Running/Paused → Cancelling (Cancel 调用)
   - Cancelling → Done/Error (Run 返回)
   - Running → Done/Error (正常结束)

4. 在 `EinoTurnRunner` 中暴露 `TurnState(id string) AgentState`
5. 状态转换通过 `EventBus` 广播 `StateChangeEvent`

### 5.3 影响面

| 模块 | 文件 | 改动量 |
|------|------|--------|
| core | `internal/core/loop.go` | ~60 行（state 字段 + 转换逻辑） |
| core | `internal/core/agent.go` | ~15 行（状态代理） |
| core | `internal/core/turnrunner.go` | ~20 行（TurnState 查询） |
| core | `internal/core/types.go` | ~20 行（AgentState + 事件类型） |
| tui | `internal/tui/bridge.go` | ~10 行（状态显示） |
| test | 新增状态机测试 | ~80 行 |

### 5.4 风险评估: 中

- **并发**: `atomic.Int32` + CAS 保证状态转换线程安全
- **兼容性**: 新增方法不破坏现有接口
- **复杂度**: 状态守卫逻辑需仔细测试，特别是 Pause/Cancel 竞态
- **收益**: 显式状态查询能力，支持 TUI 状态显示和审计

### 5.5 建议优先级: P2

显式状态机对调试和可观测性有直接价值，且改动相对内聚。建议 Phase 40 引入。

---

## 6. 范式五：Inbox 双队列

### 6.1 现状分析

go-cli 已有 **多队列消息传递系统**，但不是 DSH 范式中的 Inbox 双队列：

| 队列 | 类型 | 容量 | 用途 |
|------|------|------|------|
| EventStream | `chan AgentEvent` | 256 | 实时事件传递 |
| EventBus | fan-out | 64/sub | 多消费者广播 |
| steerCh | `chan string` | 16 | 运行中注入指令 |
| followUpCh | `chan string` | 16 | 运行中注入追问 |
| approvalCh | `chan ApprovalRequest` | 32 | HITL 审批 |

关键文件:
- `internal/core/loop.go:48-49` — `steerCh`, `followUpCh`
- `internal/core/turnrunner.go:38-39` — TurnRunner 级 steer/followUp
- `internal/cli/repl_session.go:216` — approvalCh 创建
- `internal/core/eventstream.go` — EventStream
- `internal/core/eventbus.go` — EventBus

**DSH Inbox 双队列范式**: 将用户输入分为 (1) 即时处理队列和 (2) 延迟处理队列。运行中的 agent 从延迟队列消费，空闲时处理即时队列。

**现状对比**:
- `steerCh` + `followUpCh` 类似延迟队列（运行中注入）
- 但没有统一的 Inbox 抽象，队列分散在 LoopAgent 和 TurnRunner 中
- 没有优先级机制
- 没有持久化（agent 重启后队列丢失）

### 6.2 改造方案

**方案: 统一 Inbox 抽象**

1. 定义 `Inbox` 接口：
   ```go
   type Inbox interface {
       // Submit 向 inbox 投递消息
       Submit(ctx context.Context, msg InboxMessage) error
       // Drain 非阻塞地排空所有待处理消息（按优先级）
       Drain(ctx context.Context) []InboxMessage
       // Pending 返回待处理消息数
       Pending() int
   }

   type InboxMessage struct {
       Type     InboxMessageType // steer, followup, user_input, system
       Content  string
       Priority int              // 0=最高
       At       time.Time
   }
   ```

2. 实现 `DualQueueInbox`:
   - `priorityQueue` — 高优先级（steer, system），容量 16
   - `normalQueue` — 普通优先级（followup, user_input），容量 32
   - `Drain()` 先排空 priorityQueue，再排空 normalQueue

3. 在 `LoopAgent` 中用 `Inbox` 替换 `steerCh` + `followUpCh`:
   ```go
   // Before
   drainSteerMessages(l.steerCh, &msgs, l.logger)
   drainFollowUpMessages(l.followUpCh, &msgs, l.logger)

   // After
   for _, msg := range l.inbox.Drain(ctx) {
       msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: msg.Content})
   }
   ```

4. `EinoTurnRunner` 持有 Inbox 实例，`Steer`/`FollowUp` 方法改为 `inbox.Submit`

### 6.3 影响面

| 模块 | 文件 | 改动量 |
|------|------|--------|
| core | `internal/core/inbox.go` (新) | ~120 行 |
| core | `internal/core/loop.go` | ~30 行（替换 drain 逻辑） |
| core | `internal/core/turnrunner.go` | ~25 行（Submit 替换 channel send） |
| cli | `internal/cli/repl_session.go` | ~15 行（Inbox 初始化 + 回调改造） |
| test | 新增 Inbox 测试 | ~80 行 |

### 6.4 风险评估: 中

- **行为变更**: steer/followUp 从有缓冲 channel（cap=16）改为有缓冲队列，行为略有变化
- **顺序保证**: 现有 steer 先于 followUp drain 的顺序通过优先级保证
- **并发**: Inbox 内部用 mutex 保护双队列，Drain 在 LoopAgent 迭代顶部调用（单线程）
- **向后兼容**: steerCh/followUpCh 可保留为兼容层，内部代理到 Inbox

### 6.5 建议优先级: P3

当前 steer/followUp 双 channel 机制功能完整。Inbox 抽象的主要价值在统一管理和优先级支持，可在 Phase 41+ 考虑。

---

## 7. 优先级排序总结

| # | 范式 | 优先级 | 风险 | 核心价值 | 建议阶段 |
|---|------|--------|------|---------|---------|
| 4 | Agent 生命周期状态机 | P2 | 中 | 显式状态查询、调试可观测性 | Phase 40 |
| 1 | 事件溯源会话日志 | P2 | 中 | 完整执行回放、调试基础 | Phase 40-41 |
| 3 | Pre-step 拦截事件 | P3 | 低 | 审计可观测性增强 | Phase 41 |
| 5 | Inbox 双队列 | P3 | 中 | 统一消息管理、优先级 | Phase 41+ |
| 2 | 可逆注册效应 | P3 | 高 | 动态卸载、资源清理 | Phase 41+（先 MCP/Extension 试点） |

## 8. 建议的演进路径

```
Phase 40: Agent 状态机 + 事件日志持久化（范式 4 + 1）
    ↓
Phase 41: Pre-step 事件广播 + Inbox 统一抽象（范式 3 + 5）
    ↓
Phase 42+: Registry disposer 试点（范式 2，先 MCP/Extension）
```

每个阶段独立交付，不相互依赖。范式 1 和 4 有协同效应（状态转换作为事件记录），建议同期实施。

---

## 9. 结论

go-cli 当前架构在拦截机制（范式 3）和多队列消息传递（范式 5）方面已有较完善的实现，DSH 对齐的增量价值有限。最值得投入的是 **Agent 生命周期状态机**（范式 4）和 **事件溯源会话日志**（范式 1），它们直接提升调试能力和可观测性。Registry disposer（范式 2）因 API 破坏性大且当前 cleanupList 已满足需求，建议延后并在动态注册场景试点。
