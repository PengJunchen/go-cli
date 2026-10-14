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
2. 三个实现子代理完成真实 binary 验证；另由独立 Standards / Spec 子代理交叉审查，主代理检查源码、原始记录和截图。
3. 初次混合新增测试的 `make test` 为 8704 total / 8692 pass / 6 fail / 6 skip，不是纯净基线。随后在纯净 e77 基线测得 8695 total / 8686 pass / 3 fail / 6 skip，失败为 ACP、Grep 与 gate 测试夹具。
4. 修复 follow-up / steering 空格产品缺陷；补真实工具结果、文件副作用、独立进程恢复、120 轮压缩、运行中补充指令与 Esc 取消证据。修正测试装置同步和原有夹具，未扩大生产工具功能。
5. 初始基线加 patch 的 `make verify` 和 `make test-e2e` 均通过；整合时保留已到达的上游 `1cd1951` 与 `4d1497b` 两个提交。
6. 最新整合树的完整 tagged 运行又发现 Esc 正常取消误报生命周期错误，保存失败证据后修复，并增加取消后新输入回归。独立审查拒绝额外两秒兜底设计，最终保持任务和事件桥 join 契约。
7. 最终门禁、截图和审查结果统一记录在 [完整报告](README.md) 及 `gates/*.json`，不以旧基线或定向通过替代最新源码全量验收。
8. 最终源码 `6aa6f93` 串行通过 `make verify`（8707 pass / 0 fail / 6 skip）和 `make test-e2e`（4781 pass / 0 fail / 6 skip）；源码运行前后均无差异。截图 36/36 检查、12 PNG、3 raw PTY，Linux/amd64 测试包交叉编译通过，两轴审查完成。
