# 本轮证据核对源码附录

三份 `.txt` 文件是本轮任务内实际运行的证据核对脚本精确副本，SHA 与私有源码一致，见 manifest.json。它们不是产品可执行入口。复用需审阅后复制为 `.py`，放入相同的独立任务目录布局；不能在本附录目录直接当产品命令运行。

collector 按真实 UI 观察的 physical SID 选择 JSONL，并校验启动前 byte offset 的完整 prefix SHA 和行边界，仅解析 append suffix。阶段共享 PTY 时还核对 request sequence 与 input metadata event-prefix SHA。旧数据保持原样，不通过生成用户行、receipt 或 route 来补齐证据。

离线9例与真实旧数据的零 append 自检方法见上级 report.md；这两项是证据工具校验，不计为真实模型或产品能力验收。原始日志、输入和模型正文始终留在私有任务目录。
