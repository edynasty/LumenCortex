# Core 全量整合 - 2026-11-04

结论:**MERGED_AND_VERIFIED**

授权依据:Owner 于 2026-11-04 明确授权,把 `bench/execution-lifecycle-candidate` 上的全部工作一次性整合进 Core `main`,包括 COG-001/R1a、RT-MERGE-001 B1、Runtime Upgrade、Provider Retry、Delivery Contract 和 Benchmark 材料。本轮不推送、不打 tag、不发版本。

## 1. 合并前状态

- `main` = `b90cb8f3b680258589df9d345d94cb66465ac7a8`(其中 `b90cb8f` 只改了 `README.md` 和 `README.zh-CN.md`)。
- 候选分支整合前 HEAD = `4786db146f73244b35986619cc22b3cba2921e59`。
- 共同祖先 = `c08f5c44ed753e239bfe3656f63f4d7a0ebc268a`。
- 候选分支领先 `main` 194 个提交;`main` 领先候选分支 2 个,即 `3e1c3cc`(其内容已包含在候选分支中)和 `b90cb8f`。
- 候选 worktree 中有 25 个已跟踪文件被修改、12 个未跟踪文件,内容是 Delivery 的修复、测试和审查报告。
- 共 48 个 Core worktree。

## 2. 未提交修改的处理

- **提交者:** 2026-11-04 17:15:21,候选 worktree 的全部未提交修改被提交为 `61628ef7c3af795b0d673ed706d8569a04b695e2`("fix(runtime): finalize delivery contract and regression coverage",37 个文件,+2954/−237)。5 秒后(17:15:26),`main` 上生成了 `--no-ff` 合并提交 `d5920c1c1c85151371926da0ba576ee69025b315`。这两个提交是由与本审查会话并行的操作者完成的,不是本会话执行的,本会话发现时已经存在。
- **本会话事后核实的内容:**
  - `61628ef` 包含两部分:Astra 第二轮的 N1–N4 修复,以及本会话在 14:12 做的 N5 修复和回归测试。worktree 中除 `task-delivery-fix-round2-review-2026-11-04.md` 外,没有任何文件晚于本会话最后一次已验证的状态;该报告在提交前被追加过内容。
  - 合并后 worktree 是干净的,没有遗漏的未跟踪文件。
  - `d5920c1` 的 tree(`07c5c8f`)与 `git merge-tree --write-tree b90cb8f 61628ef` 独立算出的结果完全相同,因此不存在冲突,也没有任何内容被手工改写。
  - `d5920c1` 与 `61628ef` 的差异只有 `main` 上 README 的那两个文件。
- **历史保留:** 候选分支原有的 177 个非合并提交和 18 个内部合并全部保留;没有 squash、rebase,也没有改写历史。

## 3. 合并关系与提交数量

- 合并提交:`d5920c1`,父提交依次为 `b90cb8f`(原 `main`)和 `61628ef`(候选分支 HEAD)。
- 合并后修复:`f9d2e6b`(LSP,见第 5 节)。
- 本次进入 `main` 的提交:`b90cb8f..61628ef` 共 195 个,其中 177 个非合并、18 个内部合并。加上合并提交和修复提交后,`main` 新增 197 个提交。
- 相对 `b90cb8f`,共 650 个文件变化,+155419/−2234。
- 冲突:无(merge-tree 退出码 0,tree 一致),不需要任何 ours/theirs 选择。
- 里程碑分支现在都是 `main` 的祖先:`cog/r1a@15bf4eb`、`rt/execution-runtime@ac27cad`、`fix/bench003-shell-timeout`(`ff5ccdb`)。

**纳入的工作:**
- COG-001/R1a:T1–T4、PROVIDER-005/007、LOOP-001、CHECKS-001/002。
- Runtime Upgrade(RT-MERGE-001 B1):jobs、sched、notebook、LSP、plugins/MCP、diag,以及独立审查修复。
- BENCH-003:shell 超时修复。
- PROVIDER-006 和 Provider Retry/Execution Lifecycle(`fb40879..4786db1`)。
- Delivery Contract(`ff5ccdb` 和 `61628ef`)。
- `benchmarks/tb2/` 材料。

**未纳入的分支:** 以下分支有不在候选分支上的提交,不属于本次授权范围,原样保留:
- `feat/bench004-trace`(24 个提交)和 `bench/infra-fix`(23 个):`git cherry` 显示这些是独立的新提交。
- `review/astra-runtime-20261102`、`review/astra-bench003-20261103`:各 1 个文档提交。
- `cog/t3-s4-prerebase`、`cog/t1-s1-schema`、`cog/h1`:变基前的旧引用。
- `lab/workbench-api`:实验分支。
- `fix/astra-*` 共 5 个提交:内容已经以等价补丁形式进入 `main`(`git cherry` 全部为 `-`)。

## 4. 备份引用

- `backup/main-before-full-candidate-20261104` -> `b90cb8f`(在本会话之前已存在)。
- `backup/candidate-full-integration-20261104` -> `61628ef`(本会话新建)。
- 两者都仍可解析。没有删除任何 worktree、分支或备份。

## 5. 合并后发现并修复的问题

**LSP-RACE-001(P2,由 Runtime Upgrade 带入,与 Delivery 无关)**
- **现象:** `d5920c1` 上第一次运行 `go test -race ./...` 时,`internal/lsp` 中的 `TestManagerRestartsACrashedServer` 失败,报 `err=EOF, want ErrUnavailable`。其余 31 个包通过。
- **根因:** `Client.readLoop` 先让所有 pending 请求失败,之后才执行 defer 中的 `close(done)`。`Manager.call` 以非阻塞方式检查 `Done()` 来判断服务器是否已退出,因此可能错过退出信号,把原始 `EOF` 而不是 `ErrUnavailable` 返回给调用方。
- **修复:** 提交 `f9d2e6b`。先记录 stderr 中的退出原因,然后关闭 `done`,最后让 pending 请求失败。
- **回归测试:** 新增 `TestClientRequestFailedByServerExitSeesDone`。测试持有 stderr 缓冲锁,从而确定性地停在这个顺序窗口上。
  - 修复前连续运行 3 次全部失败。
  - 修复后:普通模式运行 20 次、`-race` 运行 30 次(与 `TestManagerRestartsACrashedServer` 一起),全部通过。

**Delivery N5(P2)**
- 合并前在 worktree 中修复,已包含在 `61628ef` 中,详见 `task-delivery-final-acceptance-2026-11-04.md`。

## 6. 合并后验证(`main` = `f9d2e6b`)

| 检查 | 结果 |
|---|---|
| `go test -race -count=1 ./...` | 退出码 0;32 个包 ok、3 个包没有测试;没有 DATA RACE;runtime 307 s |
| `go test -count=1 ./...` | 退出码 0;32 个包 ok、3 个包没有测试;runtime 216 s |
| `go build ./...` / `go vet ./...` | 通过 |
| `GOOS=windows go build ./...` | 通过 |
| `GOOS=linux go vet ./...` | 通过 |
| `GOOS=windows go vet ./...` | **失败,仅限测试文件**:`internal/lsp/manager_test.go` 使用 `syscall.Kill`;`runtime/jobs_test.go` 引用的 `processRunning` 只定义在 `_unix_test.go` 中。这是 Runtime Upgrade 原本就有的问题(`15bf4eb` 不存在这些文件,`ac27cad` 已存在),产品构建不受影响,已记入 TODO,编号 TEST-WIN-001 |
| 定向:cognition、session、migrate、jobs、schedule、notebook、mcp、lsp 包 | 全部 ok |
| 定向:runtime 中撤销/fence/隐私/来源边/jobs/调度/唤醒/notebook/LSP/MCP/Deadline/Cancel/MaxSteps/Retry/Recovery/SIGKILL/Resume 相关测试 | 125 PASS,0 FAIL |
| Delivery 无 Git 真实 CLI:macOS(uid 501)和 Linux(uid 65534,无 Git 镜像) | 两个平台都是 completed/verified;权限故障时为 `verification_unavailable`;恢复后重验为 verified,RunID 不变;模型请求共 2 次,重验额外 0 次;非幂等标记只写入一次 |
| Linux 非 root Delivery 测试(有 Git / 无 Git 两种镜像) | 全部 PASS。在无 Git 镜像中,只有两个需要用 git 搭建 fixture 的测试被跳过,它们在有 Git 的镜像中通过 |
| Benchmark 材料一致性 | `MANIFEST.sha256` 171/171 OK。`PREREGISTRATION_TB2_W13.sha256` 中已提交的 4 项 OK,另 2 项指向未提交的 `build/` 二进制。13 个 Python 文件全部可解析,3 个 shell 脚本语法检查通过 |

## 7. 未验证项及原因

- **付费 TB2、冻结 Cohort、官方 Verifier:** Owner 禁止运行,因此没有执行,不声称任何 Reward。
- **`ARTIFACTS.sha256`(4167 项):** 它记录的是工作区中未提交的 W13 jobs、`build/` 和数据集。按 README 的说明,这些文件不在 Git 中,`main` 检出时不存在,所以无法在 `main` 上校验。这不是一次失败。
- **`PREREGISTRATION_TB2.sha256` 中 `PREREGISTRATION_TB2.md` 的哈希不匹配:** 该文件只在 `c45f681`(2026-10-28)提交过一次,提交时就已经不匹配,本次合并没有改动它。属于历史记录问题,不作改动,只报告。
- **Windows 运行时测试:** 测试文件无法交叉编译(见上表),也没有 Windows 主机。
- **真实 LSP 服务器(gopls 等)、Browser MCP、真实模型 provider 重试:** 本轮只运行了测试中使用的替身或辅助进程。
- **在 Delivery 结算窗口内做真实 SIGKILL:** 未执行;对应的恢复语义由 completed+pending 的测试覆盖。
- **Desktop:** 本次没有更新它的 Core pin(RT-MERGE-001 中 Desktop 部分的 B6/T7-S1 仍未完成)。

## 8. 收尾状态

- `main` 工作区干净;48 个 worktree 全部干净。
- 没有推送,没有打 tag,也没有修改版本常量。
- 临时 smoke 产物(`/tmp/dlv-final`)在报告提交后删除。

## 9. 剩余风险与后续事项

- TEST-WIN-001(P3):Windows 下测试文件无法编译。
- FMT-001(P3):仍有 19 个文件不符合 gofmt,本次没有新增。
- Desktop 还需把 Core pin 更新到一个已发布的 `main` 提交;发布或 tag 需要 Owner 另行决定。
- `feat/bench004-trace` 和 `bench/infra-fix` 是否需要整合,需要单独决定。
- Delivery 的剩余风险(没有多文件原子快照、阻塞 I/O 没有严格时间上限、存储完全不可写时错误无法落盘)见 Delivery 最终验收报告。
