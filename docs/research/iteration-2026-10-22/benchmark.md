# 轻量构建与真实CLI测量

开发完成后再统一执行以下步骤。本文件记录协议和命令；最终数字与构建来源另见[results.md](results.md) / [measurements.json](measurements.json)，未执行的场景不可作为结论。

`runtime.profile=edge` 控制运行期装配；它不会删除链接进binary的包。`headless` 去掉Bubble Tea/Lip Gloss/Glamour/Chroma终端绘制实现，仍运行同一REPL、AgentAssembly、Skills、MCP、会话树和审批。`no_plugin` 去掉Go共享库loader；用户自定义stdio/MCP/SDK进程扩展仍可用。headless不提供全屏绘制、终端主题、运行中键盘steer/modal；SIGINT取消、行式多轮输入、同步审批与最终响应保留。

```bash
# 不修改受保护Makefile；strip默认与portable产物使用相同选项。
go build -trimpath -buildvcs=false -ldflags='-s -w' -o /tmp/go-cli-full ./cmd/cli
CGO_ENABLED=0 go build -trimpath -buildvcs=false -tags headless,no_plugin -ldflags='-s -w' -o /tmp/go-cli-headless ./cmd/cli
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -tags headless,no_plugin -ldflags='-s -w' -o /tmp/go-cli-linux-amd64 ./cmd/cli
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -tags headless,no_plugin -ldflags='-s -w' -o /tmp/go-cli-linux-arm64 ./cmd/cli

go list -deps -tags headless,no_plugin ./cmd/cli > /tmp/go-cli-headless-imports.txt
# import graph中应无bubbletea/glamour/chroma/lipgloss及stdlib plugin。
rg 'charmbracelet/(bubbletea|glamour|lipgloss)|alecthomas/chroma|^plugin$' /tmp/go-cli-headless-imports.txt
```

Cross compile只能证明编译，边缘板实测必须记录具体CPU/RAM、Linux版本和实际运行结果。

`scripts/runtime-benchmark.py` 运行已有真实CLI产物，创建本机loopback OpenAI-compatible scripted provider；ready场景执行完整prompt装配及一次真实模型请求，tools场景执行read→edit→read→final并验证文件产物。它不是LLM质量评价，也不测真实网络首token。三个Go性能arm均为默认compat profile，extensions/git/tracing关闭；headless在此仅是build tags，不是edge profile性能实测。edge另有原始入口功能/预算integration证据，不作为第六性能arm，也不外推dynamic或Skill/MCP满载成本。每次新workspace，AB/BA交替，输出protocol.json、每次stdout/stderr、observations.jsonl与summary.json。失败/缺失/超时计入记录，warmup或measurement任一失败不发布该cohort headline时延/RSS。临时raw文件产生于task temp，最终清理见verification；持久化结果仅保存参数、provenance及数值，不保存workspace、模型response正文或二进制。默认5 warmup+30 measured；OS缓存未清空，不能称冷OS缓存。

```bash
python3 scripts/runtime-benchmark.py \
  --go-cli full=/tmp/go-cli-full \
  --go-cli headless=/tmp/go-cli-headless \
  --runs 30 --warmups 5 \
  --out /tmp/go-cli-runtime-measurements

# PI可选，使用固定SHA构建或固定官方1.3.0包；argv为JSON，不经shell。
python3 scripts/runtime-benchmark.py \
  --go-cli headless=/tmp/go-cli-headless \
  --pi-command '["/opt/homebrew/bin/node","/tmp/pinned-pi/dist/bundle/cli.js"]' \
  --out /tmp/go-cli-pi-measurements
```

脚本保留HOME，不修改个人配置；metadata preflight发现Go全局config、canonical用户Skills/MCP/hooks/auth/models/commands时拒绝运行，要求干净容器或账户；memory仅允许不存在或既有0字节常规文件，不读取个人内容。PI使用官方PI_CODING_AGENT_DIR隔离。OpenCode同时隔离XDG/config目录与官方 `OPENCODE_TEST_HOME`，因为其global `.agents` / `.claude` Skill兼容扫描独立于XDG。该变量见[固定tag源码](https://github.com/anomalyco/opencode/blob/843a6c514e606b436a7fe713422ace0a93ad3d61/packages/util/src/global.ts)；它不改宿主HOME。无个人配置的机器也应记录环境，检查生成provider requests里的tool集合，不能将不同功能配置成绩称完全对等。Go产物字节、PI bundle、Node解释器和安装目录总footprint分别记录；脚本分别记录 wait4 child RSS 与每 50 ms 采样的 PID/PPID 进程树 RSS，不混用口径。

默认UI测试继续完整运行；富终端专属测试以`!headless`选取。headless专属合同覆盖大输出不积累、source drain、取消join、durable refresh身份和ACK、关闭幂等。两种构建共用完整核心/会话/工具测试。

```bash
make verify
go test -race -tags headless,no_plugin ./...
# 相同history行数及流字节规模；历史body不完全同字节，两个benchmark呈现功能不同，不作精确等价UI速度倍率。
go test ./internal/tui -run '^$' -bench '^BenchmarkBubbleTeaTranscriptUpdateView$' -benchmem -benchtime=1x
go test -tags headless,no_plugin ./internal/cli -run '^$' -bench '^BenchmarkHeadlessTranscriptStream$' -benchmem -benchtime=1x
```

两个render benchmark均使用1k/5k/10k历史行与1000×1KiB流；full历史body为 `loaded canonical row N`，headless为 `history`。它们不是逐字节相同的历史内容，不把差异折算为精确UI等价速度倍率。

长会话full benchmark已有对每chunk不重新SetRows/sameRows扫描的断言；没有profile证据时本轮不做额外缓存改造。UI输入/取消/输出上限由现有默认TUI合同与headless合同分别验证。真实模型质量、lazy 0/100/1000能力规模、重载尾延迟按[上游研究实验协议](upstream.md)独立补测；脚本未覆盖的场景明确不宣称已测。

可选OpenCode v2使用官方 `@opencode/cli-darwin-arm64@2.0.32` 预编译包（对应release tag SHA `843a6c514e606b436a7fe713422ace0a93ad3d61`），追加 `--opencode-cli /absolute/path/bin/opencode`。脚本使用v2 `run --standalone --auto` 私有server模式，不连接或修改用户后台service。OpenCode默认工作流可复用后台服务；本实验只衡量每次新私有服务与新状态目录的任务，不能外推已有常驻服务的单轮延迟。它的私有服务进程结构与Go不同，Go也可能有短命helper；以 tree_peak_rss_bytes 记录采样树峰值，wait4 child RSS 保持单独标签，不直接据单进程值下总资源结论。不同默认工具集合由raw requests显式记录；`schema_bytes` 是 tools 数组经 Python json.dumps(ensure_ascii=False，默认分隔符) 重编码的 UTF-8 字节，不是原wire body字节或token数；此实验对齐固定任务语义，不能称相同全部schema集的纯框架微基准。

### RSS 的口径

脚本每 50 ms 通过 PID/PPID 快照采样该 CLI 及其后代的 RSS 总和，记录 `tree_peak_rss_bytes`、实际 sample 数、观察到的 PID 与采样错误；`peak_rss_bytes` 仍单独记录 wait4 子进程报告值。树 RSS 是采样峰值，会错过间隔内短峰，共享页也可能被相加多次；CPU 来自 wait4，不把 parent CPU 当整树 CPU。ps 采样引入额外开销，所有 arm 使用同一方法。entrypoint/安装 footprint/运行时依赖分别报告。

构建对照统一 `CGO_ENABLED=0`、`-trimpath -buildvcs=false -ldflags='-s -w'`。baseline 使用 git archive 的固定源码目录；current 工作树的所有 Go 源文件另存 SHA256 manifest。默认 Make 产物使用项目原 flags 单列，避免 VCS 元数据或 CGO 模式混入标签体积对比。pilot 先 `--runs 1 --warmups 0` 验证全部 arm 的真实任务；正式 cohort 再用 5 warmup / 30 measured。
