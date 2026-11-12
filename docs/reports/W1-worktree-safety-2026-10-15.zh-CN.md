# W1：Session Worktree 安全（中文摘要）

英文原文：[W1 worktree safety](W1-worktree-safety-2026-10-15.md)

## 目标
避免 Session 删除、应用或清理时误操作其他 Session 正在使用的 Git Worktree。

## 主要修复方向
- 明确 Worktree Owner 和 Borrower。
- 对共享资源使用 Lease。
- 执行删除前锁定 Session 和相关后代。
- 重新检查磁盘状态、忽略文件和未提交内容。
- 无法确认归属时拒绝破坏性操作。
- 部分失败必须有可恢复状态和记录。

## 实现原理
将 Worktree 所有权、Session 数据库状态和磁盘检查结合起来；不能只根据数据库中的路径就直接删除目录。
