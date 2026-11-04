# Harbor + Terminal-Bench 2.0 环境整改与复测报告

状态：进行中；供 Opus 5.5 独立审核。本文只记录已观测证据、可复现命令和明确未决项，不宣告产品 Gate。

## 1. 范围与不变量

- 基准：官方 `terminal-bench@2.0`，任务、Verifier、评分标准保持原样。
- 执行框架：Harbor 0.25.0 原生 `harbor run`。
- 当前批次：`tb2-w13a` 已完成；`tb2-w13b` 仍在运行，任何复测必须使用新 job 目录并单独统计。
- 禁止事项：不修改 Core 生产代码、官方评分器；不 Push/Tag/Release/Deploy。
- 公开对照：`bench/public/PUBLIC_BASELINES.md` 及其原始数据，条件差异单独披露。

## 2. 当前执行状态

| 批次 | 状态 | 证据 |
|---|---|---|
| `tb2-w13a` | 已完成，24 trials，raw mean 0.458333；11/24 reward=1；1 个 Harbor error | `jobs/tb2-w13a/result.json` |
| `tb2-w13b` | 已完成，15 trials，raw mean 0.466667；7/15 reward=1；0 errors；pass@2=0.666667 | `jobs/tb2-w13b/result.json` |

观测到的执行环境：Harbor 0.25.0，Docker server 29.8.2，Compose v5.6.0，Docker Desktop 10 CPU / 约 3.83 GiB，macOS 27.0.1 arm64；两个原始批次均使用 `-n 1 -k 3 --timeout-multiplier 2` 与 `host.docker.internal:7897` HTTP(S) 代理。W13A/W13B 原始目录已冻结；复测使用独立 `tb2-w13r-*` job。

```sh
cd /Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench
env -u all_proxy -u ALL_PROXY harbor --version
docker version --format '{{.Server.Version}}'
docker compose version
sh run_w13.sh <job-name> lcx_agent:LcxAplus 3
python3 collect_w13.py jobs/<job-name> > jobs/<job-name>/trials.jsonl
```

The frozen task selection and settings are in `run_w13.sh` and `PREREGISTRATION_TB2_W13.md`. The current process uses the same script with `--ae` proxy variables shown in the process inventory. Raw trial directories, Core state databases, verifier output, request bodies and response bodies remain under `jobs/`.

## 2.2 Cross-batch metering caveat

`jobs/metering/proxy.jsonl` is shared by runs. Per-trial joins are valid only when matching the exact trial suffix; unrelated tags such as `A+fix/cancel-async-tasks__KdUVbX9` must be excluded. The collector currently joins by `tag.endswith("/" + trial.name)`, which preserves exact trial identity but does not filter a job by arm label; report aggregation therefore uses the trial directory as the authority.

## 2.3 Confirmed Provider/Core signals

- Five copied W13 state databases contain DeepSeek native DSML markup in visible `content` rather than structured `tool_calls`: one `finish=stop` response was accepted as final content (`cobol-modernization__WWMWyA2`), and four more ended at exactly 32,768 completion tokens with `finish=length`. The sixth-body count in an earlier note was not reproducible from copied state databases and is removed from the confirmed total.
- W13A gateway failures were four three-request bursts: 502/502/503 at `prove-plus-comm` ×2, `sanitize-git-repo`, and `vulnerable-secret`. W13B had recovered gateway events and a 503×3 burst; exact per-trial status is in `evidence/harbor-remediation-20261027/trials.csv`.
- `sanitize-git-repo__rXFrGHS` ended with `lcx-go: model returned neither tool calls nor final content` after reward-producing work; this is a Core/provider abort signal and does not change its reward=1 classification.

These are investigation findings only. No Core production source or official scorer was changed during this task.

## 3. 已核实的 W13A 故障分类

### 3.1 INFRA / ENV

| 任务 / trial | 分类 | 证据 |
|---|---|---|
| `configure-git-webserver__Vv5u3Vg` | `INFRA`，需按规则复测 | Verifier 无 CTRF；输出显示 apt 下载失败后 `curl`/`uvx` 不存在，见 `verifier/test-stdout.txt`；该 trial 无 gateway non-200 记录，故归为 Verifier 环境故障。 |
| `pytorch-model-cli__67LsMqP` | `ENV`，按预注册规则单列 | Verifier 安装 170 MB 依赖时 `libmp3lame0` 返回 `502 Bad Gateway`；随后 `curl`、`env`、`uvx` 不存在，`test.sh` 未能进入任务检查；`verifier/test-stdout.txt` 第 156–322 行。该任务三次 oracle 失败记录在 `EXECUTION_LOG_TB2_W13.md` 第 18–35 行。 |
| `sanitize-git-repo__sU9xstz` | `INFRA`，需按规则复测 | Verifier 的 apt 在 DNS 阶段失败，随后 `curl`/`uvx` 不存在；`verifier/test-stdout.txt` 第 10–46 行；metering 为同一 trial 的 502、502、503。 |
| `vulnerable-secret__75TyDtG` | `INFRA`，需按规则复测 | Agent 在 0 请求、0 steps 时被 503、503、503 终止；metering 与 `agent`/`result.json` 对齐。 |
| `prove-plus-comm__45jET6q`、`prove-plus-comm__JSG5wMv` | `INFRA`，reward 1 保留为原始结果 | 每个 trial 有 502、502、503；Agent exit=1、session interrupted，但 Verifier 后续 reward=1。按预注册规则，已解决 trial 不重跑；原始 reward 与基础设施事件同时保留。 |
| `sanitize-git-repo__rXFrGHS` | `INFRA` 事件但 reward 1 | 502/503 事件未改变最终 reward=1；不重跑已解决 trial。 |
| `cobol-modernization__h9F6X5v` | `TIMEOUT`，含 Harbor artifact 收集异常 | Agent execution 到 1800 s，Harbor `AgentTimeoutError`；Verifier reward=1。随后 `compose cp` 找不到 `/tmp/lcx/exit.txt`，这是 Harbor/adapter 产物收集缺陷信号，不能当作模型失败。 |

### 3.2 MODEL / HARNESS signals

| 任务 / trial | 分类 | 证据 |
|---|---|---|
| `raman-fitting__j88UAtk`、`__2YiHjqv`、`__mFpm2Dv` | `MODEL`（当前证据） | apt、curl、uv 和 pytest 均完成；`__mFpm2Dv` CTRF 为 2 passed、1 failed，失败是结果数值不符合阈值，见 `verifier/test-stdout.txt` 第 142–188 行；无 gateway non-200。 |
| `configure-git-webserver__8A3Szjo`、`__ByarrX3` | `MODEL`（当前证据） | Verifier 实际运行且返回 404；CTRF `tests=1, failed=1`，见 `__8A3Szjo/verifier/ctrf.json`；无 gateway non-200。 |
| `pytorch-model-cli__946HcD5` | `MODEL`/`HARNESS` 待拆分 | Verifier 已运行但 trial 有 `finishReason=length`；需结合 agent final、任务文件和 Core loop 证据决定，不能直接计入纯 MODEL。 |
| `configure-git-webserver__ByarrX3`、`pytorch-model-cli__946HcD5`、`raman-fitting__j88UAtk`、`__2YiHjqv` | `HARNESS` signal | Core DB 的 `agent_steps` 显示 `finishReason=length`，而 agent 结果仍为 `status=completed`；这与 backlog `LOOP-001` 的既有证据一致。本报告把它作为解释变量单列，不改写原始 reward。 |
| `cobol-modernization__WWMWyA2` | `MODEL`/Provider-format signal | 无 metering non-200，Verifier 运行后失败；agent result 的可见 final 含未解析 `｜DSML｜` 标记。该 trial 需要作为 Provider translation / Core finalization 风险单列，不能仅归为任务能力失败。 |

原始成绩不因上述分类重写；分类只用于 INFRA 复测和可比性分析。
### 3.3 W13B final classification

W13B finished with 15/15 trials, raw mean 7/15 = 0.466667, zero Harbor errors, and pass@2 0.666667. The final evidence is:

| Trial(s) | Class | Evidence |
|---|---|---|
| `extract-elf` ×2, `hf-model-inference` ×3, `multi-source-data-merger__mLbB6oK`, `sqlite-with-gcov__DWyjyZW` | solved | Verifier tests passed; `hf-model-inference__DQbvfBw` had a recovered gateway event and still reward 1. |
| `extract-elf__nSfcnQf`, `multi-source-data-merger__ZUcLhd3`, `sqlite-with-gcov__H3Pkh2U` | `INFRA` | Run-ending gateway 503×3; no usable model completion and/or verifier could not observe the required binary. |
| `multi-source-data-merger__JZvrxD2`, `sqlite-with-gcov__CsX7xL5` | `INFRA` | Verifier dependency installation failed on HTTP 5xx before tests; `tests=0`. |
| `tune-mjcf__BigZ6Nc` | `INFRA` | Run-ending gateway 503×3 after a long agent execution; reward 0 and no valid verifier completion. |
| `tune-mjcf__phxPuSo` | `MODEL` with truncation signal | Verifier ran and failed 3/4 because `/app/model.xml` was missing; `finishReason=length` was recorded. |
| `tune-mjcf__3mForem` | `MODEL` | Verifier ran and failed 3/4; it is a completed raw reward=0 model result, not an additional INFRA replacement slot. |

W13B is frozen in `jobs/tb2-w13b/result.json`; final per-trial evidence is in `evidence/harbor-remediation-20261027/`.



## 4. 环境整改记录

尚未执行写入式系统或 Docker 配置变更。已知历史材料记录 Docker Hub 拉取慢、国内镜像 `docker.1ms.run` 的 manifest digest 与 Docker Hub 一致，以及代理短时错误。当前只读复测证据如下：

| 检查 | 结果 | 证据 |
|---|---|---|
| Docker / Harbor | Docker server 29.8.2；Harbor 0.25.0；Compose v5.6.0 | 现场命令输出 |
| Docker DNS | 临时 Debian 容器使用 `192.168.65.7`；容器 `getent` 可解析 `deb.debian.org`，但宿主机直接向 `192.168.65.7` 与 `114.114.114.114` 查询曾返回 `198.18.0.8`，而 `100.100.100.100` 返回 Fastly CNAME；DNS 路径存在分流差异，尚不足以证明单一根因 | `docker run ... cat /etc/resolv.conf; getent ahostsv4`；宿主机 `dig` 矩阵 |
| Debian apt，容器直连 | 5/5 次 `apt-get update` 成功 | 临时 `debian:bookworm-slim` 容器探测；每次约 3–5 s |
| Debian apt，经 `host.docker.internal:7897` | 5/5 次成功 | 同上，注入 HTTP(S)_PROXY；每次约 3–5 s |
| 宿主机显式直连 | Debian、Aliyun、USTC、Ubuntu 官方、Ubuntu Aliyun、GitHub 各 1/1 HTTP 200；使用 `env -u ...` 与 `--noproxy '*'` | 持久证据 `evidence/harbor-remediation-20261027/network-direct-*.log` |
| 宿主机显式代理 | 同一目标各 1/1 HTTP 200，经 `127.0.0.1:7897` | 持久证据 `evidence/harbor-remediation-20261027/network-proxy-*.log` |
| Debian 包元数据 | 官方、Aliyun、USTC 的 `curl` 包版本均为 `7.88.1-10+deb12u15`、amd64 | Packages.xz 解压比对 |
| Docker 镜像 digest | W13B 五个任务镜像的本地 RepoDigest 与镜像服务返回值一致，均 amd64 | `docker image inspect`、`docker buildx imagetools inspect` |
| 历史故障 | W13A verifier 曾出现 DNS failure 和单包 502；Gateway 还出现 502/503 三连 burst | W13A verifier logs、metering、`EXECUTION_LOG_TB2_W13.md` |

### 4.1 新旧环境差异

| 项目 | 原始/问题路径 | 已验证方案与差异 |
|---|---|---|
| TB2 amd64 镜像拉取 | Docker Hub 单镜像约 6m15s–26m，可能超过 Harbor 默认 600 s build timeout | 从 `docker.1ms.run` 拉取后按官方 tag retag；单镜像约 8–110 s，manifest digest 相同；W13B 使用本地缓存，未改变镜像内容 |
| Docker 镜像备用源 | `edynasty.asia:38082` 曾出现 `unknown blob`；不能作为首选 | `docker.1ms.run`、`docker.1panel.dev` 对已核验镜像返回同 digest；仅保留为手工预拉取方案，不改 daemon 配置 |
| Debian/Ubuntu 软件源 | 官方源在历史 burst 中出现 DNS/502，导致 Verifier 依赖未安装 | 官方、Aliyun、USTC 当前均 200，包元数据一致；不替换官方 task/Verifier 源，避免改变官方环境。复测沿官方源执行，若再失败按 INFRA 留证 |
| Provider gateway | 502/503 burst 在 Core 两次 retry 内结束 run | 未改 Provider/Core；复测按预注册规则单独重跑。延长 Core 重试属于生产代码变更，留在 backlog，不在本批次实施 |

### 4.2 选定的最小整改

采用“官方 task 内容 + digest 校验后的本地镜像缓存 + 原始 Harbor 参数”的方案。它实际解决了已观察的 Docker Hub 拉取超时风险，不改变基础镜像 bytes、Verifier、评分器或任务定义。Debian/Ubuntu 镜像只做旁路对照，不写入 task 文件；当前没有证据支持全局替换源能消除随机 DNS/代理 burst。Gateway 与 Verifier 的瞬时失败通过预注册的独立复测处理，不以修改 Core 生产代码掩盖。

决定：不改官方 Debian/Ubuntu 源，不改 Docker daemon，不改评分器。环境发生实质变化的复测只使用新 job 名称，并在报告中同时列出版本、digest、代理、DNS 和包版本。

## 5. 原始结果与复测

W13A 与 W13B 原始目录已冻结并完成 collector 导出：W13A 为 24/24 trials、raw mean 11/24、1 个 Harbor error；W13B 为 15/15 trials、raw mean 7/15、0 errors、pass@2=0.666667。原始合计为 18/39 reward=1；原始 reward 保持不变。

复测按预注册 ledger 只替换 INFRA slot：10 个非 ENV replacement slot 由 10 个独立 trial 填满；`pytorch-model-cli` 的 ENV exclusion 不产生 replacement。两批复测分别为 `tb2-w13r-20261027`（7/7，6/7 solved）和 `tb2-w13r-20261027b`（3/3，3/3 solved），均 0 Harbor errors。9 个 replacement reward=1；`tune-mjcf__bpHTZMU` reward=0，CTRF 为 3 passed/1 failed，分类为 MODEL，保留为原 INFRA slot 的复测结果，不改写原始 reward。逐 slot 映射、时间、token、工具、Verifier 结果和失败原因见 `evidence/harbor-remediation-20261027/`。

最终 ledger 计数为 27 个 reward=1；`pytorch-model-cli` 按预注册规则作为 ENV exclusion 单列，不进入可比主结论。逐题原始、rerun、ledger-replaced 和公开 Pi/OMP/OpenCode/SpecPi 对照见 `evidence/harbor-remediation-20261027/per_task_vs_baseline.csv`。

复测命令保持官方 Harbor 原生路径；实际执行分为两个新 job，避免同一 trial 或结果跨 slot 复用：

```sh
cd /Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench
PYTHONPATH=$PWD/adapters env -u all_proxy -u ALL_PROXY harbor run \
  --path ./tb2/terminal-bench \
  -i configure-git-webserver -i extract-elf -i multi-source-data-merger \
  -i sanitize-git-repo -i sqlite-with-gcov -i tune-mjcf -i vulnerable-secret \
  --agent lcx_agent:LcxAplus -n 1 -k 1 --timeout-multiplier 2 \
  --ae http_proxy=http://host.docker.internal:7897 \
  --ae https_proxy=http://host.docker.internal:7897 \
  --ae HTTP_PROXY=http://host.docker.internal:7897 \
  --ae HTTPS_PROXY=http://host.docker.internal:7897 \
  --ae no_proxy=host.docker.internal,localhost,127.0.0.1 \
  --ae NO_PROXY=host.docker.internal,localhost,127.0.0.1 \
  -o ./jobs --job-name tb2-w13r-20261027 -y

PYTHONPATH=$PWD/adapters env -u all_proxy -u ALL_PROXY harbor run \
  --path ./tb2/terminal-bench \
  -i multi-source-data-merger -i sqlite-with-gcov -i vulnerable-secret \
  --agent lcx_agent:LcxAplus -n 1 -k 1 --timeout-multiplier 2 \
  --ae http_proxy=http://host.docker.internal:7897 \
  --ae https_proxy=http://host.docker.internal:7897 \
  --ae HTTP_PROXY=http://host.docker.internal:7897 \
  --ae HTTPS_PROXY=http://host.docker.internal:7897 \
  --ae no_proxy=host.docker.internal,localhost,127.0.0.1 \
  --ae NO_PROXY=host.docker.internal,localhost,127.0.0.1 \
  -o ./jobs --job-name tb2-w13r-20261027b -y

python3 collect_w13.py jobs/tb2-w13a > jobs/tb2-w13a/trials.jsonl
python3 collect_w13.py jobs/tb2-w13b > jobs/tb2-w13b/trials.jsonl
python3 export_w13_evidence.py --jobs-dir jobs --out evidence/harbor-remediation-20261027
```

这些命令只能在 W13B `finished_at` 非空后执行；任务、Verifier、评分器、Core binary 和冻结协议均未修改。

## 6. 公开基线可比性

沿用 W13 预注册披露：公开 Pi/OMP/OpenCode/SpecPi 数据不是同环境 A/B；主差异包括宿主架构、代理/Provider endpoint、reasoning effort、timeout multiplier、并发、attempt pooling 和 harness 版本。SpecPi widened aggregate：Pi 134/195、SpecPi 62/77、Oh My Pi 84/116、Claude Code 50/77、OpenCode 29/39、DeepSeek Harness 21/38；逐题数据见 `bench/public/PUBLIC_BASELINES.md` 第 119–156 行。逐题比较只用于历史对照，不能解释为因果性能结论；LC 的 ENV/INFRA trial 必须同时报告原始 reward 与排除/复测口径。
## 6.1 问题清单与优先级

| 优先级 | 项目 | 证据 / 处理 |
|---|---|---|
| P1 | Provider 502/503 burst 使未解决 trial 提前终止 | W13A 四个三连 burst，W13B 一组三连 burst；复测后仍需在 Provider/Core 边界实现有界可用性重试并补 conformance test，当前只登记，不改生产代码。 |
| P1 | Verifier 依赖下载受 DNS/上游单包 502 影响 | 5 个 W13A `tests=0` trial；先以原始日志 + 独立复测隔离，评估可复现缓存/预拉取但不得改官方评分环境。 |
| P1 | `finish_reason=length` 被完成判定，plain-text DSML 被当 final | W13A 4 个截断/1 个 DSML stop，W13B 已出现 1 个截断；对应 `TODO.md` 的 LOOP-001 / PROVIDER-003，需独立 Core 修复与回归，不在基准批次内热修。 |
| P2 | Harbor timeout artifact interpretation | `cobol-modernization__h9F6X5v` 的 `AgentTimeoutError` 后缺少 `exit.txt` 是 adapter 在取消 `docker compose exec` 后的预期结果，`.missing` sidecar 已记录；不作为 Harbor 缺陷。实际工具缺口是 collector 未读取 `sessions.error_json`，应作为后续可观测性改进。 |
| P2 | metering 共享日志的批次聚合 | 当前按完整 trial suffix 关联可用；需在最终报告生成脚本中明确 job/arm 过滤，避免跨批次误归因。 |

## 6.2 LC 与公开基线逐题口径

公开 SpecPi widened 的六组逐题数据见 `bench/public/PUBLIC_BASELINES.md` 表 A；LC W13A 原始 11/24，W13B 原始 7/15，合并原始 18/39。W13A 按任务为 cobol-modernization 2/3、configure-git-webserver 0/3、fix-git 3/3、prove-plus-comm 3/3、pytorch-model-cli 0/3、raman-fitting 0/3、sanitize-git-repo 2/3、vulnerable-secret 1/3；W13B 的完整逐 trial、分类和 replacement ledger 以 `evidence/harbor-remediation-20261027/` 为准。每个 INFRA/ENV trial 保留 raw reward、分类和复测 reward 三列。

公开基线只有聚合和逐题 solved/attempts；没有同模型的 per-attempt wall time、tool calls、trajectory。因此 LC 的 token/tool/time 数据用于 LC 内部诊断，不能与公开基线做逐项因果比较。

## 7. 待完成

- [x] 活动批次自然完成并冻结原始目录。
- [x] 逐 trial 分类 Agent / Verifier / Provider / Harness / ENV（W13A、W13B 与两批复测）。
- [x] 读取并实测 Docker DNS、HTTP(S) 代理、容器到外网、Debian/Ubuntu 源；选定最小整改且不改官方评分环境。
- [x] 完成独立 Harbor-native 复测批次，和原始成绩分开；10/10 非 ENV replacement slots 已闭合。
- [x] 写入逐题 LC/公开基线表、失败轨迹、复现命令和优先级。
- [x] 明确未验证项与 Opus 5.5 审核输入；本报告不宣告任何产品 Gate。


