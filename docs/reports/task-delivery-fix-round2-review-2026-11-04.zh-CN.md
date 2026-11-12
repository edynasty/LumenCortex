# Delivery Contract 第二轮审查与修复（中文摘要）

英文原文：[Delivery fix round 2 review](task-delivery-fix-round2-review-2026-11-04.md)

## 审查重点
复核绝对符号链接目标、工作区路径别名、读取期间祖先链接替换，以及 Absent + Changed + Commit 的文件系统语义。

## 主要原则
- 路径解析前不能随意 Clean，避免改变操作系统真实路径语义。
- 合法工作区别名不能误判越界。
- 读取期间路径身份发生变化时必须拒绝采信。
- ENOTDIR 等错误必须与真正的文件缺失区分。
- Race 发现的测试缺陷要单独记录。

## 边界
报告只覆盖其候选和列出的测试环境。
