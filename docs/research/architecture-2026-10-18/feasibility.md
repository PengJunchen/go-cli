# P0 可行性验证记录

日期：2026-10-18。基线：`9046ec3246fa89c69bbdd0dcb150672d85b0c5a2`。本文记录独立 PoC、基线 `make verify` 和本机基准；PoC 不是生产实现，数据不构成 Rust 性能对比或生产 SLO。

## 执行环境与边界

主机为 macOS 15.5、darwin/arm64、Apple M1、16 GiB，`go version go1.26.1 darwin/arm64`，默认 `GOMAXPROCS=8`。本机没有 `rustc`/`cargo`，因此没有 Go/Rust 对照。项目 CI 固定 Go 1.24、golangci-lint v2.12.2；本次使用 CI 同版 golangci-lint（临时 `GOBIN`，二进制由 Go 1.26.1 构建）和临时安装的 `goimports`。本机 Go 版本与 CI 不同，macOS 运行也不能替代 Linux/Windows 的平台覆盖。

基线源码由上述 SHA 的独立归档副本运行。全量验证与临时工具安装时，将 `GOCACHE`、`GOTMPDIR`、`GOBIN` 和可执行输出指向 `/tmp/go-cli-architecture-20261018.QZFu4f/current/` 下；PoC 源码、复验日志也保存在该临时根。早期 PoC 首轮缓存路径不作此承诺，未清理或覆盖用户既有全局 Go 缓存。`make fmt` 只作用于归档副本；运行前后比较 1,166 个跟踪 Go 文件没有差异。仓库工作树未写入生产 Go 代码、受保护文件或构建产物。临时副本在 Reviewer 从文档恢复验证完成后由主代理清理。

## 基线全量验证

在基线副本执行 `make verify`，退出码为 **0**，末尾输出 `=== Verification complete ===`。该目标依次通过 `check`（fmt、vet、lint、build、race 测试）、`scan`、`test-log` 和 `test-leak`：golangci-lint v2.12.2 报告 `0 issues`；build 成功；主测试 gate 为 8,867 started、8,861 passed、0 failed、6 skipped，覆盖 28 个包；日志捕获 gate 为 10/10；goroutine leak gate 为 6/6。scanner 扫描 434 个文件，报告 1,807 个 warning、0 个 error，gate 通过。

6 个 skip 按日志原文归类：`TestWithAuditExclusiveLockFallsBackToStrictLockWhenOwnershipIsUnknown`（输出“ownership validation is supported on Darwin and Linux”，该 skip reason 与本机平台的关系不从日志推断）；`TestStdioProcessOwnerRejectsUnsupportedContainmentBeforeStart`（输出“process-group containment is supported on this operating system”）；3 个 `fd` 测试因 `fd is not installed` 跳过；`TestBashTool_ResourceLimitsMemoryOOM` 因“ulimit -v memory enforcement is most reliable on Linux”跳过。这些平台/工具覆盖没有被记成通过。

命令和摘要来自临时副本中的 `make verify`。本机执行 CI 指定 lint 版本，但 Go 编译器版本不是 CI 的 1.24；本次没有把版本差异静默折算成 CI 通过。

## PoC 红绿与独立复审

PoC 是独立、无第三方依赖的 Go module，module 声明 `go 1.24.0`；不调用真实 LLM、MCP server、插件或文件系统。首轮 `poc-red.log` 与第二轮 `poc-review-red.log` 是**编译失败**：分别缺少 `Snapshot`/`Node`/`BuildOrder` 等符号和 `latestReloadIntent`/`ErrRevokedDuringCall`。它们证明测试先于实现提交，但不是运行时合同的行为红测。行为 RED 另有证据：`poc-scope-red.log` 中 `TestScopePanicAndReentrantCloseDoNotStopLIFOCleanup` 因 reentrant `Close` deadlock 失败；独立 Reviewer 的旧源码副本中，8 个生命周期/并发行为用例失败；本轮资源上限补测也在修复前稳定失败：`TestNoCandidateBuildWhileRetiringGenerationIsLeased` 报告 generation A 仍被 lease 持有时 generation C 的 builder 已启动。

修复后，`go test -json -count=1 -timeout=60s ./...` 统计为 16 个顶层测试、16 通过、0 失败、0 跳过、0 子测试；`go test -race -count=1 -timeout=60s ./...` 通过。Builder 的 10 个生命周期回归测试以 `-count=25` 重复通过。随后 Reviewer 将冻结的 4 个源文件复制到 fresh module，并加入其独立对抗测试：26 个顶层测试全量普通与 race 测试通过；同一 26 个顶层测试 `-count=25` 全部通过（8.210s）。Reviewer 对并发 builder 测试指出仍使用短 `time.Sleep`，候选三代门测试也用 50ms 负断言；25 轮没有触发失败，但这不是完全无时序敏感性的证明。

资源门的修复在 `Manager.Reload` 中实现：单候选构建串行；在创建新候选之前等待全部 retiring generation 的 `done`；等待受 context 取消，并在候选 builder 回调前检查请求是否过期。等待期间不持全局管理锁。当前 PoC 只允许已发布代排空后再开始下一候选，避免 A retiring、B current、C candidate 同时占有资源。Reviewer fresh-copy 验证了这一点和取消等待行为。候选回调不合作时只能返回超时/取消状态，Go 无法强制终止同进程 goroutine。

最终 Builder 测试名共 16 个：

- 基础 6 项：`TestBuildOrderIsStableAndRejectsInvalidGraphs`、`TestHookAndPromptPlansAreDeterministic`、`TestLeasePinsOldGenerationAndCandidateFailurePreservesCurrent`、`TestRevokeDeniesOldLeaseAndCancelsInFlightCall`、`TestScopeClosesLIFOAndLimiterHonorsCancellation`、`TestAcquireReloadRaceKeepsSnapshotFieldsCoherent`。
- 生命周期回归 10 项：`TestLatestConcurrentReloadIntentWins`、`TestCallPinsGenerationUntilCallbackReturns`、`TestSnapshotReturnsDetachedValue`、`TestRevokedNonCooperativeCallCannotReturnSuccess`、`TestReloadBuildersAreSerialized`、`TestNoCandidateBuildWhileRetiringGenerationIsLeased`、`TestRetiringGenerationWaitHonorsCancellation`、`TestCloseWaitsForInFlightCandidateCleanup`、`TestScopePanicAndReentrantCloseDoNotStopLIFOCleanup`、`TestScopeCloseInProgressIsExplicitAndWaitable`。

独立 Reviewer 的最终结论是 PoC 生命周期状态机没有已知阻断；该结论只证明小型模型和选定交错，不表示 P0 的所有 AC 或生产 adapter 均验收通过。Reviewer 额外覆盖了队列中 limiter 取消、panic/重入 Close 和 Close 等候 builder 等对抗场景，测试源码与结果在 [review.md](review.md)。

## P0 合同覆盖矩阵

| 合同 | 本轮证据 | 判定与未覆盖范围 |
|---|---|---|
| AC-P0-1 generation lease/drain | 旧 lease 跨发布继续读旧快照；release/in-flight Call 保持旧 Scope 存活，结束后再关闭；A/B/C 三代上限对抗测试通过。 | toy 生命周期已验证；没有生产 runOwner/Assembly adapter。 |
| AC-P0-2 失败 candidate 保留旧代 | builder setup 返回错误时旧代仍 current，候选 Scope 只清理一次。 | 仅通用 builder 错误；没有分阶段 parse/compile/DAG、独占资源回滚或 cleanup 失败状态矩阵。 |
| AC-P0-3 canonical content hash | prompt parts 排序稳定，相同内容得到相同渲染与 SHA-256，prompt 字节变化改变摘要。 | 仅内存 prompt 字符串；未测真实文件同 mtime/size 内容变更、规范化输入/manifest hash 或增量缓存。 |
| AC-P0-4 DAG 与确定性 | 缺依赖、重复 provider、循环会拒绝；单一 fixture 的稳定拓扑、hook 排序和 prompt 排序通过。 | 未覆盖任意 map/注册顺序生成矩阵、hook 冲突/provenance 优先级或生产扩展计划。 |
| AC-P0-5 revoke 与迟到结果 | revoke 后新调用拒绝；合作式 callback 收到取消；忽略 context 的 callback 返回后被标为 `ErrRevokedDuringCall`。 | 只 fence host 接受的结果；callback 已触发的文件/网络等外部副作用无法回滚，也未做真实 mutation adapter 或提交端 policy epoch 集成。 |
| AC-P0-6 有界背压 | limiter 容量、取消和释放路径在 PoC/Reviewer 测试中验证。 | 只限制并发 permit；没有 RPC frame/byte accounting、跨 owner 隔离或队列公平性。 |
| AC-P0-7 红绿、race、复审 | 有实际行为 RED、修复后的普通/race/重复交错，以及 Reviewer fresh-copy 复跑。 | 两个较早的 RED 日志是编译错误，不能用它们单独代替合同失败；时序敏感用例还有短 sleep/负断言。 |
| AC-P0-8 benchmark 报告 | 统一宿主、固定小 fixture 的 `ns/op`、`B/op`、`allocs/op` 已记录在下表。 | 微基准不代表 CLI/Agent turn，不设 SLO；没有 Rust 工具链或同语义比较。 |
| AC-P0-9 生命周期对抗 | stale 发布、在途 pin、快照访问、revoke late result、候选并行构建、panic、重入关闭、Close/build 等待和 deadline 状态都通过 Reviewer fresh-copy。 | 这是状态机边界验证，不是所有平台/资源类型的综合压力测试；builder 并发用例仍含短 sleep。retiring+current+candidate 上限单列在 AC-P0-10。 |
| AC-P0-10 retiring/candidate 资源上限 | A 仍有 lease 时发布 B；请求 C 后验证其 builder 等 A 完成 drain 才启动，防止 current+retiring+candidate 三代同存。 | Builder 与 Reviewer 的 `TestNoCandidateBuildWhileRetiringGenerationIsLeased` 通过；只证明 toy `Scope` 行为，不证明真实资源 RSS 或独占资源双开预算。 |
| AC-P0-11 等待取消（部分） | 在 A 尚未 drain 时取消 C 的 reload context；验证返回 `context.Canceled` 且 C builder 未运行，随后释放 A 并关闭 manager。 | `TestRetiringGenerationWaitHonorsCancellation` 通过；没有显式重读并断言 B 仍为 current，当前证据不覆盖生产 run owner 或多资源撤销事务。 |

因此，P0 的**核心状态机可行性**得到支持，但 AC-P0-2/3/4/5/6 各有明确局部边界，仍需按计划在 P3–P6 对文件、真实资源、policy fence、RPC bytes 和生产 runOwner 逐项验收。不能将“26 个测试通过”改写成“11 项 AC 全部通过”。

## 性能基准

所有数据均为 Go 1.26.1、darwin/arm64、Apple M1 本机 `-benchmem -count=3`。没有另做语言间排名；以下只用于保存研究起点。多次运行波动可见，不能当成跨机器阈值。

### PoC 微基准

命令：`go test -run '^$' -bench 'Benchmark(AcquireLease|HookPlanEmpty|HookPlanOne|PromptRenderSmall|ReloadOneNode)$' -benchmem -count=3`。

| Fixture | 三次 ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `AcquireLease` + `Release` | 48.36 / 37.20 / 33.97 | 24 | 1 |
| 空 hook plan | 2.537 / 2.533 / 2.550 | 0 | 0 |
| 单 hook plan | 4.145 / 4.147 / 10.42 | 0 | 0 |
| 两段小 prompt（约 50 bytes）render + SHA-256 | 268.8 / 269.9 / 272.2 | 536 | 9 |
| one-node reload | 1,181 / 1,221 / 1,288 | 960 | 18 |

`BenchmarkReloadOneNode` 每轮测 candidate build/publish，并等待前一 retired Scope 的 cleanup；最后一个 current Scope 在停止计时后由 `Manager.Close` 关闭，所以不等同完整的从启动到最终 drain 测量。单 hook 第三次耗时和 lease 三次值有明显本机波动。此组仅是机制开销样本，不可据此声明接近 Rust。

### 基线仓库微基准

在归档副本运行：`go test -run '^$' -bench 'Benchmark(BuildToolDefinitions|EventStreamPushPop|SpanLifecycle)$' -benchmem -count=3 ./internal/core ./internal/tracing`。构造 `EventStream` 与 `BuildToolDefinitions/cache_miss` 的基准在计时区域打印大量 INFO 日志，因此这两组计时受 stdout/logging 干扰，不是干净的 CPU 起点；as-run 数值仍在此保留。`cache_hit` 和 `SpanLifecycle` fixture 没有同类计时日志。每项 B/op 与 allocs/op 在三次中保持一致：

| Benchmark | 三次 ns/op | B/op | allocs/op | 解释 |
|---|---:|---:|---:|---|
| `EventStreamPushPop/buf_10` | 4,280 / 4,176 / 4,649 | 3,140 | 14 | 日志污染，不当纯 CPU 基线 |
| `EventStreamPushPop/buf_100` | 19,988 / 19,758 / 19,699 | 24,066–24,067 | 104 | 日志污染，不当纯 CPU 基线 |
| `EventStreamPushPop/buf_1000` | 168,222 / 168,846 / 167,615 | 229,965–229,969 | 1,005 | 日志污染，不当纯 CPU 基线 |
| `BuildToolDefinitions/cache_miss` | 2,809 / 2,957 / 2,979 | 1,057 | 4 | 构造期间有 INFO 输出，结果有噪声 |
| `BuildToolDefinitions/cache_hit` | 20.94 / 20.90 / 21.93 | 0 | 0 | 暖缓存路径，可作本机起点 |
| `SpanLifecycle/attrs_1` | 309.4 / 298.1 / 299.7 | 501 | 8 | 本机起点 |
| `SpanLifecycle/attrs_10` | 1,264 / 1,258 / 1,257 | 2,246 | 30 | 本机起点 |
| `SpanLifecycle/attrs_100` | 9,146 / 9,044 / 9,118 | 17,910 | 213 | 本机起点 |

上述构造日志在原始输出中累计为约 159 MB；报告只保留 24 行基准结果，不保留大体积 stdout。后续性能工作应在 fixture 中关闭/隔离日志，固定 payload 和并发，补冷/暖路径、堆峰值、GC 与延迟分位数。当前数据不能作为 Rust 性能比例或产品资源预算。

## 复现步骤

从仓库根目录将本文末尾四个 fenced source block 重建到临时 module，然后执行测试。脚本只提取明确文件名的 fenced block，不依赖已清理的 `/tmp` 文件：

```sh
POC_REPRO_DIR="$(mktemp -d /tmp/go-cli-architecture-poc-repro.XXXXXX)"
export POC_REPRO_DIR
python3 - <<'PY'
from pathlib import Path
import os
import re

doc = Path("docs/research/architecture-2026-10-18/feasibility.md").read_text()
out = Path(os.environ["POC_REPRO_DIR"])
for name in ("go.mod", "generation.go", "generation_test.go", "review_regression_test.go"):
    match = re.search(rf"### `{re.escape(name)}`\n\n```[^\n]*\n(.*?)\n```", doc, re.S)
    if match is None:
        raise SystemExit(f"missing source fence: {name}")
    (out / name).write_text(match.group(1) + "\n")
print(out)
PY

cd "$POC_REPRO_DIR"
go test -json -count=1 -timeout=60s ./...
go test -race -count=1 -timeout=60s ./...
go test -count=25 -timeout=120s -run '^Test(LatestConcurrentReloadIntentWins|CallPinsGenerationUntilCallbackReturns|SnapshotReturnsDetachedValue|RevokedNonCooperativeCallCannotReturnSuccess|ReloadBuildersAreSerialized|NoCandidateBuildWhileRetiringGenerationIsLeased|RetiringGenerationWaitHonorsCancellation|CloseWaitsForInFlightCandidateCleanup|ScopePanicAndReentrantCloseDoNotStopLIFOCleanup|ScopeCloseInProgressIsExplicitAndWaitable)$' ./...
go test -run '^$' -bench 'Benchmark(AcquireLease|HookPlanEmpty|HookPlanOne|PromptRenderSmall|ReloadOneNode)$' -benchmem -count=3
```

最终冻结源文件 SHA-256：`go.mod` `dae5eee122cbc2016ff04869c24a5698aae8c9336c4cde7900ad87e5f634b2a4`；`generation.go` `4a69e25ba44fef0ea9af48b4c1ae7671cbe9b49124aea8115b19d22dfd098ae5`；`generation_test.go` `067d0d9f55347ef4340ce184cde0726b553f76e36506c4dc1be03edd76b2adab`；`review_regression_test.go` `034391e7e93397b150b2d1af297037f8207e12a6afa271ade2d0aa8f9a055025`。独立 Reviewer 的额外测试与 fresh-copy 结果另见 [review.md](review.md)。

## PoC 源码

以下四个代码块就是可重建的 PoC 源码快照；不依赖本机 Rust、真实 LLM、网络凭证或额外 Go module。

### `go.mod`

```text
module architecture-poc

go 1.24.0
```

### `generation.go`

```go
package generation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

var (
	ErrDenied            = errors.New("capability denied")
	ErrManagerClosed     = errors.New("generation manager closed")
	ErrLeaseReleased     = errors.New("generation lease released")
	ErrRevokedDuringCall = errors.New("capability revoked during call")
	ErrStaleCandidate    = errors.New("candidate superseded by a newer reload request")
	ErrScopeClosing      = errors.New("resource scope close in progress")
	ErrScopeClosed       = errors.New("resource scope closed")
)

type Snapshot struct {
	ID      string
	ToolSet string
	HookSet string
	Prompt  string
}

type Node struct {
	ID       string
	Requires []string
	Provides []string
	Priority int
}

// BuildOrder validates a capability DAG and returns a stable topological order.
// Lower priority values are emitted first among nodes whose dependencies are met.
func BuildOrder(nodes []Node) ([]string, error) {
	byID := make(map[string]Node, len(nodes))
	provider := make(map[string]string)
	for _, node := range nodes {
		if node.ID == "" {
			return nil, errors.New("node id is empty")
		}
		if _, exists := byID[node.ID]; exists {
			return nil, fmt.Errorf("duplicate node id %q", node.ID)
		}
		byID[node.ID] = node
		for _, capability := range node.Provides {
			if capability == "" {
				return nil, fmt.Errorf("node %q provides an empty capability", node.ID)
			}
			if previous, exists := provider[capability]; exists {
				return nil, fmt.Errorf("capability %q provided by both %q and %q", capability, previous, node.ID)
			}
			provider[capability] = node.ID
		}
	}

	indegree := make(map[string]int, len(nodes))
	dependents := make(map[string][]string, len(nodes))
	for _, node := range nodes {
		seen := make(map[string]struct{}, len(node.Requires))
		for _, capability := range node.Requires {
			providerID, exists := provider[capability]
			if !exists {
				return nil, fmt.Errorf("node %q requires missing capability %q", node.ID, capability)
			}
			if providerID == node.ID {
				return nil, fmt.Errorf("node %q depends on its own capability %q", node.ID, capability)
			}
			if _, duplicate := seen[providerID]; duplicate {
				continue
			}
			seen[providerID] = struct{}{}
			indegree[node.ID]++
			dependents[providerID] = append(dependents[providerID], node.ID)
		}
	}

	less := func(left, right string) bool {
		if byID[left].Priority != byID[right].Priority {
			return byID[left].Priority < byID[right].Priority
		}
		return left < right
	}
	ready := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if indegree[node.ID] == 0 {
			ready = append(ready, node.ID)
		}
	}
	order := make([]string, 0, len(nodes))
	for len(ready) != 0 {
		sort.Slice(ready, func(i, j int) bool { return less(ready[i], ready[j]) })
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, dependent := range dependents[id] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}
	if len(order) != len(nodes) {
		return nil, errors.New("dependency cycle detected")
	}
	return order, nil
}

type Hook struct {
	Phase    string
	Priority int
	OwnerID  string
	ID       string
}

type HookPlan struct{ hooks []Hook }

func orderedHooks(hooks []Hook) []Hook {
	ordered := append([]Hook(nil), hooks...)
	sort.Slice(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.Phase != right.Phase {
			return left.Phase < right.Phase
		}
		if left.Priority != right.Priority {
			return left.Priority < right.Priority
		}
		if left.OwnerID != right.OwnerID {
			return left.OwnerID < right.OwnerID
		}
		return left.ID < right.ID
	})
	return ordered
}

func CompileHookPlan(hooks []Hook) HookPlan {
	return HookPlan{hooks: orderedHooks(hooks)}
}

func (p HookPlan) Hooks() []Hook { return append([]Hook(nil), p.hooks...) }

func RunHooks(ctx context.Context, plan HookPlan, run func(context.Context, Hook) error) error {
	for _, hook := range plan.hooks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := run(ctx, hook); err != nil {
			return fmt.Errorf("hook %s/%s: %w", hook.OwnerID, hook.ID, err)
		}
	}
	return ctx.Err()
}

type PromptPart struct {
	Key     string
	Order   int
	OwnerID string
	Text    string
}

func RenderPrompt(parts []PromptPart) (string, string) {
	ordered := append([]PromptPart(nil), parts...)
	sort.Slice(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.Order != right.Order {
			return left.Order < right.Order
		}
		if left.OwnerID != right.OwnerID {
			return left.OwnerID < right.OwnerID
		}
		if left.Key != right.Key {
			return left.Key < right.Key
		}
		return left.Text < right.Text
	})
	var builder strings.Builder
	for index, part := range ordered {
		if index > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(part.Text)
	}
	rendered := builder.String()
	digest := sha256.Sum256([]byte(rendered))
	return rendered, hex.EncodeToString(digest[:])
}

type Scope struct {
	mu       sync.Mutex
	closers  []func() error
	closing  bool
	closeErr error
	done     chan struct{}
}

func NewScope() *Scope { return &Scope{done: make(chan struct{})} }

func (s *Scope) Add(closeFn func() error) error {
	if closeFn == nil {
		return errors.New("nil resource disposer")
	}
	s.mu.Lock()
	if !s.closing {
		s.closers = append(s.closers, closeFn)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	cleanupErr := closeResource(closeFn)
	return errors.Join(ErrScopeClosed, cleanupErr)
}

func (s *Scope) Close() (result error) {
	s.mu.Lock()
	if s.closing {
		select {
		case <-s.done:
			err := s.closeErr
			s.mu.Unlock()
			return err
		default:
			s.mu.Unlock()
			return ErrScopeClosing
		}
	}
	s.closing = true
	closers := s.closers
	s.closers = nil
	s.mu.Unlock()

	var closeErrors []error
	defer func() {
		if recovered := recover(); recovered != nil {
			closeErrors = append(closeErrors, fmt.Errorf("resource disposer panic: %v", recovered))
		}
		result = errors.Join(closeErrors...)
		s.mu.Lock()
		s.closeErr = result
		close(s.done)
		s.mu.Unlock()
	}()
	for index := len(closers) - 1; index >= 0; index-- {
		if err := closeResource(closers[index]); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	return nil
}

// Wait reports completion of the first Close call without confusing an
// in-progress close (including a reentrant disposer call) with a completed one.
func (s *Scope) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func closeResource(closeFn func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("resource disposer panic: %v", recovered)
		}
	}()
	return closeFn()
}

type CandidateBuilder func(context.Context) (*Snapshot, *Scope, error)

type generation struct {
	snapshot  *Snapshot
	scope     *Scope
	refs      int
	retired   bool
	disposing bool
	done      chan struct{}
	closeErr  error
}

type activeCall struct {
	cancel context.CancelFunc
}

type Manager struct {
	mu                 sync.Mutex
	reloadSlots        chan struct{}
	current            *generation
	generations        map[*generation]struct{}
	grants             map[string]bool
	active             map[string]map[uint64]activeCall
	nextCallID         uint64
	latestReloadIntent uint64
	nextReloadID       uint64
	inflightReloads    int
	reloadsIdle        chan struct{}
	reloadCancels      map[uint64]context.CancelFunc
	closed             bool
}

func NewManager(snapshot *Snapshot, scope *Scope, grants []string) *Manager {
	if scope == nil {
		scope = NewScope()
	}
	initial := &generation{snapshot: snapshot, scope: scope, done: make(chan struct{})}
	reloadsIdle := make(chan struct{})
	close(reloadsIdle)
	manager := &Manager{
		current:       initial,
		reloadSlots:   make(chan struct{}, 1),
		generations:   map[*generation]struct{}{initial: {}},
		grants:        make(map[string]bool, len(grants)),
		active:        make(map[string]map[uint64]activeCall),
		reloadsIdle:   reloadsIdle,
		reloadCancels: make(map[uint64]context.CancelFunc),
	}
	for _, capability := range grants {
		manager.grants[capability] = true
	}
	return manager
}

type Lease struct {
	manager  *Manager
	gen      *generation
	released bool // protected by manager.mu; Release linearizes while holding that lock.
}

func (m *Manager) Acquire(ctx context.Context) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.closed || m.current == nil {
		return nil, ErrManagerClosed
	}
	m.current.refs++
	return &Lease{manager: m, gen: m.current}, nil
}

// Snapshot returns a detached value copy. Snapshot contains only scalar fields,
// so callers cannot mutate the generation through this accessor.
func (l *Lease) Snapshot() Snapshot { return *l.gen.snapshot }

func (l *Lease) Release() {
	m := l.manager
	m.mu.Lock()
	if l.released {
		m.mu.Unlock()
		return
	}
	l.released = true
	l.gen.refs--
	shouldDispose := m.markDisposableLocked(l.gen)
	m.mu.Unlock()
	if shouldDispose {
		m.startDispose(l.gen)
	}
}

func (l *Lease) Call(ctx context.Context, capability string, run func(context.Context) error) error {
	m := l.manager
	m.mu.Lock()
	if l.released {
		m.mu.Unlock()
		return ErrLeaseReleased
	}
	if m.closed || !m.grants[capability] {
		m.mu.Unlock()
		return ErrDenied
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return err
	}
	callCtx, cancel := context.WithCancel(ctx)
	l.gen.refs++ // pin the generation until this callback returns, even if Release races it.
	m.nextCallID++
	id := m.nextCallID
	if m.active[capability] == nil {
		m.active[capability] = make(map[uint64]activeCall)
	}
	m.active[capability][id] = activeCall{cancel: cancel}
	m.mu.Unlock()

	if err := callCtx.Err(); err != nil {
		m.finishCall(l.gen, capability, id, cancel)
		return err
	}
	runErr := invokeCall(callCtx, run)
	m.mu.Lock()
	revoked := !m.grants[capability]
	closed := m.closed
	delete(m.active[capability], id)
	if len(m.active[capability]) == 0 {
		delete(m.active, capability)
	}
	l.gen.refs--
	shouldDispose := m.markDisposableLocked(l.gen)
	m.mu.Unlock()
	cancel()
	if shouldDispose {
		m.startDispose(l.gen)
	}
	if runErr == nil && revoked {
		return ErrRevokedDuringCall
	}
	if runErr == nil && closed {
		return ErrManagerClosed
	}
	return runErr
}

func invokeCall(ctx context.Context, run func(context.Context) error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("capability callback panic: %v", recovered)
		}
	}()
	return run(ctx)
}

func (m *Manager) finishCall(gen *generation, capability string, id uint64, cancel context.CancelFunc) {
	m.mu.Lock()
	delete(m.active[capability], id)
	if len(m.active[capability]) == 0 {
		delete(m.active, capability)
	}
	gen.refs--
	shouldDispose := m.markDisposableLocked(gen)
	m.mu.Unlock()
	cancel()
	if shouldDispose {
		m.startDispose(gen)
	}
}

func (m *Manager) Revoke(capability string) {
	m.mu.Lock()
	m.grants[capability] = false
	cancels := make([]context.CancelFunc, 0, len(m.active[capability]))
	for _, call := range m.active[capability] {
		cancels = append(cancels, call.cancel)
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (m *Manager) Reload(ctx context.Context, build CandidateBuilder) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	m.latestReloadIntent++
	intent := m.latestReloadIntent
	m.nextReloadID++
	reloadID := m.nextReloadID
	reloadCtx, cancel := context.WithCancel(ctx)
	if m.inflightReloads == 0 {
		m.reloadsIdle = make(chan struct{})
	}
	m.inflightReloads++
	m.reloadCancels[reloadID] = cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.finishReload(reloadID)
	}()

	// Serialize candidate resource construction to bound peak candidate resources.
	select {
	case m.reloadSlots <- struct{}{}:
		defer func() { <-m.reloadSlots }()
	case <-reloadCtx.Done():
		return reloadCtx.Err()
	}
	if err := m.waitForRetiring(reloadCtx, intent); err != nil {
		return err
	}
	snapshot, scope, err := build(reloadCtx)
	if scope == nil {
		scope = NewScope()
	}
	if err != nil {
		cleanupErr := scope.Close()
		return errors.Join(err, cleanupErr)
	}
	if snapshot == nil || snapshot.ID == "" {
		cleanupErr := scope.Close()
		return errors.Join(errors.New("candidate snapshot must have an id"), cleanupErr)
	}
	if err := reloadCtx.Err(); err != nil {
		cleanupErr := scope.Close()
		return errors.Join(err, cleanupErr)
	}
	candidate := &generation{snapshot: snapshot, scope: scope, done: make(chan struct{})}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cleanupErr := scope.Close()
		return errors.Join(ErrManagerClosed, cleanupErr)
	}
	if intent != m.latestReloadIntent {
		m.mu.Unlock()
		cleanupErr := scope.Close()
		return errors.Join(ErrStaleCandidate, cleanupErr)
	}
	if err := reloadCtx.Err(); err != nil {
		m.mu.Unlock()
		cleanupErr := scope.Close()
		return errors.Join(err, cleanupErr)
	}
	old := m.current
	m.current = candidate
	m.generations[candidate] = struct{}{}
	old.retired = true
	shouldDispose := m.markDisposableLocked(old)
	m.mu.Unlock()
	if shouldDispose {
		m.startDispose(old)
	}
	return nil
}

func (m *Manager) finishReload(id uint64) {
	m.mu.Lock()
	m.inflightReloads--
	delete(m.reloadCancels, id)
	if m.inflightReloads == 0 {
		close(m.reloadsIdle)
	}
	m.mu.Unlock()
}

// waitForRetiring ensures a new candidate is not constructed while a prior
// generation is still draining or disposing. This bounds the live set to the
// current generation plus at most one candidate/retiring generation.
func (m *Manager) waitForRetiring(ctx context.Context, intent uint64) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return ErrManagerClosed
		}
		if intent != m.latestReloadIntent {
			m.mu.Unlock()
			return ErrStaleCandidate
		}
		retiring := make([]chan struct{}, 0)
		for gen := range m.generations {
			if gen.retired {
				retiring = append(retiring, gen.done)
			}
		}
		m.mu.Unlock()
		if len(retiring) == 0 {
			return nil
		}
		for _, done := range retiring {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.current = nil
	toDispose := make([]*generation, 0)
	generations := make([]*generation, 0, len(m.generations))
	for gen := range m.generations {
		gen.retired = true
		generations = append(generations, gen)
		if m.markDisposableLocked(gen) {
			toDispose = append(toDispose, gen)
		}
	}
	reloadsIdle := m.reloadsIdle
	cancels := make([]context.CancelFunc, 0)
	for _, cancel := range m.reloadCancels {
		cancels = append(cancels, cancel)
	}
	for _, calls := range m.active {
		for _, call := range calls {
			cancels = append(cancels, call.cancel)
		}
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	for _, gen := range toDispose {
		m.startDispose(gen)
	}
	var closeErrors []error
	select {
	case <-reloadsIdle:
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, gen := range generations {
		select {
		case <-gen.done:
			if gen.closeErr != nil {
				closeErrors = append(closeErrors, gen.closeErr)
			}
		case <-ctx.Done():
			return errors.Join(ctx.Err(), errors.Join(closeErrors...))
		}
	}
	return errors.Join(closeErrors...)
}

func (m *Manager) markDisposableLocked(gen *generation) bool {
	if gen.retired && gen.refs == 0 && !gen.disposing {
		gen.disposing = true
		return true
	}
	return false
}

func (m *Manager) startDispose(gen *generation) {
	go func() {
		err := gen.scope.Close()
		m.mu.Lock()
		gen.closeErr = err
		delete(m.generations, gen)
		close(gen.done)
		m.mu.Unlock()
	}()
}

type Limiter struct{ permits chan struct{} }

func NewLimiter(limit int) (*Limiter, error) {
	if limit <= 0 {
		return nil, errors.New("limiter capacity must be positive")
	}
	return &Limiter{permits: make(chan struct{}, limit)}, nil
}

func (l *Limiter) Acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case l.permits <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-l.permits
			return nil, err
		}
		var once sync.Once
		return func() { once.Do(func() { <-l.permits }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
```

### `generation_test.go`

```go
package generation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func snapshot(id string) *Snapshot {
	return &Snapshot{ID: id, ToolSet: "tool/" + id, HookSet: "hook/" + id, Prompt: "prompt/" + id}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}

func TestBuildOrderIsStableAndRejectsInvalidGraphs(t *testing.T) {
	nodes := []Node{
		{ID: "leaf", Requires: []string{"cap/a", "cap/z"}, Provides: []string{"cap/leaf"}},
		{ID: "z", Provides: []string{"cap/z"}, Priority: 2},
		{ID: "a", Provides: []string{"cap/a"}, Priority: 1},
	}
	got, err := BuildOrder(nodes)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "z", "leaf"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for _, invalid := range [][]Node{
		{{ID: "consumer", Requires: []string{"missing"}}},
		{{ID: "x", Provides: []string{"same"}}, {ID: "y", Provides: []string{"same"}}},
		{{ID: "a", Provides: []string{"a"}, Requires: []string{"b"}}, {ID: "b", Provides: []string{"b"}, Requires: []string{"a"}}},
	} {
		if _, err := BuildOrder(invalid); err == nil {
			t.Fatalf("BuildOrder(%v) unexpectedly succeeded", invalid)
		}
	}
}

func TestHookAndPromptPlansAreDeterministic(t *testing.T) {
	hooks := []Hook{
		{Phase: "turn", Priority: 2, OwnerID: "b", ID: "z"},
		{Phase: "turn", Priority: 1, OwnerID: "b", ID: "a"},
		{Phase: "setup", Priority: 9, OwnerID: "z", ID: "last"},
		{Phase: "turn", Priority: 1, OwnerID: "a", ID: "x"},
	}
	plan := CompileHookPlan(hooks)
	var got []string
	if err := RunHooks(context.Background(), plan, func(_ context.Context, h Hook) error {
		got = append(got, h.Phase+":"+h.OwnerID+":"+h.ID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"setup:z:last", "turn:a:x", "turn:b:a", "turn:b:z"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hook order = %v, want %v", got, want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := RunHooks(ctx, plan, func(context.Context, Hook) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled hooks: err=%v called=%v", err, called)
	}

	partsA := []PromptPart{{Key: "skill", Order: 2, OwnerID: "s", Text: "two"}, {Key: "core", Order: 1, Text: "one"}}
	partsB := []PromptPart{partsA[1], partsA[0]}
	renderA, hashA := RenderPrompt(partsA)
	renderB, hashB := RenderPrompt(partsB)
	if renderA != "one\ntwo" || renderA != renderB || hashA != hashB {
		t.Fatalf("unstable prompt: (%q,%q) vs (%q,%q)", renderA, hashA, renderB, hashB)
	}
	partsB[1].Text = "too"
	_, changedHash := RenderPrompt(partsB)
	if hashA == changedHash {
		t.Fatal("prompt content change did not change content hash")
	}
}

func TestLeasePinsOldGenerationAndCandidateFailurePreservesCurrent(t *testing.T) {
	var oldClosed, failedClosed atomic.Int32
	oldScope := NewScope()
	if err := oldScope.Add(func() error { oldClosed.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	m := NewManager(snapshot("old"), oldScope, []string{"read"})
	oldLease, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	candidateScope := NewScope()
	if err := candidateScope.Add(func() error { failedClosed.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	buildErr := errors.New("candidate setup failed")
	if err := m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
		return nil, candidateScope, fmt.Errorf("setup: %w", buildErr)
	}); !errors.Is(err, buildErr) {
		t.Fatalf("Reload error = %v, want wrapped cause", err)
	}
	current, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := current.Snapshot().ID; got != "old" {
		t.Fatalf("current generation = %q after failure, want old", got)
	}
	current.Release()
	if got := failedClosed.Load(); got != 1 {
		t.Fatalf("failed candidate disposers = %d, want 1", got)
	}
	if got := oldClosed.Load(); got != 0 {
		t.Fatalf("old generation closed while leased: %d", got)
	}
	newScope := NewScope()
	if err := m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
		return snapshot("new"), newScope, nil
	}); err != nil {
		t.Fatal(err)
	}
	newLease, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if oldLease.Snapshot().ID != "old" || newLease.Snapshot().ID != "new" {
		t.Fatalf("leases mixed generations: old=%q new=%q", oldLease.Snapshot().ID, newLease.Snapshot().ID)
	}
	oldLease.Release()
	oldLease.Release()
	waitFor(t, func() bool { return oldClosed.Load() == 1 })
	if got := oldClosed.Load(); got != 1 {
		t.Fatalf("old generation disposers = %d, want exactly 1", got)
	}
	newLease.Release()
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeDeniesOldLeaseAndCancelsInFlightCall(t *testing.T) {
	m := NewManager(snapshot("g1"), NewScope(), []string{"write"})
	lease, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- lease.Call(context.Background(), "write", func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	<-started
	m.Revoke("write")
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight call error = %v, want canceled", err)
	}
	if err := lease.Call(context.Background(), "write", func(context.Context) error { return nil }); !errors.Is(err, ErrDenied) {
		t.Fatalf("old lease call after revoke = %v, want ErrDenied", err)
	}
	lease.Release()
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestScopeClosesLIFOAndLimiterHonorsCancellation(t *testing.T) {
	var order []int
	scope := NewScope()
	for i := 1; i <= 3; i++ {
		i := i
		if err := scope.Add(func() error { order = append(order, i); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	_ = scope.Close()
	if !reflect.DeepEqual(order, []int{3, 2, 1}) {
		t.Fatalf("close order = %v, want [3 2 1]", order)
	}

	limiter, err := NewLimiter(1)
	if err != nil {
		t.Fatal(err)
	}
	release, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := limiter.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire = %v, want context.Canceled", err)
	}
	release()
	release()
	next, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatalf("limiter leaked permit: %v", err)
	}
	next()
}

func TestAcquireReloadRaceKeepsSnapshotFieldsCoherent(t *testing.T) {
	m := NewManager(snapshot("0"), NewScope(), nil)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 80; i++ {
				lease, err := m.Acquire(context.Background())
				if err != nil {
					return
				}
				s := lease.Snapshot()
				if s.ToolSet != "tool/"+s.ID || s.HookSet != "hook/"+s.ID || s.Prompt != "prompt/"+s.ID {
					t.Errorf("mixed snapshot fields: %+v", s)
				}
				lease.Release()
			}
		}()
	}
	for i := 1; i <= 30; i++ {
		id := fmt.Sprint(i)
		if err := m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
			return snapshot(id), NewScope(), nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkAcquireLease(b *testing.B) {
	m := NewManager(snapshot("bench"), NewScope(), nil)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lease, err := m.Acquire(ctx)
		if err != nil {
			b.Fatal(err)
		}
		lease.Release()
	}
	b.StopTimer()
	if err := m.Close(ctx); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkHookPlanEmpty(b *testing.B) {
	ctx := context.Background()
	plan := CompileHookPlan(nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := RunHooks(ctx, plan, func(context.Context, Hook) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHookPlanOne(b *testing.B) {
	ctx := context.Background()
	plan := CompileHookPlan([]Hook{{Phase: "turn", OwnerID: "builtin", ID: "noop"}})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := RunHooks(ctx, plan, func(context.Context, Hook) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPromptRenderSmall(b *testing.B) {
	parts := []PromptPart{{Key: "core", Order: 1, Text: "You are a helpful assistant."}, {Key: "skill", Order: 2, OwnerID: "skill-a", Text: "Use tools carefully."}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = RenderPrompt(parts)
	}
}

func BenchmarkReloadOneNode(b *testing.B) {
	m := NewManager(snapshot("bench"), NewScope(), nil)
	ctx := context.Background()
	var previousDrain chan struct{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		i := i
		drained := make(chan struct{})
		if err := m.Reload(ctx, func(context.Context) (*Snapshot, *Scope, error) {
			scope := NewScope()
			if err := scope.Add(func() error { close(drained); return nil }); err != nil {
				return nil, scope, err
			}
			return snapshot(fmt.Sprint(i)), scope, nil
		}); err != nil {
			b.Fatal(err)
		}
		if previousDrain != nil {
			<-previousDrain
		}
		previousDrain = drained
	}
	b.StopTimer()
	if err := m.Close(ctx); err != nil {
		b.Fatal(err)
	}
}
```

### `review_regression_test.go`

```go
package generation

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLatestConcurrentReloadIntentWins(t *testing.T) {
	m := NewManager(snapshot("initial"), NewScope(), nil)
	var staleClosed atomic.Int32
	oldStarted := make(chan struct{})
	releaseOld := make(chan struct{})
	oldDone := make(chan error, 1)
	go func() {
		oldDone <- m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
			close(oldStarted)
			<-releaseOld
			scope := NewScope()
			_ = scope.Add(func() error { staleClosed.Add(1); return nil })
			return snapshot("slow-old-candidate"), scope, nil
		})
	}()
	<-oldStarted

	newDone := make(chan error, 1)
	go func() {
		newDone <- m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
			return snapshot("newest-candidate"), NewScope(), nil
		})
	}()
	waitFor(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.latestReloadIntent == 2
	})
	close(releaseOld)
	if err := <-oldDone; err == nil {
		t.Fatal("stale candidate unexpectedly published")
	}
	if err := <-newDone; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return staleClosed.Load() == 1 })
	lease, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if got := lease.Snapshot().ID; got != "newest-candidate" {
		t.Fatalf("current id = %q, want newest candidate", got)
	}
	lease.Release()
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCallPinsGenerationUntilCallbackReturns(t *testing.T) {
	var closed atomic.Int32
	scope := NewScope()
	if err := scope.Add(func() error { closed.Add(1); return nil }); err != nil {
		t.Fatal(err)
	}
	m := NewManager(snapshot("old"), scope, []string{"read"})
	lease, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finish := make(chan struct{})
	callDone := make(chan error, 1)
	go func() {
		callDone <- lease.Call(context.Background(), "read", func(context.Context) error {
			close(started)
			<-finish
			return nil
		})
	}()
	<-started
	lease.Release()
	if err := lease.Call(context.Background(), "read", func(context.Context) error { return nil }); !errors.Is(err, ErrLeaseReleased) {
		t.Fatalf("call after lease release = %v, want ErrLeaseReleased", err)
	}
	if err := m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
		return snapshot("new"), NewScope(), nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := closed.Load(); got != 0 {
		t.Fatalf("generation closed before in-flight call returned: %d", got)
	}
	close(finish)
	if err := <-callDone; err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return closed.Load() == 1 })
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotReturnsDetachedValue(t *testing.T) {
	m := NewManager(snapshot("immutable"), NewScope(), nil)
	lease, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	exposed := lease.Snapshot()
	exposed.ToolSet = "corrupted"
	if got := lease.Snapshot().ToolSet; got != "tool/immutable" {
		t.Fatalf("snapshot mutation escaped lease accessor: %q", got)
	}
	lease.Release()
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRevokedNonCooperativeCallCannotReturnSuccess(t *testing.T) {
	m := NewManager(snapshot("g1"), NewScope(), []string{"write"})
	lease, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finish := make(chan struct{})
	callDone := make(chan error, 1)
	go func() {
		callDone <- lease.Call(context.Background(), "write", func(context.Context) error {
			close(started)
			<-finish // intentionally ignores the revoked context
			return nil
		})
	}()
	<-started
	m.Revoke("write")
	close(finish)
	if err := <-callDone; !errors.Is(err, ErrRevokedDuringCall) {
		t.Fatalf("revoked non-cooperative call = %v, want ErrRevokedDuringCall", err)
	}
	lease.Release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestReloadBuildersAreSerialized(t *testing.T) {
	m := NewManager(snapshot("initial"), NewScope(), nil)
	var active, maximum atomic.Int32
	builder := func(_ context.Context) (*Snapshot, *Scope, error) {
		current := active.Add(1)
		for previous := maximum.Load(); current > previous; previous = maximum.Load() {
			if maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		return snapshot("candidate"), NewScope(), nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.Reload(context.Background(), builder)
		}()
	}
	wg.Wait()
	if got := maximum.Load(); got != 1 {
		t.Fatalf("maximum simultaneous candidate builders = %d, want 1", got)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNoCandidateBuildWhileRetiringGenerationIsLeased(t *testing.T) {
	oldScope := NewScope()
	closed := make(chan struct{})
	if err := oldScope.Add(func() error { close(closed); return nil }); err != nil {
		t.Fatal(err)
	}
	m := NewManager(snapshot("generation-a"), oldScope, nil)
	lease, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
		return snapshot("generation-b"), NewScope(), nil
	}); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
			close(started)
			return snapshot("generation-c"), NewScope(), nil
		})
	}()
	select {
	case <-started:
		t.Fatal("candidate builder started while generation-a was still leased and retiring")
	case <-time.After(50 * time.Millisecond):
	}
	lease.Release()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("generation-a did not drain after its final lease was released")
	}
	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("candidate did not start after the retiring generation drained")
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRetiringGenerationWaitHonorsCancellation(t *testing.T) {
	m := NewManager(snapshot("generation-a"), NewScope(), nil)
	lease, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
		return snapshot("generation-b"), NewScope(), nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- m.Reload(ctx, func(context.Context) (*Snapshot, *Scope, error) {
			close(started)
			return snapshot("generation-c"), NewScope(), nil
		})
	}()
	waitFor(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.inflightReloads == 1
	})
	cancel()
	if err := <-reloadDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("reload waiting for retire drain = %v, want context.Canceled", err)
	}
	select {
	case <-started:
		t.Fatal("candidate builder ran after its wait context was canceled")
	default:
	}
	lease.Release()
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCloseWaitsForInFlightCandidateCleanup(t *testing.T) {
	var cleanup atomic.Int32
	m := NewManager(snapshot("initial"), NewScope(), nil)
	started := make(chan struct{})
	release := make(chan struct{})
	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
			scope := NewScope()
			if err := scope.Add(func() error { cleanup.Add(1); return nil }); err != nil {
				return nil, scope, err
			}
			close(started)
			<-release
			return snapshot("late"), scope, nil
		})
	}()
	<-started
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Close(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close with cancelled wait = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-reloadDone; !errors.Is(err, context.Canceled) && !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("candidate completed after close = %v, want cancellation or ErrManagerClosed", err)
	}
	waitFor(t, func() bool { return cleanup.Load() == 1 })
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestScopePanicAndReentrantCloseDoNotStopLIFOCleanup(t *testing.T) {
	scope := NewScope()
	var order []int
	if err := scope.Add(func() error { order = append(order, 1); panic("fixture panic") }); err != nil {
		t.Fatal(err)
	}
	if err := scope.Add(func() error {
		order = append(order, 2)
		return scope.Close() // reentrant close must not wait on this disposer.
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- scope.Close() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("panic disposer did not produce an error")
		}
	case <-time.After(time.Second):
		t.Fatal("reentrant Close deadlocked")
	}
	if !reflect.DeepEqual(order, []int{2, 1}) {
		t.Fatalf("cleanup order = %v, want [2 1] despite panic", order)
	}
	select {
	case <-scope.done:
	default:
		t.Fatal("scope completion signal not closed after disposer panic")
	}
}

func TestScopeCloseInProgressIsExplicitAndWaitable(t *testing.T) {
	scope := NewScope()
	started := make(chan struct{})
	release := make(chan struct{})
	if err := scope.Add(func() error {
		close(started)
		<-release
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- scope.Close() }()
	<-started
	if err := scope.Close(); !errors.Is(err, ErrScopeClosing) {
		t.Fatalf("concurrent Close = %v, want ErrScopeClosing", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := scope.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait before finalizer completes = %v, want deadline", err)
	}
	close(release)
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if err := scope.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
```
