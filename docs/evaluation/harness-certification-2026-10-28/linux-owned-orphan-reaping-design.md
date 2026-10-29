# Linux outer-CLI reaper：首版设计与验收门

状态：2026-10-29，设计已准入、候选已实现。Linux ARM64真实CLI四臂已有独立证据，真实CLI取消补臂仍缺；amd64/Rosetta没有通过opt-in，全组合fullverify待完成，见[第3轮](round-03.md)。**采用隔离 outer-CLI reaper，不采用旧稿的业务 CLI 全局 scoped reaper，也不做每命令 supervisor/RPC。** D 的生产内容仍为5aa063e，历史结果不改；R1b 独立。

## 修复目标和最小接口

[第2轮](round-02.md)的合成实验已证实：no-init 中孤儿僵尸可使真实进程组持续存在。回收僵尸应让内层原有 owner 完成关停；后台工作被终止仍须 OutcomeUnknown/error，不变成业务成功。

首版只提供 standalone Linux main 启动开关 `GO_CLI_LINUX_SUBREAPER=1`（0/未设置为关闭，其他值拒绝）。显式启用时，fd0/1/2 **任一为 TTY 都在模型/业务前拒绝**。Darwin/其它平台默认不变，显式启用报不支持；嵌入式 `cli.Run` 和工具库不自动读取此开关、不修改宿主 subreaper 状态。不开启时走原 main 路径。

生产改动优先限制为 cmd/cli 的启动分派、Linux outer/内部引导及平台 stub；内层 `internal/tools/process_scope*`、core completion/close timeout/Unknown 规则不动。第一版不支持 TTY job control、任意额外FD、per-tool RPC、默认开启。

## 为什么来源可以闭合

outer 在 main 的最前面完成独立分派，不创建 trust/config/model/plugin/assembly，也不运行正常 CLI。它唯一一次主动 spawn 是同一受信任 executable 的实际 CLI（inner），设置新的 PGID。Linux selfexec绑定当前executable inode（例如 `/proc/self/exe`），不先解析一个可被替换的磁盘路径再启动别的文件。outer不再产生其它业务exec。这不是“Go进程整个生命周期只有一次fork”的承诺：Go标准库os.StartProcess可能同步进行并join短暂clone/能力探针。实现必须在Start返回、私有release前再次确认sole actualCLI child，reaper到此之后才启动，不消费标准库正在Wait的探针。

启用前读取 `/proc/self/task/*/children`，检查**所有线程**的直属 child，且保持整个检查/设置/启动阶段无其它 spawn。exec 会保留已有 child，故只要发现预存 child（包含 zombie）或不能完整读取，就拒绝、不 wait、不 spawn、不请求模型。拒绝不能靠消费这个 child 来“清空环境”。

Linux 设置并读回 `PR_SET_CHILD_SUBREAPER`，只在这个专用 outer 中生效。inner 不设置它。inner 还活着时，inner 的普通 exec child 仍以 inner 为 PPID，outer 不能也不会消费其 wait；发生 orphan/reparent 后，PPID=outer 的其它 child 来自唯一 inner 子树。唯一 actualCLI 始终受保护，由其原 `exec.Cmd.Wait` 独占；outer 只精确 wait 已死的 adopted child。这不再靠工具 PGID、历史成员或业务插件 registry 推断 adopted 来源。

## inner 引导和 FD 契约

建议使用一对私有 Unix socketpair（仅一个 ExtraFile，child端为fd3）和一次性 nonce/内部 role 环境标记。inner 仍收到原公开 argv、cwd、stdio 和用户环境；只增加私有引导项。actualCLI的信号能力由 `SysProcAttr.PidFD` 在fork/clone时原子返回，不能在Start后用数值PID重新PidfdOpen构造替代目标；outer持原fd，标准库os.Process持独立duplicate供唯一Cmd.Wait。父取得原子fd后再完成一次引导握手，避免PID复用窗口。

inner 引导须验证 socket类型；只有 inner 端可以利用 fork 前 socketpair 的 SO_PEERCRED 核对创建者 outer 与真实 getppid 对应，父 PID/start token 和预期同 executable 身份相符。**outer 端的 SO_PEERCRED 也可能是创建者 outer，不能冒称它证明实际发送者是 inner。** 本最小协议在 outer 端依赖唯一主动 child、只向它继承的私有FD及nonce作角色capability；若实现确需发送者PID认证，必须使用SO_PASSCRED/SCM_CREDENTIALS，不用错误的peerPID推断。父仅在 subreaper/唯一child前提成立后发正确 nonce。没有真实私有通道的 role 标记、错误 nonce、错误 peer/父身份、EOF、重复/畸形引导均拒绝，不能用一个可伪造 env 字符串直接跳过 opt-in。这个通道用于正确分派和防递归，不是对同权限恶意用户的沙箱或签名认证。

验证后先复制必要数据到局部变量，unset所有私有 role/nonce/FD 环境键、关闭fd3，再进入正常 CLI。正常工具/子Agent/MCP exec 不能继承这些私有项或socket；outer其余自建FD均CLOEXEC。首版除0/1/2外不支持用户传入额外FD，发现未声明的继承FD应在spawn前拒绝，不能静默改变其含义。Go runtime自身已知CLOEXEC FD不算用户extraFD；检查方案需按实际 /proc descriptor状态审查。

## 回收、信号、关停

- actualCLI 唯一 `cmd.Wait` 可在专用 goroutine 执行；其PID/start token及原子返回的pidfd从启动成功到Wait join都受保护。保护的是该identity，Wait完成后不能把整数PID永久列入黑名单而误跳过一个复用该PID的新adopted identity。outer从不wait它的状态。每次扫描 PPID=outer 且非actualCLI的 Z，打开pidfd，前后核对PID/starttime/PPID/state，再用 `waitid(P_PIDFD, WEXITED|WNOHANG)` 精确回收。身份变化/ESRCH/ECHILD不是成功回收证据；可重观测实际absence，不退回数字PID或全局wait。
- 不安装全局 SIGCHLD wait consumer，不调用 wait4(-1)。不处理 inner 仍有父子关系的 child、不按整数 PGID 猜所属工具。任何 waitid 返回0都不能单独当成已回收，仍确认绑定身份消失。
- 非TTY下 outer 通过 signal.Notify 收到 SIGINT/TERM后，**只通过 actualCLI pidfd转发一次同一信号**；不再向actualCLI PGID发送一遍。inner保留原 signal.NotifyContext取消和工具所有权收尾。actualCLI已Wait结束时不向复用PID发信号。多次真实信号可分别转发，不扩展成“二次信号强杀全部group”的新协议。
- inner启动使用 Linux Pdeathsig=SIGTERM，父的spawn线程以 runtime.LockOSThread 保持存活到inner退出，避免Go创建线程提前结束误触发。inner验证父身份和握手时也检查父存活，关闭Pdeathsig设置前父已死的竞态。SIGTERM在inner装配前的默认终止也是非成功，不能转作干净交付。
- outer在inner活着时只reap已死孤儿，**不杀存活的adopted child**；内层仍负责原本的TERM/KILL、groupESRCH、reader及observer join。
- inner结束后，outer继续精确drain。若发现存活adopted child，outer进入明确异常收尾：对已绑定pidfd的本子树孤儿发TERM/KILL并有界join，不按group广播；必须报supervisor异常，不能把inner的0静默当成功。此故障分支沿用现有100ms TERM/300ms KILL量级，不延长内层close时限。不可观测/无法回收则非零并披露残留，不伪造proof。
- 干净outer收尾保留inner原退出码；inner信号退出按明确128+signal映射，parent自身异常不得返回成功，不能把inner非零改为0。转发后outer继续陪伴inner完成原收尾，不因收到信号立即退出丢失reaper。
- outer被SIGKILL/宿主崩溃时，不可能由已死父提供回收保证。Pdeathsig让inner走取消；若两者皆被杀或后代不能join，只能报告异常并由原容器/任务所有者兜底清理，不能声称该场景已有完整资源证明。首版不实现cgroup或进程树沙箱。

Linux需要实际可用的subreaper、/proc、pidfd_open和waitid(P_PIDFD)，所需 syscall 通常至少Linux5.4，且seccomp允许。显式启用而不可用必须在inner正常CLI/模型启动前拒绝，不静默降级。实现应先做受信任无业务的能力预检；其中不得再主动spawn测试child破坏“outer只spawnactualCLI一次”的不变量，必要的能力确认在actualCLI仍停在私有bootstrap barrier时以其pidfd调用 `waitid(P_PIDFD, WEXITED|WNOWAIT|WNOHANG)` 做不消费的检查；绝不消耗actualCLI退出状态，也不在inner已开始业务后才发现不支持。

## Bash契约澄清，独立于reaper修复

已核 `internal/tools/bash.go:Description` 仅说运行命令；`owned_worker_tool.go` 的名字来自动态SDK/RPC工具definition，没有默认通用 worker_start/worker_wait。`worker_echo`只是测试插件，当前正式extensions关闭时不可用。

当前 BashTool.Parameters 仅有 `command:string`，additionalProperties=false；Streaming继承同schema，没有模型可传入的timeout参数。WithTimeout默认30s、可选tier5/30/300s属于host配置；WithBashRuntimeExecutionTimeout是host-only且仅收紧。不能为文案虚构参数、单位或上限。

建议文案：命令是同步、受管执行；不要以未等待的 `&` 留下后台任务然后退出，较长同步任务需由宿主配置适当执行时限；需要命令内并行时自行wait；只有当前工具列表实际提供受管worker时才使用它。保留合法 `cmd & wait`、重定向和shell语义，不用正则禁止所有 `&`，不自动移除后台标记或伪造async成功。纯描述澄清不写复刻文案的测试；实际语义靠下面执行合同。

## TDD验收与移交顺序

所有 subreaper 实验在独立Linux子进程/容器，不修改go test宿主全局属性。以下名称是拟实现的合同，不声称测试已存在或已通过。

| 门 | 可执行验收结果 |
|---|---|
| AC1 先红 | 旧D同no-init/image/security、合成后台重定向sleep10：真实孤儿Z和group存续、原shutdown pending；保存新目录，零付费模型。 |
| AC2 修复绿 | opt-in outer同no-init：实际/proc记录PPID从inner转outer、Z→absent；内层原group最终ESRCH，leader/readers/observer/assembly结束、无shutdown pending。后台仍OutcomeUnknown/error/CLI非零。不能用外部删容器作为absence。 |
| AC3 不偷Wait | inner同时启动两个普通exec，其原cmd.Wait各取得精确退出码；actualCLI自身非零码同样保留。另adopted多层fork僵尸由outer回收。无需推断每工具PGID。 |
| AC4 启动拒绝 | 通过exec继承一个尚活或Z的preexistingchild：outer拒绝、不wait它/不spawninner；测试原owner仍能取得其退出状态。/proc读取失败、seccomp拒绝PRCTL/pidfd/waitid、任何TTY均beforemodel拒绝。 |
| AC5 引导隔离 | 缺fd/普通file伪fd/错误nonce/错误peer/重复role都拒绝；inner只启动一次；真实工具读取env和FD清单确认私有项全部消失，cwd/普通env/stdin/out/err字节契约保持。 |
| AC6 信号 | SIGINT与TERM的正常外部转发各独立场景，inner实际仅收到一份转发、原cancel/后代join守卫保留；actualCLI已退出不再向复用PID发信号。pidfd/身份变化注入合同确保无数字PID fallback；不把注入称真实PID wrap。 |
| AC7 异常父/子 | 在inner被本地peer/工具barrier阻塞的受控场景kill outer触发Pdeathsig，至少一次可观察取消信号到达（不要求parent-death总计恰好一次），观察取消后inner不得新发模型请求或输出干净成功（不把信号送达前的调度竞态伪称零窗口）；kill inner则outer有界清理adopted孩子并非零，不抢actualCLI Wait。记录残留与容器兜底边界，不把SIGKILL当可保证无残留。 |
| AC8 成功/语义负例 | 前台sleep10与合法cmd&wait为success；未等待后台被杀仍Unknown；forced-reader-close/escaped-writer原未知负例继续拒绝proof。不修改core、tool timeout或close预算。 |
| AC9 默认与全链 | opt-in关闭/Linux、Darwin默认和嵌入库不产生outer或PRCTL；新本地HTTP/SSE真实CLI矩阵包含durable proof/resolution/trace关闭核查。全部零付费。通过后独立源审、全make verify，再冻结新二进制/profile。 |

Builder先做bootstrap/preexistingchild/Wait归属/exitcode小合同红→绿，再做真实Linux no-init矩阵；Reviewer检查来源闭合及所有负例。新变体另名，容器仍no-init；不覆盖D，不把合成诊断计入标准题成功率，不在此阶段重跑付费模型或解封holdout。R1b测试按独立资源窗继续，与本修复分版本。

## 一手依据

- [Linux subreaper手册](https://man7.org/linux/man-pages/man2/PR_SET_CHILD_SUBREAPER.2const.html)：孤儿交给最近存活subreaper；属性fork不继承、exec保留。这同时解释为何outer启动要排除exec继承的旧child。
- [Linux wait手册](https://man7.org/linux/man-pages/man2/waitpid.2.html)：只能wait当前child；P_PIDFD与WNOHANG行为。
- [Linux pidfd手册](https://man7.org/linux/man-pages/man2/pidfd_open.2.html)：绑定特定进程而非复用整数PID；此处不据此推断工具group世代。
- [Linux parent-death signal手册](https://man7.org/linux/man-pages/man2/PR_SET_PDEATHSIG.2const.html)：创建者线程死亡及设置前竞态必须处理；不能把Pdeathsig描述成整个子树强保证。
- 已有 `golang.org/x/sys v0.38.0` 提供所需Prctl/PidfdOpen/Waitid，设计不新增第三方依赖。
