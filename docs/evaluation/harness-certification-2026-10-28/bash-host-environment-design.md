# Bash受信宿主环境契约：最小修复设计

2026-10-29，Designer；基线`e1f5291f254d6e2a50ffa4598ea443dd822381c0`。仅设计，生产尚未改动；Go-only，待Reviewer准入后由Builder在隔离工作树TDD。

**问题已复现**：同镜像0模型诊断中，基础环境能找到go/gofmt，Go工具找不到；宿主GOPATH存在，工具丢失后模块目录变到另一个默认位置。已有模块目录存在31个直接条目，不证明依赖齐全；两边GOCACHE均指向一个不存在的默认目录，不能称构建缓存命中。诊断安全摘要SHA：`22a49410da127094e9c87ad052854344f0f0f06a520fa07218ed409026e595e9`；不回填原Flipt成绩。

## 推荐的最小实现

1. [普通Bash](../../../internal/tools/bash.go)与[流式Bash](../../../internal/tools/streaming_bash.go)的目标命令统一为非登录、非交互`bash -c`，使启动者提供的PATH不再被登录初始化改写；不增加模型参数或自由shell配置。
2. **必须同步受管gate**：[process_scope_unix.go](../../../internal/tools/process_scope_unix.go)当前既只接受exact `-lc`，又在gate脚本末尾硬编码`-lc`。改为只接受exact `-c`并以`-c`执行目标，不能仅改两个调用点或放宽为任意flags。空环境bootstrap、可信系统Bash验证、身份持久化后放行、私管道关闭、NUL分隔无eval恢复及现有env校验均保留。
3. `filteredEnv`保持现有白名单，只新增固定的`GOPATH`、`GOCACHE`、`GOMODCACHE`三个标准开发路径键；值取启动者实际环境，无仓库/任务/镜像路径特判，不注入默认值，保留unset/set-empty/set-value区别。首修不添加XDG等无当前需求的键，不默认继承GOPROXY、GOFLAGS、BASH_ENV、ENV、LD_PRELOAD或其它未列键，也不承诺解决DNS/缺失依赖。
4. 现有受信库API `WithEnv`的覆盖及大小写不敏感敏感后缀过滤保持；不接入任意CLI/project配置env通道。现有明确`WithEnv(BASH_ENV/ENV/PS4/选项)`在gate之后生效的合同不能被误删，默认环境继承与调用方显式授予是两件事。

注册默认使用StreamingBash，当前assembly只有Bash执行窗口配置，无自由Env接线；上述修复无需新增配置API。超时/上层取消只收紧、sandbox/审批/effect、reader/group join、proof/typed resolution与Unknown策略不变。兼容变化须说明：普通工具命令不再自动执行登录初始化；需要初始化的操作者应在受信宿主准备环境，显式脚本仍受原有审批/沙箱规则约束。

## 可执行验收与红→绿

| AC | 最小验证，不造新框架 |
| --- | --- |
| 两入口真实非登录 | 普通/流式工具各实际执行`shopt -q login_shell`的条件分支，旧目标必为login、新目标为nonlogin；不依赖宿主profile是否碰巧改PATH，也不只断言argv。 |
| PATH及缓存交付 | 临时自有bin提供带nonce的合成命令，由调用者PATH寻址；三个缓存键用pure filter表覆盖unset/空/非空，再各入口实际观察路径值。`WithEnv`覆盖父值仍有效，不复制默认目录。 |
| 过滤负例 | 复用既有敏感key/大小写及extra过滤测试，补有限新增键的精确继承；未列普通变量和上述ambient启动/网络变量仍不继承，不输出真实secret。 |
| gate安全不退化 | 复用`process_lifecycle_test.go`的身份观察阻塞/失败、受信Bash、BASH_ENV/PS4不得在身份前执行、原样环境恢复、argv0/管道关闭及选项恢复测试；直接gate fixture的目标模式随新契约改成`-c`，不删断言/不加skip。 |
| 收尾与时限 | 复用普通/流式取消、后代持pipe、超时、prepared completion与runtime tightening合同；错误/Unknown原样，不凭receipt或完成资源清理改成业务成功。 |
| 实际CLI | 本地脚本peer经真实CLI→Bash→下一请求→final；实际广告schema仍仅command，逐action proof/resolution/pending0。source/binary/config/env policy另冻。 |
| Linux0模型复验 | 新OUT、同固定镜像/no-init/300秒配置及原诊断命令，对照原基础环境与旧Go记录，观察go/gofmt可寻址及GOPATH/模块目录与宿主一致；显式GOCACHE/GOMODCACHE另用任意自有路径合同验证。cache覆盖仍unknown，rg缺失不冒称修复。保留原件和安全标量，不改镜像/题目/网络/旧记录。 |
| 发布门 | 先目标合同race，再实际Linux无费复验及Reviewer，最后在协调资源窗执行`make verify`；记录source SHA/mode前后、真实执行/skip/未测范围。故障先保留工件分类，不盲重跑。 |

既有可复用位置：[环境过滤](../../../internal/tools/bash_test.go)、[普通/流式受管生命周期](../../../internal/tools/process_lifecycle_test.go)、[流式完成与取消](../../../internal/tools/streaming_bash_test.go)、[上层时限/放行guard](../../../internal/tools/bash_runtime_input_test.go)。Builder登记新增目标函数和真实执行命令后再标PASS；本设计不声称修复或验证已经完成。
