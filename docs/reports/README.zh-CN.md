# 实施与审查报告目录

本目录保存各阶段实施、修复、审查和验收记录。中文文件是对应英文报告的中文摘要，方便快速了解目标、主要实现、结论和限制；**完整细节、原始日志、测试表格和逐项证据以英文原文为准**。

报告记录的是特定日期和提交的历史事实，不能代替当前组件文档，也不能直接证明当前 main 仍保持当时状态。

## Harness、执行平台和调试

- [Agent Debugger 详细设计](agent-debugger-design-2026-11-08.zh-CN.md) · [原文](agent-debugger-design-2026-11-08.md)
- [执行平台实施报告](cognitive-agent-execution-platform-implementation-2026-11-08.zh-CN.md) · [原文](cognitive-agent-execution-platform-implementation-2026-11-08.md)
- [Harness Evolution：DAP](harness-evolution-dap-20261110.zh-CN.md) · [原文](harness-evolution-dap-20261110.md)
- [Harness Evolution：Editing / Hashline / AST](harness-evolution-editing-20261110.zh-CN.md) · [原文](harness-evolution-editing-20261110.md)
- [Harness Evolution：Persistent Eval](harness-evolution-eval-20261110.zh-CN.md) · [原文](harness-evolution-eval-20261110.md)
- [Harness Evolution：Agent Hub](harness-evolution-hub-20261110.zh-CN.md) · [原文](harness-evolution-hub-20261110.md)
- [Agent Presets](agent-presets-2026-10-22.zh-CN.md) · [原文](agent-presets-2026-10-22.md)
- [Agent Presets 验证](agent-presets-validation-2026-10-22.zh-CN.md) · [原文](agent-presets-validation-2026-10-22.md)
- [Agent Catalog](agent-catalog-2026-10-20.zh-CN.md) · [原文](agent-catalog-2026-10-20.md)
- [Agent 并发配置](agent-concurrency-2026-10-20.zh-CN.md) · [原文](agent-concurrency-2026-10-20.md)
- [LSP 生命周期加固](lsp-lifecycle-2026-11-01.zh-CN.md) · [原文](lsp-lifecycle-2026-11-01.md)
- [Notebook 实施](notebook-slice-20261101.zh-CN.md) · [原文](notebook-slice-20261101.md)

## Cognitive、Context 和知识连续性

- [Context 连续性](context-continuity-2026-10-21.zh-CN.md) · [原文](context-continuity-2026-10-21.md)
- [M3/M4 Runtime](M3-M4-runtime-2026-10-15.zh-CN.md) · [原文](M3-M4-runtime-2026-10-15.md)
- [自动推理强度](auto-effort-2026-10-19.zh-CN.md) · [原文](auto-effort-2026-10-19.md)

## Provider、Plugin 和安全

- [M2.3 模型 Runtime 切换](M2.3-model-runtime-cutover.zh-CN.md) · [原文](M2.3-model-runtime-cutover.md)
- [M2.3 独立审计](M2.3-independent-audit.zh-CN.md) · [原文](M2.3-independent-audit.md)
- [Provider 协议](ux-provider-completion-protocol-2026-10-19.zh-CN.md) · [原文](ux-provider-completion-protocol-2026-10-19.md)
- [Provider 凭据](ux-provider-completion-credentials-2026-10-19.zh-CN.md) · [原文](ux-provider-completion-credentials-2026-10-19.md)
- [Provider 重试恢复](provider-retry-recovery-2026-11-03.zh-CN.md) · [原文](provider-retry-recovery-2026-11-03.md)
- [MCP 信任和工具权限](mcp-trust-and-tool-permissions-2026-10-22.zh-CN.md) · [原文](mcp-trust-and-tool-permissions-2026-10-22.md)
- [Plugin 平台](plugin-platform-2026-10-22.zh-CN.md) · [原文](plugin-platform-2026-10-22.md)
- [Capability Registry 和 MCP 生命周期](plugin-capability-registry-2026-11-01.zh-CN.md) · [原文](plugin-capability-registry-2026-11-01.md)

## 任务交付和执行恢复

- [Task Delivery 实施](task-delivery-2026-11-03.zh-CN.md) · [原文](task-delivery-2026-11-03.md)
- [Task Delivery 审查](task-delivery-review-2026-11-04.zh-CN.md) · [原文](task-delivery-review-2026-11-04.md)
- [Task Delivery 最终验收](task-delivery-final-acceptance-2026-11-04.zh-CN.md) · [原文](task-delivery-final-acceptance-2026-11-04.md)
- [Task Delivery 第二轮修复](task-delivery-fix-round2-2026-11-04.zh-CN.md) · [原文](task-delivery-fix-round2-2026-11-04.md)
- [Task Delivery 第二轮复审](task-delivery-fix-round2-review-2026-11-04.zh-CN.md) · [原文](task-delivery-fix-round2-review-2026-11-04.md)
- [Task Delivery 独立复核](task-delivery-independent-review-2026-11-04.zh-CN.md) · [原文](task-delivery-independent-review-2026-11-04.md)
- [执行重试审查](execution-retry-review-2026-11-03.zh-CN.md) · [原文](execution-retry-review-2026-11-03.md)
- [BENCH003 Shell 超时修复](bench003-shell-timeout-2026-11-02.zh-CN.md) · [原文](bench003-shell-timeout-2026-11-02.md)
- [Runtime 定时任务](runtime-004-schedules-2026-11-01.zh-CN.md) · [原文](runtime-004-schedules-2026-11-01.md)

## Runtime 整合、迁移和恢复

- [Core 全量整合](core-full-integration-2026-11-04.zh-CN.md) · [原文](core-full-integration-2026-11-04.md)
- [Runtime 升级验收](runtime-merge-acceptance-2026-11-01.zh-CN.md) · [原文](runtime-merge-acceptance-2026-11-01.md)
- [Go-only 基线更新](readme-go-only-baseline-refresh-20261111.zh-CN.md) · [原文](readme-go-only-baseline-refresh-20261111.md)
- [README 品牌和图片修复](readme-brand-and-image-repair-20261111.zh-CN.md) · [原文](readme-brand-and-image-repair-20261111.md)
- [PR #2 LSP 退役](pr-2-lsp-refactor-retirement-20261112.zh-CN.md) · [原文](pr-2-lsp-refactor-retirement-20261112.md)
- [10 月 16 日恢复审计](oct16-recovery-2026-10-17.zh-CN.md) · [原文](oct16-recovery-2026-10-17.md)
- [Phase 0](phase0-2026-10-22.zh-CN.md) · [原文](phase0-2026-10-22.md)
- [Phase 0 工作区信任](phase0-trust-2026-10-22.zh-CN.md) · [原文](phase0-trust-2026-10-22.md)
- [OMP Session 导入](omp-session-import-2026-10-22.zh-CN.md) · [原文](omp-session-import-2026-10-22.md)
- [C1 交互式执行](C1-interactive-2026-10-18.zh-CN.md) · [原文](C1-interactive-2026-10-18.md)
- [C2 Execution Tasks](C2-execution-tasks-2026-10-18.zh-CN.md) · [原文](C2-execution-tasks-2026-10-18.md)
- [W1 Worktree 安全](W1-worktree-safety-2026-10-15.zh-CN.md) · [原文](W1-worktree-safety-2026-10-15.md)

## 其他

所有报告均保留英文原文。中文摘要用于导航和理解，不覆盖原始测试记录、完整 Finding、提交哈希或证据文件。
