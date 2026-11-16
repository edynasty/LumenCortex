# Cognitive Core v1.0 — Git integration and protected cleanup

## Baseline / 基线

| Role / 角色 | Commit / 分支 |
|---|---|
| Protected main / 受保护主线 | `00344b5d6a67673fea9fcbc90e0cab16de17dc57` |
| Sole implementation input / 唯一实现输入 | `8c30f75e063db69a1dbc867741dc8f042c7d0dbc` |
| Input branch / 输入分支 | `review/cognitive-core-icra-final-verify-20261115` |
| Isolated integration / 独立整合 | `integ/cognitive-core-v1-20261116`, `wt/core-cognitive-v1-integration-20261116` |
| Final commit identity / 最终提交身份 | The commit containing this report is the delivery commit; its exact SHA and parent are recorded in workspace `bench/evidence/cognitive-core-v1-integration-20261116/FINAL_DELIVERY.json` after commit. A commit cannot contain its own SHA. |

The start snapshot is `BASELINE.json`; the complete 33-commit ancestry and all protected cognitive worktrees are in `GIT_RELATIONSHIPS.json`. Core main had only its pre-existing `M AGENTS.md`. The older finalization baseline also had an untracked lifecycle test, and the old main-delivery worktree contained a large staged/untracked candidate. None was used as an integration source, changed, staged or discarded. 原有修改属于用户/其他任务，必须保留。

## Included lineage / 已包含的提交链

| Layer / 层 | Final code / 代码 | Report / 报告 | Integration fact / 整合事实 |
|---|---|---|---|
| Astra completion | `949b2c6533dcf66f1401d0b72c5d2c44cb560354` | `c1d322938942c9bb4a82f184d1624103b92a5bf4` | Ancestors of selected input / 输入祖先 |
| Fable FFR-001–017 | `c6bdfaf066f9c9fab7be824523471fa1b0bdcf92` | `f2dfa2861c177dcb34331490d4bd427741c375ad` | All fix commits retained / 保留全部修复提交 |
| Carbon ICRA-001–004 | `f8a87f5648221d546a77008bfd91f1387369efce` | Workspace ICRA ledger | Linear successor to Fable / 线性继承 |
| Opus ICRA-V-001 | `701a0325e6a3cfcce3e29f87473313ec3d577ffe` | Workspace final-verification ledger | Run sequence ordering / Run 序号权威 |
| Opus ICRA-V-002 | `a3178728639d9047cb5dcd50ebf664d3be40884a` | Same ledger | Uses-only premise identity / 仅 Uses 变化不误失效 |
| Opus ICRA-V-003 | `35af88d59f703768a32ceb1eb03a2cb03f78879b` | Same ledger | Canonical-chain guards / 规范节点防链 |
| Opus ICRA-V-T1 | `8c30f75e063db69a1dbc867741dc8f042c7d0dbc` | Same ledger | Dependent-session fence assertion / 依赖方不被连带 Fence |

Older P0, `e2c69c6`, `61a1d31` and `91af861` work remains preserved as historical evidence. The completion import already incorporated the selected implementations; blindly cherry-picking those parallel historical branch tips would duplicate or regress work. Current findings and source checks, not a branch name alone, establish inclusion. No new merge, rebase, squash or cherry-pick occurs in this integration.

## Scope of the delivery commit / 最终提交范围

The integration keeps all 893 Go/module/SQL source files byte-identical to the selected input. Their sorted path/hash map has SHA-256 `ff95fd3bcb8585810f121c73dd1be1450d29ad2fb35b4e53c5ef16e56964926a` (`SOURCE.json`). No changed candidate Go file required gofmt; unrelated formatting deviations remain untouched. The final commit contains current bilingual documentation, finding reconciliation, migration semantics and delivery reports only. It does not change module/runtime versions, a Desktop pin, SQL, test assertions or `internal/window`.

This is **Cognitive Core v1.0 capability baseline**, not a Core SemVer release or a product tag. Tests exercise the exact inherited implementation while documentation identifies its limits. The selective commit tool records only this task's authored changes and reports any remaining worktree changes; it does not stage main's `AGENTS.md`.

## Owner-gated merge plan / 待 Owner 授权合并方案

No step in this section was executed by the integration task.

1. Obtain the Owner's final merge/publication authorization. Recheck current main/remote identity then, because it may have changed since the captured baseline. Preserve the main worktree's `AGENTS.md`; do not reset, stash, clean or force-switch that worktree.
2. If main remains `00344b5` and the finalized integration is its descendant, publish the exact frozen integration commit, then fast-forward main through a protected PR or equivalent owner-approved update. Retain the full 33-commit repair lineage and the delivery commit; do not squash or rewrite historical evidence.
3. If main advanced incompatibly, stop the fast-forward path. Use a new integration worktree, inspect only the new delta, resolve actual conflicts and run affected checks; never silently declare the old tests valid for changed code.
4. Verify the remote main SHA equals the intended tested integration commit (or the explicitly tested new merge commit). A local commit alone is not publication.
5. Any tag/release must be explicitly authorized and use the project's existing Core SemVer/release process. Do not automatically create a `v1.0.0` product tag merely because the cognitive capability is called v1.0. Desktop adoption remains a separate authorized change.
6. Only after the exact cognitive integration is on main, create a new branch for separately authorized Context Space work. Keep both planes in the same Go module and `runtime` as their composition root.

## Branch/worktree cleanup candidates / 分支与 Worktree 清理清单

**None is deleted now.** The full path, branch, SHA and dirty-at-start status of every one of the 27 protected cognitive worktrees is in `GIT_RELATIONSHIPS.json`; the main worktree is the 28th protected record. Cleanup requires confirming no live process/session owns a worktree, preserving its evidence and obtaining Owner approval.

| Class / 分类 | Paths or branch group / 对象 | Proposed handling after authorization / 授权后建议 |
|---|---|---|
| Current formal input | `wt/core-cognitive-core-icra-final-verify-20261115` | Retain until remote integration and delivery hashes are verified; keep review commit/evidence permanently. |
| Current integration | `wt/core-cognitive-v1-integration-20261116` | Retain through Owner acceptance; remove only after published commit is safely reachable and no task is active. |
| Dated clean review evidence | `wt/core-cognitive-core-final-completion-20261113`, `wt/core-cognitive-core-fable-full-review-20261114`, `wt/core-cognitive-core-icra-cross-review-20261115` | Worktrees may become removable, but commits and all external ledgers/logs must remain reachable. Check links before removing directories. |
| Earlier clean cognitive candidates | `core-cognitive-core-*20261111`, `*20261112`, `core-cognitive-core-t5a-*`, `core-cognitive-finalization-integration-20261111`, `core-cognitive-finalization-f05-20261111` | Review the exact manifest one by one; no wildcard deletion. Not automatic merge inputs. |
| Detached verification trees | `core-cognitive-capability-*20261110`, `core-cognitive-governance-verify-20261113`, `core-cognitive-learning-verify-20261113`, `core-cognitive-p0-remediation-verification-20261111` | Retain referenced commits and evidence before optional worktree removal; do not invent branch names for detached trees. |
| Dirty historical trees — do not delete | `wt/core-cognitive-finalization-baseline-20261111`, `wt/core-cognitive-finalization-main-delivery-20261111` | Owner must separately reconcile untracked/staged work. A newer candidate does not authorize deleting it. |
| Main and unrelated projects | `LumenCortex-Core`, Desktop, benchmark/other worktrees | Not cleanup targets. Preserve their changes and independent Git histories. |

The existing shared Linux container is not owned by this task and was neither stopped nor removed. The unique Linux verification directory and owned Windows cross-test binary directory were removed after their checks (`logs/platform-cleanup.log`). Workspace-root TODO and evidence are outside the Core Git repository; do not claim they are committed.
