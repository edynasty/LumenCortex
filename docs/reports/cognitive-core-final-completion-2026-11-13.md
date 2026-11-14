# Cognitive Core Final Completion / 认知核心完整收敛

Status: IMPLEMENTED / ENGINEERING-VERIFIED / INDEPENDENT-REVIEW PASS / FROZEN LOCAL CANDIDATE; not merged or released. Primary implementer and architecture owner: `local/gpt-6-astra`. All 14 in-scope Findings are fixed; separate reviewer instances verified the corrections. Long-term autonomous-learning benefit remains unmeasured.

> Addendum 2026-11-14, not part of the 2026-11-13 record, which is left as written: an independent full review of this frozen candidate (`c1d3229`) reproduced 17 further defects, four of them P1, and fixed them on `review/cognitive-core-fable-full-review-20261114`. See the [review report](cognitive-core-fable-full-review-2026-11-14.md) · [中文](cognitive-core-fable-full-review-2026-11-14.zh-CN.md).

## Scope and protected baseline

- Owner authorizes one Go-only completion project: Memory, Attention, Reasoning, Learning, Belief Revision, Governance and Verification, with unified authority/lifecycle semantics.
- Fetched GitHub main: `00344b5d6a67673fea9fcbc90e0cab16de17dc57`. Main worktree has a pre-existing `AGENTS.md` modification; preserved. Other cognitive worktree/index changes are recorded and not imported.
- Isolated branch/worktree: `feat/cognitive-core-final-completion-20261113`, `wt/core-cognitive-core-final-completion-20261113`.
- Historical candidate `91af861da4c3714ae8d9d8214d22501ce61c5002` contains `61a1d31`, `e2c69c6`, Fable FR-001/003 and Astra ACR-001/002. These candidates are NOT main. Common ancestor: `208ad6bb1bc969e9b71703631ac1da7a76fec79c`.
- Import uses the committed production/test delta and relevant cognitive contracts only. Main's latest documentation and translations are preserved. No merge, push, tag, release, paid request, Desktop edit or production database mutation.
- Workspace evidence: `bench/evidence/cognitive-core-final-completion-20261113/BASELINE.json` and `CANDIDATE_IMPORT.patch` (outside Core Git).

## Current architecture decision

Keep one SQLite store, one Agent Loop, one Governor and the existing Task State, Context Graph, maintenance lease and evaluator. Working memory remains Task State; episodes remain observable session/run records; semantic candidates remain `cognition_candidates`; typed beliefs/procedures remain versioned `cognition_nodes`. Share scope, source, time, evidence, revision and privacy predicates rather than introduce a universal memory object. [Architecture](../cognitive-core/architecture.md) · [中文架构](../cognitive-core/architecture.zh-CN.md) · [ADR-0023](../adr/0023-unified-cognitive-lifecycle.md).

The imported historical implementation supplied qualified evidence, source fences, CAS revisions, bounded maintenance and host-invoked Experience APIs. This candidate now connects Experience tools, explicit application receipts and terminal/restart finalization to the ordinary Agent Loop. Native graph metadata and host-only Governor lifecycle operations cover dependencies, protected retention and evidence-equivalent consolidation; task-ranked attention reuses the existing tokenizer. No second runtime or database was introduced.

## Work and verification

Verified receipts under the workspace evidence directory (not Core Git):

| Command / surface | Observed result | Receipt |
|---|---|---|
| Final macOS ARM64 Go 1.26.5 CGO build/vet, `949b2c6` | Both exit 0 | `candidate-macos-build.*`, `candidate-macos-vet.*` |
| First full suite, `2151f17` | Exit 1; 2882 PASS / 5 FAIL / 46 SKIP; original failures preserved | `final-macos-tests.*` |
| Task-index fixture correction, `445e667` | 14 PASS / 0 FAIL / 0 SKIP; five goal strings changed, all assertions retained | `final-index-fixtures.*`, `FIXTURE_SOURCE_BRIDGE.json` |
| Clean full `go test -json -count=1 -timeout=1800s ./...`, `eb47a7d` | Exit 0; 2921 PASS / 0 FAIL / 46 SKIP | `freeze-macos-full.*` |
| Final permission delta, `949b2c6` | 36 PASS / 0 FAIL / 0 SKIP; real deny-all application, Workflow and EffectivePrompt callers | `ilr001-empty-allowlist-green.*` |
| Final source-bound unique test union | 2930 PASS / 0 FAIL / 46 SKIP; full `eb47a7d` plus verified `949b2c6` delta, **not a single final-SHA full-suite command** | `FINAL_CANDIDATE_TEST_RESULTS.json`, `ILR001_EMPTY_ALLOWLIST_SOURCE.json` |
| macOS cognitive Race, `2151f17` | 371 PASS / 0 FAIL / 0 SKIP; historical broad coverage | `final-macos-cognitive-race.*` |
| macOS remediation Race, `eb47a7d` then `949b2c6` | Separate runs: 63 PASS and 36 PASS, each 0 FAIL / 0 SKIP | `freeze-macos-repair-race.*`, `ilr001-empty-macos-race.*` |
| Public offline Engine demo | Real tools/checks, distinct origin/consumer runs, cleared/recreated output, conditional feedback, refusal and reopen; no HTTP model client | `completion-demo-1.*` |
| Restart/settlement/reader-local access | 3 PASS; real SQLite injected failure, pending remains unknown, no replay | `completion-recovery-1.*` |
| Migration 0017 | Exit 0; upgrade/failure rollback, clean roundtrip, nonempty/new-semantic downgrade refusal and hot-WAL missing-table preservation | `completion-migration-2.*` |
| CC-001, CC-005/006, CC-007 regression | Old implementation fails; current behavior passes, originals retained | `cc001-red.*`, `cc005-006-{red,green}.*`, `cc007-*.stdout` |
| CC-002/003/004 missing-mechanism controls | Each Go overlay compiles and FAILs its behavior assertion; patches retained. Production remains unchanged. | `cc002-missing-tools.*`, `cc003-missing-retention.*`, `cc004-missing-relevance.*` |
| Linux ARM64 CGO Go 1.27.1, earlier candidate | Build/vet exit 0; affected Race 159 PASS / 0 FAIL / 0 SKIP | `final-linux-{build,vet,cognitive-race}.*`, `LINUX_FINAL_ACCEPTANCE.json`, `PLATFORM_SOURCE.json` |
| Linux remediation Race, `eb47a7d` then `949b2c6` | Separate offline native runs: 63 PASS and 36 PASS, each 0 FAIL / 0 SKIP | `freeze-linux-race.*`, `ilr001-empty-linux-race.*` |
| Windows AMD64 Go 1.26.5, `eb47a7d` | Full-package test compilation exit 0, CGO disabled; final delta is portable slice semantics exercised natively on macOS/Linux. **No final-SHA Windows build, native SQLite execution or Windows Race claim.** | `freeze-windows-tests-compile.*` |

Counts are Go `-json` terminal test events, including parent and subtest rows; they are not independent scenario counts. Package terminal events are never added to test counts. `FINAL_CANDIDATE_TEST_RESULTS.json` retains the 46 SKIP outputs: opt-in real DAP/IDE adapters, Linux-only behavior on macOS, helper entrypoints, captured/provider inputs and optional Eval runtimes. No new Skip suppresses a completion failure. The final two-branch Workflow correction is proven by red/green, all affected permission/Workflow/Prompt tests, native Race on both available platforms, and final build/vet; the clean full run remains pinned to `eb47a7d`.

The initial five FAIL events are three leaf knowledge-index scenarios plus two parent tests. Generic “use cognition/services” task labels no longer select arbitrary facts under the new positive-relevance rule. Fixtures now name service ports/alpha/beta/gamma; the same applicability, conflict and two-check assertions pass. An added Engine regression proves unrelated tasks receive no locator and reordered related Experience tokens still match. This is a contract-fixture migration, not a relaxed expectation. Original compile/fixture/product failures are retained and classified in the Ledger.

Linux validation uses the pre-existing `lc-ubuntu-go:24.04` image (`sha256:5429acb521265c6d07af7a371b975cf88d333f3e554e987836ea77adbc12651e`), read-only source/module mounts, offline module resolution and task-owned caches. These are native Linux/ARM64 Go/SQLite executions inside a container, not UTM/full-OS sandbox acceptance. Each process is capped at two CPUs and 2 GiB; shared Docker settings were untouched. Windows evidence is test-binary compilation only; no Windows host was available. Toolchain versions differ and are not described as one identical platform environment.

The single [Finding Ledger](../cognitive-core/finding-ledger.md) records all 14 Findings: CC-001–008, CC-GOV-001/002 and ILR-001–004. The last permission defect was not hidden by the clean full suite: `ilr001-empty-allowlist-red` proves both explicit-empty clone failures and an actually executed forbidden second action. `slices.Clone` fixes the shared nil-versus-empty boundary for ordinary runs, nested application and prompt inspection. The allowed/restricted/human-gate/hook-denied controls remain. Long-term learning benefit and arbitrary task transfer are unmeasured; deterministic mechanism proof does not establish them.

## Git and delivery boundaries

| Artifact | Identity / status |
|---|---|
| Main at task start | `00344b5d6a67673fea9fcbc90e0cab16de17dc57`, unchanged by this task |
| Imported history | `91af861da4c3714ae8d9d8214d22501ce61c5002`; includes `e2c69c6`, `61a1d31`, `1966cec`/ACR-001/002; not an ancestor claim for the new branch because only scoped committed delta was imported |
| Unified Go implementation | `2151f17b33a84912892353e69c9b00f6b348a239` |
| Task-specific regression contract | `445e667`, no production code difference |
| Application/canonical remediation | `eb47a7d4aa9aa1429bf5fb50d6b2483445faf07c` |
| Final production / test candidate | `949b2c6533dcf66f1401d0b72c5d2c44cb560354`; only the shared explicit-empty allowlist correction and its regressions follow `eb47a7d` |
| Documentation / final freeze | Separate local documentation commit; exact SHA, clean tree and checksums recorded in workspace `FINAL_DELIVERY.json` / `SHA256SUMS`; no publication authorized |

The code commit includes imported historical fixes and new completion implementation. `CANDIDATE_IMPORT.patch`, source hashes and baseline ancestry preserve provenance without rewriting old branches. New completion migration is 0017 after unchanged Hub 0013/cognition 0014–0016. No existing user/production database was opened for migration. All tests/demo use disposable state; downgrade deliberately refuses semantic loss rather than offering false backward compatibility.

All new production implementation is Go. No Desktop feature, second agent loop, new database, external embedding service, benchmark platform or autonomous model worker was introduced. Workspace evidence and root `TODO.md` are outside Core Git; repository docs are committed with the candidate. Merge, push, tags and release require a separate Owner decision.

The freeze's exact documentation commit and clean candidate state are recorded in `FINAL_DELIVERY.json`; `FINAL_SOURCE.json` binds the final Go/test source. `FINAL_BASELINE_PRESERVATION.json` compares protected worktrees with the starting snapshots. `DOCUMENT_LINKS_FREEZE.json` checks the final documentation references. `SHA256SUMS` covers the new evidence directory and delivered documents; `CHECKSUM_VERIFICATION.json` records its verification. These manifests live under workspace `bench/evidence/cognitive-core-final-completion-20261113/`, not Core Git. No historical evidence is rewritten to make the new manifest pass.

## Independent verification scope

The mounted `task` tool rejected explicit model overrides (`task.perCallModel: off`); configured reviewer/worker routes were not Astra. No model/config permission was changed. Two separate read-only OMP CLI sessions used explicit `--model local/gpt-6-astra --models local/gpt-6-astra` in isolated review worktrees. Session metadata confirms that model and `resolvedModelIsFallback:false` (`INDEPENDENT_REVIEW_MODEL_METADATA.json`), including exact-session closure continuations. They inspected source and primary-executed evidence, did not implement the fixes and did not rerun tests. This is independent reviewer-instance verification, not cross-model corroboration.

- Governance: `closure-governance.stdout` PASS at `eb47a7d`; CC-GOV-001/002 closed, CC-003/006 accepted and unchanged CC-004/005 acceptance retained.
- Learning: `closure-learning.stdout` correctly kept ILR-001 open despite the clean full suite. `closure-learning-empty.stdout` PASS at `949b2c6`; ILR-001 closed with deterministic red/green. CC-001/002/007/008, ILR-002/003/004 and inherited ACR-001/002 are closed/retained. Governance files are unchanged by this final correction.
- No unresolved P0/P1/P2/P3 Finding remains in this completion scope. Existing out-of-scope scale/platform/product limitations are not relabelled as tested capabilities. Historical review verdicts and failures remain unchanged.

## 工程结论与最终十问

1. **统一架构：**候选已有一个 Go Engine/Store 上的统一范围、时间、来源、证据、CAS、Fence 和生命周期；不是七套独立服务。
2. **七领域协同：**任务与上下文进入召回，证据判断影响可用前提，普通工具执行产生经历，反馈驱动修订和保留，随后任务再使用同一来源链。
3. **经验学习闭环：**普通 Agent 的提案→提取→检查→采纳→明确应用→反馈→修订可达且有真实 SQLite/工具证据。使用的是原操作序列，不承诺任意参数迁移。
4. **未知、冲突、错误、过期：**独立状态与受影响依赖保留；没有检查、来源变化、条件不成立或证据冲突不能被当作当前事实。
5. **记忆管理：**工作/情景/语义/程序记忆复用现有存储，短期上下文压缩、带来源摘要、等价证据归并、私有访问统计和保护性容量维护分工清楚；不以 TTL/LRU 删反例。冷热层是逻辑策略。
6. **Governor：**正常 Go CGO SQLite 可用，Analyze/Plan/Validate 加宿主事务化生命周期 Apply；模型 Curator 和自主 Epoch 未获准入口仍关闭，不是必需的第二套运行时。
7. **main 已有内容：**`00344b5` 已有 Harness、Task State、Session/Notes、Context/Knowledge、Attention Light 与只读 Governor 基础。本轮完整经验/原生图/生命周期属于候选，未进 main。
8. **未解决能力：**未测真实模型提案质量、长期学习收益、任意任务泛化、大规模经验集性能；Windows 原生 SQLite/恢复/Race 未运行。它们不冒充本轮 PASS。
9. **架构级缺口：**既定本地可信宿主与有条件复用范围内，未留已知未修复的架构阻断，治理和学习独立增量复核均通过。跨项目共享、通用因果推理、参数化程序迁移和物理分层不是本轮已经实现或证明的能力。
10. **是否达到初始目标：**已达到可运行、可检查、可修订的条件认知工程闭环，并形成可冻结的本地候选；未证明持续自主学习带来真实任务收益，更不等于人类级认知。main、发布和长期效果仍分别判断。
