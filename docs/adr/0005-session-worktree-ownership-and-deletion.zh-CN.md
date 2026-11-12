# ADR-0005：Session Worktree 的所有权、租约和删除

## 背景
多个 Session 可能访问同一 Git Worktree。若不区分所有者和借用者，清理操作可能误删其他任务正在使用的文件。

## 决策
- 区分 Owner、Borrower 和无法确认归属的 Uncertain 状态。
- Worktree 使用明确 Lease；Run 操作使用 Session/Run 锁。
- 删除前重新检查所有权、租约、后代 Session 和磁盘状态。
- 任一检查冲突就拒绝操作，不做部分写入。

## 删除流程
1. 锁定相关 Session 和后代。
2. 获取独占 Worktree Lease。
3. 重新检查磁盘状态和删除条件。
4. 执行磁盘清理。
5. 在一个数据库事务中删除 Session 或 Worktree 关联。
6. 写 Journal、清理锁文件并释放锁。

## 实现原理
先确认所有权，再执行破坏性操作。数据库和磁盘状态必须一起核对；无法证明安全时拒绝删除。子 Session、部分失败和 Worktree 丢失有单独规则，见英文原文。
