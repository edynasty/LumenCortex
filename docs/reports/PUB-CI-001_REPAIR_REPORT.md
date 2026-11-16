# PUB-CI-001 — GitHub Actions Linux Sandbox Environment Repair

## Scope and current evidence / 范围与当前证据

Owner authorizes CI-only workflow, dependency, runner-environment and documentation changes. Base: `c73d17359f5d9dd16ef88b0de4525862593fddaa`. Branch: `fix/pub-ci-001-linux-sandbox-20261116`. Cognitive Core production code, Go test assertions, SQLite schema and migrations 0014–0017 remain unchanged. No privileged container, unsandboxed fallback, added test skip or relaxed sandbox policy is permitted.

原始失败：GitHub `go-runtime` main run [43824768729](https://github.com/edynasty/LumenCortex/actions/runs/43824768729) 与整合分支 run [43824657120](https://github.com/edynasty/LumenCortex/actions/runs/43824657120)。Ubuntu 24.04.5，image `20261111.370.1`，Go 1.27.4。每个run在evalruntime/runtime产生171个直接bwrap缺失错误及5个父测试失败；后续Vet/Build未执行。原日志位于工作区 `bench/evidence/cognitive-core-v1-github-delivery-20261116/`，不回写。

The production Linux launcher calls `exec.LookPath("bwrap")`; its observed error establishes that no executable was discoverable on the test process PATH. It does not prove a namespace, seccomp or version failure, because the process never reached bwrap execution. Existing CI installs no Linux sandbox prerequisites. Linux workers also require real cgroup v2 memory/pids delegation via a systemd user manager; installing bwrap alone does not satisfy that independent contract.

## Intended minimal correction / 最小修复方向

Retain GitHub-hosted, non-container Ubuntu 24.04 and the existing Go version. Install the existing sandbox dependencies explicitly, establish the test user's normal systemd/cgroup context, and run a fail-closed namespace/filesystem/network/process smoke before Go tests. Record actual image/kernel/tool versions and permissions. Do not change AppArmor/sysctl policy to bypass refusal. Unsupported host conditions must fail clearly.

## Verification status / 验证状态

Implementation and real GitHub Actions validation are in progress. No new PASS is asserted. The final evidence will separately identify the independent bwrap smoke, evalruntime, runtime, existing negative controls, full Go CI and independent review. Local checks cannot substitute for hosted-runner results.

## Evidence location / 证据目录

New execution receipts, original-source manifest, exact commits, Actions logs and closure records: workspace `bench/evidence/pub-ci-001-repair-20261116/` (outside Core Git). This report is committed with CI changes; a post-commit receipt binds the exact SHA without pretending a commit can include its own identity.
