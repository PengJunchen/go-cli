# 文档入口

- [迭代报告与架构图](iterations/README.md)：历史迭代记录、验收报告和关联材料。
- [动态能力运行时迭代总结](iterations/2026-10-22-dynamic-runtime/README.md)：设计、验证与使用说明。
- [运行时构建、profile 与扩展边界](runtime-guide.md)：四种构建组合、edge profile、扩展重叠、当前限制与固定版本参考项目。
- [公开扩展 worker API](../pkg/extensionapi/v1/README.md)：独立 JSONL worker 的协议、能力范围和隔离边界。

这些文件使用仓库内的相对路径互链。内容提交后保存在 Git 仓库和提交历史中，不依赖某个 worktree 的绝对路径；删除临时 worktree 不会删除已提交的文档或素材。
