# Dot develop 端到端验证计划

目标：使用确定性模拟 LLM，对真实 CLI 二进制、终端界面、工具副作用、长任务与持久化进行校验；复现问题后修复，并交叉审查和回归。

## 基线与隔离

- 仓库：`https://github.com/PengJunchen/go-cli.git`。
- 开发分支：`develop`，基线 `e77e8058c51e5370a2ecc3eacb5fce0419829bcf`。
- 原本地 checkout 为 `feat/phase38-review-v5-fix` / `4203619`，其远端缓存过期；通过 HTTPS 查询并取得真正 develop。
- 在临时副本工作，各场景使用独立配置、HOME、cwd、fixture 假密钥和 loopback HTTP server，禁止接触真实用户会话。
- 原有旧分支审批疑虑在最新 develop 中已修复，不能作为本分支缺陷记录。

## 分工与验收

| 负责人 | 范围 | 可执行验收 |
| --- | --- | --- |
| GPT 6 Luna Max / terminal | Python mock HTTP + binary PTY + xterm.js/Playwright | 启动、流式回答、审批拒绝与允许、工具运行与完成；真实终端模拟器截图 |
| GPT 6 Luna Max / tools | binary 工具与持久化回归 | read/write/edit/bash auto/deny，默认权限，错误工具结果与修正调用，独立进程会话恢复；核对工具消息与文件字节 |
| GPT 6 Luna Max / longtask | binary -race / PTY 长任务 | 120轮与压缩、follow-up 实际模型请求、Esc 取消及延迟副作用；无子进程 race 报告 |
| 主代理 | 规划、权限、整合、审查 | 固定基线，审查改动与证据，调度交叉审查，`make verify` 与 tagged E2E 回归，形成最终报告 |

各代理仅修改分配范围，不提交、不切分支。受保护 Makefile、CI、规则和 infra 规划文件不修改。

## 证据约定

- 模拟的仅为 LLM provider；文件、bash、CLI、PTY、会话与终端渲染均走实际实现。
- 不以最终模型回答含某个词代替真实工具执行；断言工具结果、调用 ID、文件字节与副作用。
- 截图来自实际 xterm.js 浏览器终端渲染真实 PTY 输出，标记为 headless browser terminal；当前 Mac 锁屏，截图不代表原生 Terminal 窗口。
- 记录失败前后测试、构建提交、退出码、请求摘要与限制。测试装置错误与产品缺陷分别记录。
- 全量检查的历史扫描警告保留；失败或未测项不能称为通过。

## 进度

1. 仓库和分支定位完成。
2. 三个子代理完成入口探索，开始真实 binary 验证。
3. 首次 `make test` 同时纳入新增工具测试：8704 个测试，8692 通过、6 失败、6 跳过。不是纯净基线结果。正在复现失败；`make vet` 和 `make scan` 通过，扫描 ERROR=0 / WARN=1802。
4. 工具测试的 read/deny 与默认 read 两个失败已归因为 fixture 断言及 trusted read scope 配置，修正测试装置后重跑。
5. 最终结果待所有独立验收及交叉审查完成后填写报告。
