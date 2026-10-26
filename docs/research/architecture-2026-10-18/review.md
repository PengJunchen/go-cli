# 架构与 PoC 独立审查

日期：2026-10-18。审查者：OpenCode v2 专项 Explorer / Reviewer。

## 审查结论

推荐采用 typed Go 模块化单体：控制面加载并校验来源、配置和扩展，再编译不可变的能力计划；运行面通过 run admission 选定计划，以 generation lease、invocation ref 和 owner Scope 保证资源寿命；命令、provider、skill、tool、hook 等各有真正的消费点。公开 API/schema 与内核实现分层，外部 subprocess RPC 是可选隔离后端，不预先引入 WASM 或把全部插件 API 塞入一个可导入所有 `internal/` 包的 context。

目标架构把旧代排空、撤权后拒绝迟到结果、资源峰值和回滚限制都明确写出，方向可靠。Builder 完成二次修复后，我在独立 fresh copy 中复跑；8 个初始对抗点、跨 retiring/candidate 资源峰值检查、普通测试、race 测试及 26 个顶层测试各 25 轮均通过。结论是**PoC 生命周期机制可行**，不是 P0 全部合同已完成，更不是生产实现已验证；P1–P7 仍未实施。

## 源码和测试证据

OpenCode 专项报告固定到官方 v2 ref 当日 SHA `4617210822bdbb31e5749751afbfb4584ccedac8`。报告中的 GitHub blob/tree permalink 已检查为 40 位 SHA，路径和行号可在该固定 checkout 中定位。`dev` 与 `v2.0.32` 是不同 ref；研究没有把它们混进 v2 证据。OpenCode 项目源码测试未运行；报告以固定 commit 的源码、测试定义和官方 GitHub 页面为证据，不声称测试运行通过。

PoC 首轮普通与 race 命令曾通过，但随后独立对抗检查发现状态机缺陷。最终冻结快照由以下四个文件组成，SHA256 已与 Builder 提供的冻结值逐一比对：

| 文件 | SHA256 |
|---|---|
| `go.mod` | `dae5eee122cbc2016ff04869c24a5698aae8c9336c4cde7900ad87e5f634b2a4` |
| `generation.go` | `4a69e25ba44fef0ea9af48b4c1ae7671cbe9b49124aea8115b19d22dfd098ae5` |
| `generation_test.go` | `067d0d9f55347ef4340ce184cde0726b553f76e36506c4dc1be03edd76b2adab` |
| `review_regression_test.go` | `034391e7e93397b150b2d1af297037f8207e12a6afa271ade2d0aa8f9a055025` |

随后我将 PoC 独立复制到 `/tmp/go-cli-architecture-20261018.QZFu4f/review-opencode/poc`，不改 Builder 原目录。旧快照上我通过 channel barrier 触发 stale publish、lease release、revoke 等竞态，8 个生命周期/并发检查失败；queued limiter cancellation 检查通过。这些是**行为失败**，证明首版违反对应合同。二次修复后，fresh copy 的复跑结果如下：

| 独立复跑命令 | 结果 |
|---|---|
| `go test -count=1 -timeout=30s ./...` | 通过，fresh copy 共 26 个顶层测试（Builder 16 个：base 6 + regression 10；Reviewer 10 个） |
| `go test -race -count=1 -timeout=60s ./...` | 通过，同一 fresh copy 全套测试，无 race 报告 |
| `go test -count=25 -timeout=120s -run '^Test' ./...` | 全部 26 个顶层测试各运行 25 轮，通过，8.210 秒 |

旧阶段的红日志须区分行为失败和编译失败：`poc-red.log` 因 `Snapshot`、`Node`、`BuildOrder` 等未定义而编译失败；`poc-review-red.log` 因 `latestReloadIntent`、`ErrRevokedDuringCall` 尚未实现而编译失败；`poc-scope-red.log` 则有真实行为失败（finalizer 重入 `Close` 死锁）。因此不能把所有红日志都描述为合同测试先红。后续独立审查确实在旧实现上运行行为对抗测试并捕获了 8 个失败，再与 Builder 一起补测修复；跨 retiring/candidate 的资源峰值还捕获了第 9 个具体问题，`poc-budget-red.log` 记录其失败。最终冻结 POC 有 16 个 Builder 顶层测试（base 6 + regression 10）；Reviewer 副本额外有 10 个测试。最终独立复测的普通/race 全套均为 26 个顶层测试；25 轮命令重复全部 26 项，而非 17 项子集。

| 对抗场景 | 旧快照观察 | 修复后独立复跑 |
|---|---|---|
| 慢 candidate 在更新请求后完成 | 旧配置可覆盖新配置 | `TestLatestConcurrentReloadIntentWins` + 独立对抗测试通过；stale 候选被拒绝并清理 |
| Parent lease release 与 callback 并发 | callback 仍在运行时 owner 可被关闭 | `TestCallPinsGenerationUntilCallbackReturns` + 独立对抗测试通过；在途 invocation pin 保活 |
| Snapshot accessor 暴露的值被调用方修改 | 后续 reader 看到被篡改的共享快照 | value-copy 测试通过；当前 Snapshot 只有 string 标量，slice/map/nested deep-copy 尚未验证 |
| Revoke 与忽略 context 的 callback 完成竞态 | callback 返回 `nil` 被接受为成功 | `TestRevokedNonCooperativeCallCannotReturnSuccess` + 独立对抗测试通过；迟到返回为 `ErrRevokedDuringCall` |
| 并行 reload candidate 构建 | 可同时构建多个 candidate | serial builder 与最新 intent fence 测试通过；代码层用 `reloadMu` 串行，candidate overlap 边界另由下一行验证 |
| retiring + candidate 叠代峰值 | A 仍被 lease 持有并 retiring、B 为 current 时，C candidate builder 已启动，出现三代重叠 | `TestNoCandidateBuildWhileRetiringGenerationIsLeased` 与 `TestRetiringGenerationWaitHonorsCancellation` 通过；候选构建现在等待 retiring owner 完成，并在等待被取消时不启动 builder |
| Scope finalizer panic | 后续 cleanup 被跳过，`done` 不关闭 | panic 捕获、继续 LIFO cleanup、最终 `Wait` 状态测试通过 |
| finalizer 重入 Scope.Close | 等待自己的 `done`，死锁 | 重入得到 `ErrScopeClosing`，外层 cleanup 完成；单独 `Wait(ctx)` 等待结束 |
| Manager.Close 与候选 builder 并发 | Close 可在未登记的 builder 仍运行时返回成功 | Close 跟踪 inflight reload；候选完成后拒绝 publish 并清理；deadline 可返回 context 错误 |

这些对抗测试验证的是 toy 状态机，不等于生产实现验证。按本轮 P0 合同清单，11 项合同不能由 16 个 Builder 顶层测试或独立复测代表全通过。当前仍未覆盖真实文件保留 mtime/size 后内容变化、字节配额、独占资源切换/回滚、SessionStore commit fence、以及真实外部副作用状态；也没有触及生产 Assembly、MCP adapter 或第三方 provider。toy Snapshot 只有标量，不能证明未来 slice/map/nested schema 的深拷贝正确。toy limiter 只验证并发 permit，不代表 queued bytes 上限。新的 retiring 等待测试证明小型 Lease/Scope 场景下不会同时建立第三代；它不覆盖候选资源类型之间的所有组合峰值。

### 独立复跑记录

冻结的 Builder 源码已从 `current/poc` 复制到独立 fresh copy；Reviewer 的额外测试代码在本文件附录，提取后组成 26 个顶层测试。完整普通、race 和 25 轮重复运行均通过：

```sh
cd /tmp/go-cli-architecture-20261018.QZFu4f/review-opencode-final2/poc
go test -count=1 -timeout=30s ./...
go test -race -count=1 -timeout=60s ./...
# 全部 26 个顶层测试，重复 25 轮
go test -count=25 -timeout=120s -run '^Test' ./...
```

我又从 [feasibility.md](feasibility.md) 的四个源码 fence 重建 module，并从本附录恢复 `review_test.go` 到 `/tmp/go-cli-architecture-20261018.QZFu4f/current/doc-repro/final-from-feasibility`。四个源码文件哈希与冻结值一致，Reviewer 测试哈希为 `8820bd91babe72406ec94e804ea3e64d767ce0ac458c94228a1645a7251cc9b2`。文档重建副本的 `go test -count=1 -timeout=30s ./...`、`go test -race -count=1 -timeout=60s ./...`、全 26 项 `-count=25` 以及 retiring/candidate 与 Close/candidate targeted tests 均通过；重复测试耗时 8.127 秒。这验证的是报告中所存源码和测试可恢复、可运行。

stale revision、在途 Call 生命周期、retiring 资源等待与 Close/candidate cleanup 使用显式 channel / manager-state barrier。serial builder 最大并行度测试和若干负向等待断言使用短 timeout/sleep；25 轮独立复跑未触发失败，但这仍比纯调度无关的事件断言弱。该测试证据只覆盖 PoC 的小型状态机。

## 架构审查要点

### generation 与 owner

候选完整构建后再发布、旧 generation 只接收已有 run、资源由 Scope 逆序释放，都是值得保留的方向。实现必须把 acquire/publish/retire 做成线性化协议，不应仅依赖 `atomic.Pointer`。Scope 的关闭需在 panic、超时和重入下都给出明确状态；忽略 context 的同进程回调只能进入 stuck/unknown 诊断，不能被宣称已经强制停止。

资源预算应覆盖完整峰值：candidate 构建期间的 current+candidate；publish 后 current+retiring；清理失败或非合作调用仍在运行时的占用也不能从统计中消失。独占端口、唯一设备、文件锁和长期 SessionStore/schema 不适合普通双代热切换。对独占资源要返回 `restart-required` 或执行明示的 stop-start cutover，并说明旧资源停止后新候选失败时可恢复到什么程度；不能保证没有停机且可以完整回滚。

跨 generation 复用只适用于明确可共享的不可变制品或受专门 ref-count owner 管理的对象。cache key 应包括来源身份/信任范围、规范化输入、依赖与编译器版本、相关 policy revision；引用记录应携带 source/project/owner provenance，最后引用退出才释放。不能让同内容的两个来源跨信任或项目边界共享带状态资源。

### 权限和 late result

policy/trust epoch 应独立于 generation。撤权后应拒绝新 invocation，并对在途合作式 callback 发出取消；callback 忽略 context 时，主机仍需拒绝迟到结果、事件和 commit。对任意插件的不可逆外部副作用，撤权和真正写入之间无法由通用 host wrapper 做成事务：契约应说明调用已获 invocation token 后可能继续产生外部副作用，host completion fence 只阻止接纳结果，无法撤回副作用；未知状态须标为 `unknown/partial`。若某类文件变更需要更强顺序保证，应由相应 mutation adapter 提供可串行化 admission，而不是声称所有回调均可即时杀停。

run pin 应绑定 runOwner 覆盖 agent、并行调用、子任务、stream/event producer 和异步 commit；Session 不持有永久 generation lease。旧 generation 中仍有效的 run 可以提交自己的结果，但必须证明它仍是 Session 当前的 run owner，并经过当前 policy/trust 与 invocation epoch fence。

### API 与模块依赖

`pkg/extensionapi/v1` 应只承载稳定 DTO/协议和兼容规则，不反向依赖 `internal/core`、`internal/tools`、`internal/llm`。host 内部 adapter 可以依赖公开合同，并按 consumer 注入最小接口。保持单 repo 模块化单体即可；没有第三方版本独立发布需求时，不急于拆 Go module。每种注册都必须证明 CLI 实际消费：command 在 route 解析前，provider 在 model resolver 前，skill 经生产 `List/Match`，tool/hook/prompt 投影到同一 run 能力计划。

## 性能与基线边界

PoC 数据只是小对象模型的微基准，不是生产 SLO。最终 `BenchmarkReloadOneNode` 每轮等待前一候选 Scope 的 drain；bench timer 停止后由 `Manager.Close` 关闭最后一个 current Scope，因此不是完整 Agent/reload 生命周期。最终三次数据、基线仓库微基准和各自限制统一记录在 [feasibility.md 的性能基准](feasibility.md#性能基准)；这里不重复早期或过期数字。后续性能工作须分开给出 publish latency 与完整 drain/cleanup，使用可重复 fixture，并报告冷/暖路径、相同 payload、并发、p50/p95/p99、CPU、RSS、GC 与峰值 overlap。不能把 lease 纳秒数推断到完整 Agent turn，也不能据此宣称 Go 与 Rust 性能等价。

baseline 快照 commit 为 `9046ec3246fa89c69bbdd0dcb150672d85b0c5a2`。`make verify` 已执行到 `=== Verification complete ===`：格式、vet、golangci-lint（0 issues）、build 通过；全量 race test gate 8,867 个 test function started、8,861 passed、6 skipped、0 failed；verify 的日志捕获与 goroutine leak gate 也通过。该结果是 Go 1.26.1 darwin/arm64 Apple M1 本地快照验证，不等于 Go 1.24 CI。日志中 scanner 有存量 WARN（例如 SCAN-006 / SCAN-008），但 scan gate 以 0 errors 完成。上述基线与临时 PoC 不能替代生产实现验收。

## 阶段建议与阻断条件

- **P0**：冻结版 toy 状态机的普通、race、25 轮全测试和 benchmark 证据已复核；只代表机制可行。完整合同仍有缺项，且两种初始 RED 日志是编译失败而非行为失败。补齐真实同 mtime/size digest、bytes budget、nested snapshot 等测试前，P0 不可标记全通过，也不能接生产。
- **P1–P3**：窄 Assembly seam、公开 schema 和真实 consumer、内容 digest/DAG/确定性 plan 分开推进。不要在 P2 只测 `Register` 调用而未证明可达消费者。
- **P4**：先验收 revision fence、ref 生命周期、panic/close、current/candidate/retiring 峰值、exclusive resource 路径和 scope/source-limited cache ref，再接生产 generation。
- **P5**：通过真实 runOwner 和 SessionStore fence 测试后，才允许 turn boundary adopt。撤权新 invocation 和 in-flight副作用状态应分别验收。
- **P6–P7**：RPC 是显式 opt-in 的过程隔离后端；它本身不是 OS sandbox。WASM 仅在真实隔离/跨语言需求和 adoption gate 满足后另立 ADR。基准比较必须同语义、同宿主、同输入。



## 附录：Reviewer 独立对抗测试源码

这 10 个顶层测试与冻结 Builder 的 16 个顶层测试共同组成独立复测的 26 项。它们使用同 package 的 `snapshot` 和 `waitFor` helpers；从 feasibility 文档恢复 PoC module 后，将此源码提取为 `review_test.go` 再运行普通、race、重复验证。

提取命令（先把 `POC_DIR` 设为 feasibility 恢复出的 PoC module 目录）：

```sh
POC_DIR=/tmp/go-cli-architecture-20261018.QZFu4f/repro/poc
python3 - "$POC_DIR/review_test.go" <<'PY'
from pathlib import Path
import sys

markdown = Path("docs/research/architecture-2026-10-18/review.md").read_text()
marker = "<!-- reviewer-test-source:start -->"
start = markdown.index("```go\n", markdown.index(marker)) + len("```go\n")
end = markdown.index("\n```\n<!-- reviewer-test-source:end -->", start)
Path(sys.argv[1]).write_text(markdown[start:end] + "\n")
PY
cd "$POC_DIR"
go test -count=1 -timeout=30s ./...
go test -race -count=1 -timeout=60s ./...
go test -count=25 -timeout=120s -run '^Test' ./...
```

<!-- reviewer-test-source:start -->
```go
package generation

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestReviewConcurrentReloadDoesNotPublishStaleCandidate(t *testing.T) {
	m := NewManager(snapshot("initial"), NewScope(), nil)
	started := make(chan struct{})
	finishSlow := make(chan struct{})
	var slowErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		slowErr = m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
			close(started)
			<-finishSlow
			return snapshot("older-candidate"), NewScope(), nil
		})
	}()
	<-started
	newDone := make(chan error, 1)
	go func() {
		newDone <- m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
			return snapshot("newer-candidate"), NewScope(), nil
		})
	}()
	waitFor(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.latestReloadIntent == 2
	})
	close(finishSlow)
	wg.Wait()
	if !errors.Is(slowErr, ErrStaleCandidate) {
		t.Fatalf("older reload result = %v, want ErrStaleCandidate", slowErr)
	}
	if err := <-newDone; err != nil {
		t.Fatal(err)
	}
	lease, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if got := lease.Snapshot().ID; got != "newer-candidate" {
		t.Fatalf("stale reload won publication: current=%q, want newer-candidate", got)
	}
}

func TestReviewReleaseDoesNotDisposeScopeDuringCall(t *testing.T) {
	closed := make(chan struct{})
	oldScope := NewScope()
	if err := oldScope.Add(func() error { close(closed); return nil }); err != nil {
		t.Fatal(err)
	}
	m := NewManager(snapshot("old"), oldScope, []string{"read"})
	lease, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
		return snapshot("new"), NewScope(), nil
	}); err != nil {
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
	select {
	case <-closed:
		close(finish)
		<-callDone
		t.Fatal("retired generation scope closed while its tool call was still running")
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	if err := <-callDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("retired scope was not closed after call completion")
	}
}

func TestReviewSnapshotCannotBeMutatedThroughLease(t *testing.T) {
	m := NewManager(snapshot("v1"), NewScope(), nil)
	lease, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	exposed := lease.Snapshot()
	exposed.ToolSet = "tampered"
	other, err := m.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if got := other.Snapshot().ToolSet; got != "tool/v1" {
		t.Fatalf("published snapshot was externally mutated: ToolSet=%q", got)
	}
	lease.Release()
}

func TestReviewRevokeCannotReturnLateSuccess(t *testing.T) {
	m := NewManager(snapshot("v1"), NewScope(), []string{"write"})
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
			<-finish // Models a side effect that does not cooperate with context cancellation.
			return nil
		})
	}()
	<-started
	m.Revoke("write")
	close(finish)
	if err := <-callDone; !errors.Is(err, ErrRevokedDuringCall) {
		t.Fatalf("revoked call returned a successful/committable result: %v", err)
	}
	lease.Release()
}

func TestReviewReloadBuildsAtMostOneCandidateAtATime(t *testing.T) {
	m := NewManager(snapshot("initial"), NewScope(), nil)
	started := make(chan struct{}, 2)
	finish := make(chan struct{})
	results := make(chan error, 2)
	build := func(id string) {
		results <- m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
			started <- struct{}{}
			<-finish
			return snapshot(id), NewScope(), nil
		})
	}
	go build("candidate-a")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first candidate did not start")
	}
	go build("candidate-b")
	concurrent := false
	select {
	case <-started:
		concurrent = true
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	successes := 0
	for i := 0; i < 2; i++ {
		if err := <-results; err == nil {
			successes++
		} else if !errors.Is(err, ErrStaleCandidate) {
			t.Fatal(err)
		}
	}
	if successes == 0 {
		t.Fatal("neither reload request published a candidate")
	}
	if concurrent {
		t.Fatal("concurrent reload built multiple candidate resource sets despite the two-generation cap")
	}
}

func TestReviewNoCandidateBuildWhileRetiringGenerationIsLeased(t *testing.T) {
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

func TestReviewScopeCloseSurvivesPanickingFinalizer(t *testing.T) {
	var mu sync.Mutex
	var order []int
	scope := NewScope()
	_ = scope.Add(func() error { mu.Lock(); order = append(order, 1); mu.Unlock(); return nil })
	_ = scope.Add(func() error { panic("broken cleanup") })
	_ = scope.Add(func() error { mu.Lock(); order = append(order, 3); mu.Unlock(); return nil })
	finished := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				finished <- errors.New("Scope.Close panicked instead of recording cleanup failure")
			}
		}()
		finished <- scope.Close()
	}()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("panicking finalizer did not produce a cleanup error")
		}
	case <-time.After(time.Second):
		t.Fatal("Scope.Close hung after a panicking finalizer")
	}
	mu.Lock()
	got := append([]int(nil), order...)
	mu.Unlock()
	if !reflect.DeepEqual(got, []int{3, 1}) {
		t.Fatalf("close continued order = %v, want [3 1]", got)
	}
}

func TestReviewScopeCloseFinalizerMayReenterClose(t *testing.T) {
	scope := NewScope()
	if err := scope.Add(func() error { return scope.Close() }); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- scope.Close() }()
	select {
	case err := <-finished:
		if !errors.Is(err, ErrScopeClosing) {
			t.Fatalf("outer Close error = %v, want reentrant ErrScopeClosing", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("reentrant finalizer deadlocked Scope.Close")
	}
	if err := scope.Wait(context.Background()); !errors.Is(err, ErrScopeClosing) {
		t.Fatalf("Wait result = %v, want recorded ErrScopeClosing", err)
	}
}

func TestReviewLimiterWaitIsCanceledAfterQueueing(t *testing.T) {
	limiter, err := NewLimiter(1)
	if err != nil {
		t.Fatal(err)
	}
	release, err := limiter.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, acquireErr := limiter.Acquire(ctx)
		finished <- acquireErr
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued acquire error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued acquire did not observe cancellation")
	}
	release()
}

func TestReviewCloseWaitsForOrCancelsCandidateBuilder(t *testing.T) {
	m := NewManager(snapshot("initial"), NewScope(), nil)
	started := make(chan struct{})
	finish := make(chan struct{})
	reloadDone := make(chan error, 1)
	go func() {
		reloadDone <- m.Reload(context.Background(), func(context.Context) (*Snapshot, *Scope, error) {
			close(started)
			<-finish
			return snapshot("late"), NewScope(), nil
		})
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := m.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		close(finish)
		<-reloadDone
		t.Fatalf("Close returned %v while an owned candidate build was still blocked", err)
	}
	close(finish)
	if err := <-reloadDone; !errors.Is(err, ErrManagerClosed) && !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight candidate publish after Close = %v, want manager-closed/canceled", err)
	}
}
```
<!-- reviewer-test-source:end -->
