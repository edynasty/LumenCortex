# LC Harness 调查与修复报告 — TB2 W13 证据（2026-10-27）

**Benchmark Status: NOT RUN.** 本报告只使用已有离线证据（W13 Harbor 结果、metering proxy 原始流、Core 状态库、
本地 sub2api 源码与只读 usage 记录、DeepSeek 公开文档），没有发起任何模型请求、Harbor 任务或回放。

关联文档：`bench/EVAL_PLAN_TB2_DUAL_BASELINE.md`（评测方案）、`bench/baselines/BASELINE_A_DEEPSEEK_OFFICIAL.md`、
`bench/baselines/BASELINE_B_SPECPI_HARNESS.md`、`bench/RESULTS_TEMPLATE_TB2_DUAL_BASELINE.md`、`TODO.md`
（LOOP-001、PROVIDER-002/003/005/006、BENCH-001）。

## 0. 结论

| 问题 | 归属 | 可信度 | 处理 |
|---|---|---|---|
| 被服务端中途切断的流被当作完整响应，其中的工具调用被执行（PROVIDER-005） | **LC 缺陷**（Provider 适配层） | 高：原始流 + Core 状态库逐条对上 | 已修复 `da9c5bd` |
| 输出达到 token 上限（`finish_reason=length`）且无工具调用的回复被当作最终答案，run 以 `completed` 结束（LOOP-001） | **LC 缺陷**（Agent Loop 停止条件） | 高：5 个 W13 FAILED trial 均如此结束 | 已修复 `ff19b89` |
| 模型在 `reasoning_content` 内输出 DSML 工具标记、泄露特殊 token，未退出 thinking | **上游模型行为** | 中：20/1,074 响应；未在官方端点复现 | 不改 LC；诊断实验已设计（方案 §7，NOT RUN） |
| `content` 与 `reasoning_content` 逐字节相同 | **网关转换**（sub2api reasoning-only fallback） | 中：行为与源码一致；具体是哪一跳网关未可观测 | 不改 LC；记录在 PROVIDER-003 |
| 流内 `upstream_error` 事件、约 10 秒的 502/503 突发 | **上游服务 / 网关可用性** | 高 | 切断流现在按 availability 重试；突发窗口问题保留为 PROVIDER-002 |
| temperature 过低导致异常 | **LC 侧无依据；上游未排除** | 中：LC 的 W13 请求体都没有 temperature（LC 也没有该配置项）；网关或上游推理后端会不会自己加一个默认温度，从客户端看不到；DeepSeek 说 thinking 模式忽略 temperature，但这只是官方 API 文档的说法，没有在本路由上验证 | 无代码修改；第 1 阶段（G0 vs G1）只能隔离路由因素，不能隔离温度，所以不单独设计温度矩阵 |
| DSML 解析、`reasoning_content` 回传、工具参数校验 | **W13 证据中未发现 LC 缺陷** | 高（范围限于 arm A+ 的 1,074 个流） | 无修改 |
| 独立审查发现（2026-10-31，§10）：`insufficient_system_resource`/`aborted` 被当作完整回复；`length` 时参数为完整 JSON 对象的最后一个调用仍会执行；`output_limit` 抢在完成门禁纠正之前结束 run；schema 文件读取无界、`$ref` 遍历不确定 | **LC 缺陷**（Provider 适配层、Agent Loop、检查输入） | 高：审查人的本地协议模拟复现，修复前失败、修复后通过 | 已修复 `f4a2c58`、`ecd6a92`、`4bd4b02` |

没有证据表明需要在 LC 里加字符串修补或协议猜测：在这 1,074 个流中，DSML 没有出现在 `tool_calls` 参数里，LC 的解析也
没有观察到错误。这个结论不能外推到其他路由或模型版本。

## 1. 证据与方法

| 来源 | 内容 |
|---|---|
| `bench/jobs/tb2-w13a`、`tb2-w13b` | 39 个 W13 trial 的 Harbor `result.json`、verifier 日志、Core 状态库（`agent/state/**/lumencortex.db`）、`trials.jsonl` |
| `bench/jobs/tb2-w13r-20261027*` | W13 INFRA 重跑（仅用于协议统计的分母） |
| `bench/jobs/metering/proxy.jsonl` + `bodies/` | 每个请求的请求体和完整 SSE 响应流；arm A+ 共 1,074 个 200 响应流（a/b 作业 864 个） |
| `~/IdeaProjects/learn/sub2api@45df23af2` | 本地网关源码；运行镜像 `sub2api-local:0.2.19-oauth-banner-v68`（`sha256:2f6af2f72454`） |
| sub2api `usage_logs`（只读查询） | 账号 42 的 `deepseek-v4.1-flash` 请求：入口 `/v1/chat/completions`，上游 `/v1/responses`，base URL `https://ai.edynasty.asia` |
| `bench/analysis/analyze_tb2.py`、`echo_check.py` | 离线统计脚本，可复现本文所有计数 |

W13 a+b 汇总（`python3 bench/analysis/analyze_tb2.py --arm A+ bench/jobs/tb2-w13a bench/jobs/tb2-w13b`）：
39 trial，18 solved（0.462，Wilson 95% 0.316–0.614）；分类 SOLVED 18 / FAILED 10 / INFRA_VERIFIER 8 /
INFRA_PROVIDER 3；pass@1 0.462、pass@3 0.692。单 trial 最大 prompt 131,989 token（proxy usage；窗口 600K）。
Core 状态库中 39 个 trial 的 862 个 step 记录（W13A/B 的 `agent_steps.json.window`），`dropped` 都是 0，LC 估算的
最大窗口为 141,796 token。也就是说，W13 中没有发生窗口裁剪（LC 没有单独的 compaction 机制，裁剪就是它的上下文
压缩），因此 W13 的失败不能归因于裁剪或 compaction。但这不能说明十几万 token 的长上下文本身不影响模型质量，
这一点没有测量。

10 个 FAILED trial 的归因：

| Trial | 标记 | 归因 |
|---|---|---|
| raman-fitting__j88UAtk、raman-fitting__2YiHjqv、configure-git-webserver__ByarrX3、pytorch-model-cli__946HcD5、tune-mjcf__phxPuSo | 最后一次回复 `length` 且含 DSML | LOOP-001：截断的思考被当作 `final` |
| cobol-modernization__WWMWyA2、tune-mjcf__3mForem | 最后一次回复 `stop`，`content == reasoning_content` 且含 DSML | 上游模型 + 网关 fallback（PROVIDER-003） |
| pytorch-model-cli__WefCCaP（5 个切断流）、configure-git-webserver__8A3Szjo（1 个） | 切断流 | 任务失败；切断流污染了上下文（§3）。pytorch-model-cli 在 W13 中按 ENV 排除 |
| raman-fitting__mFpm2Dv | 无 | 任务失败 |

LOOP-001 修复只会让这 5 个 run 以 `interrupted/output_limit` 结束，而不是 `completed`；它本身不会让任务通过。
修复后的成功率是否变化，只能由方案中的 `LCpost` 对照 `LCpre` 测出（NOT RUN）。

## 2. DSML 与 reasoning/content

统计范围：arm A+ 的 1,074 个 200 响应流。

- 含 DSML 标记的响应 20 个，**全部位于 `reasoning_content`，在这 1,074 个流中没有一个出现在 `tool_calls` 参数中**。
- 其中 11 个仍以一个正常解析的工具调用结束；1 个是切断流；8 个的 `content` 中也含 DSML。
- 这 8 个的 `content` 与 `reasoning_content` **逐字节相同**（6 个 `length`、2 个 `stop`）。所有响应中
  `content == reasoning_content` 共 9 个（6 `length` + 3 `stop`）。
- 14 个含泄露的特殊 token（`<｜begin▁of▁sentence｜>`、字面 `</thinking>`、双写 `｜｜DSML｜｜`）。
- 回传：805 个带 reasoning 的响应，在下一次请求中 805 个逐字节回传（`echo_check.py`）；224 个无 reasoning
  的响应在下次请求中也无回传。LC 满足 DeepSeek「工具场景必须回传 `reasoning_content`」的要求（Baseline A §4.5）。

解释：

1. 模型在思考中写出了 DSML 工具调用（DeepSeek V4.1 的工具协议，Baseline A §4.6、§8.5），但没有退出
   thinking，所以服务端没有把它解析成 `tool_calls`。现有证据指向模型/推理服务的行为，而不是 LC 的解析问题；
   路由（网关）是否参与，要等方案 §7 的诊断。
2. sub2api 的 `synthesizeChatReasoningFallbackMessage`（`backend/internal/pkg/apicompat/chatcompletions_responses_bridge.go:1945`）
   在一个流只有 reasoning、没有正文也没有工具调用时，把 reasoning 全文复制为可见正文。观察到的
   `content == reasoning_content` 正是这一行为的输出。本地网关之后还有 `ai.edynasty.asia` 一跳，具体哪一跳执行了
   fallback 从客户端无法观测 `[INFERENCE: 本地 sub2api 的 Chat Completions→Responses 桥是最可能的位置]`。
3. LC 收到的是一个合法的、无工具调用的 `stop` 回复，按现有语义把它作为最终答案。要拒绝它只能依据文本内容
   判断，这属于针对某个网关的启发式规则，所以不改（PROVIDER-003，P3）。

是否由路由引起，还是模型本身在这些 prompt 下就会这样，需要做方案 §7 的回放诊断（G0 当前路由 vs G1 DeepSeek
官方 API）才能确定。

## 3. 切断流（PROVIDER-005）

W13 a/b 中 16 个响应流以流内事件 `data: {"error":{"message":"Upstream request failed","type":"upstream_error"}}`
结束，既没有 `finish_reason` 也没有 `[DONE]`。修复前，OpenAI 兼容适配器把已收到的 chunk 当作完整响应返回。
将流中的 tool call id 与 Core 状态库中的 tool 结果逐条对照：

| 结果 | 数量 | 例子 |
|---|---:|---|
| 流中工具调用总数 | 20 | 分布在 14 个流中 |
| 参数被截断（不是 JSON 对象），返回 invalid-argument 错误，写入 transcript | 15 | `apply_patch` ×13、`note_write` ×2 |
| 参数完整，被执行 | 5 | `shell` ×2（cobol-modernization__W6sfRVm #528；prove-plus-comm__JSG5wMv #606，包含 `rm -rf /tmp/...`）、`apply_patch` ×1（执行后失败）、`note_write` ×2（被校验拒绝） |
| 无工具调用、只有部分正文的切断流成为 run 的 `final`，状态 `completed` | 1 | sanitize-git-repo__8rVxCip #493（2,122 字符） |
| 无工具调用也无正文的切断流，run 以 `empty_response` 结束 | 1 | sanitize-git-repo__rXFrGHS #301 |

这违反了「不能伪造工具调用、不能污染上下文」的要求：一个未完成的响应中的命令被真实执行，部分参数被当作
模型的完整输出回填。修复后，这类流是 availability 错误，由适配器现有的重试机制重新请求；已收到的工具调用
不会执行。

Anthropic 适配器存在同类缺陷（没有 `message_stop` 的流也会返回已收到的 block），同时修复。

## 4. 修复

分支 `LumenCortex-Core` `cog/harness-fix`（基于 `cog/r1a-fix@e562be1`，worktree `wt/core-harness`），2026-10-28 合入 `cog/r1a`（§9）；未推送：

| 提交 | 改动 | 原因 |
|---|---|---|
| `da9c5bd` fix(provider) | `provider/openai/wire.go`：解析流内 `error` 事件（`streamError`），记录首个非空 `finish_reason`；`provider/openai/client.go`：流在既无 `finish_reason` 也无 `[DONE]` 时结束 → `errStreamCut`；`provider/anthropic/client.go`、`wire.go`：没有 `message_stop` → `errStreamCut`；`protocol.FinishLength` 常量；`factory_test.go` 的流式测试服务改为返回 SSE | §3 |
| `ff19b89` fix(agent) | `internal/agent/loop.go`：无工具调用且 `FinishReason == protocol.FinishLength` 时，以 `interrupted` / `output_limit` 结束（交互式会话也一样），回复仍保存在 transcript 中；`types.go` 新增 `KindOutputLimit`；`docs/agent-runtime.md` 补充退出表和流完整性规则 | §1 |
| `50aa96d` fix(agent) | `internal/agent/loop.go`：`length` 结束且带工具调用时，最后一个调用如果没有参数（`{}`），返回工具错误而不执行；`exit_test.go` 三组边界；`docs/agent-runtime.md` | §9.1（2026-10-28 合入前审查发现）；规则已被 `ecd6a92` 取代（§10） |
| `f4a2c58` fix(provider) | `provider/openai/wire.go`：`finish_reason` 为 `insufficient_system_resource` 或 `aborted`（DeepSeek 文档：生成被中断）时，流式和非流式都按 availability 错误处理 | §10 R1 |
| `ecd6a92` fix(agent) | `internal/agent/loop.go`：`length` 结束时按位置拒绝最后一个调用，不看参数；`MaxToolCallsPerStep` 截掉尾部时保留的调用全部执行；无工具调用的截断回复只在本来会 `completed` 时才以 `output_limit` 结束，完成门禁的纠正照常生效 | §10 R2、R3、R7 |
| `4bd4b02` fix(checks) | `internal/evaluator/inputs.go`：schema 文件只在是工作区内普通文件且不超过 1 MiB 时读取；`$ref` 按排序遍历，最多 64 个文件 | §10 R4、R5 |

设计边界：

- 两处都在协议边界上判断，依据的是协议本身的结束标记（`finish_reason`/`[DONE]`/`message_stop`、规范化后的
  `length`），不检查模型文本，不针对 DeepSeek 或 Benchmark。
- 切断流按 availability 分类，走现有的 2 次重试；没有增加新的重试路径。适配器内部的重试在 Core 中不留痕迹，
  记录为 PROVIDER-006（P3）。
- `output_limit` 不自动续写：续写会把新的模型行为带入评测，而且是否需要续写属于策略决定，不是缺陷修复。
- Desktop 对未知 kind 显示原始字符串（`session-status.ts` `kindLabel`）。`output_limit` 的 zh-CN/en 文案已加入现有
  kind 表（Desktop `4a6c2f3`），见 §9。

先失败后通过（在 `e562be1` 的生产代码上运行新测试）：

| 测试 | `e562be1` | `ff19b89` |
|---|---|---|
| `provider/openai` `TestTruncatedStreamIsAvailability` | FAIL | ok |
| `provider/anthropic` `TestStreamWithoutMessageStopIsAvailability` | FAIL | ok |
| `provider/openai` `TestStreamEndsAtFinishReasonOrDone`（正向对照） | ok | ok |
| `internal/agent` `TestEveryExitPathStoresStatusAndCause/reply_cut_at_the_output_limit`、`/conversation_turn_cut_at_the_output_limit`（基线上只加入 `KindOutputLimit` 常量，不改 loop） | FAIL | ok |

## 5. 采样参数

- LC 的所有 W13 请求只带 `{"max_tokens": 32768, "model": "deepseek-v4.1-flash"}`（加 messages/tools/stream）：
  **不发送 temperature、top_p、reasoning_effort、thinking**。LC 配置也没有 temperature/top_p 选项。
- DeepSeek 文档：thinking 模式默认开启；该模式下 temperature「has no effect」，不报错（Baseline A §4.2、§8.1，S12/S13）。
  `top_p` 在 thinking 模式下有效（有效范围 0.95–1.0）。
- 判断：LC 自己没有发送过低的温度，所以修改 LC 的温度不能算修复。但「上游没有温度问题」没有被证明：本地桥之后
  的 `ai.edynasty.asia` 和推理后端实际用什么采样参数看不到，DeepSeek 关于忽略 temperature 的说明也只适用于官方
  API。这一项保持未确定，要等第 1 阶段（G0 vs G1）结果再决定是否值得做参数实验。
- `reasoning_effort` 是实际起作用的参数（low=50 / high=75 / max=100，默认 high）。官方 agentic 评测使用 max；
  LC 不发送；如果中间网关和上游也不注入，就运行在默认值（Baseline A §8.2）。网关 usage_logs 中 LC 请求的 effort
  字段为空，与此一致，但上游最终用的 effort 看不到。
- 本地桥 `apicompat/chatcompletions_to_responses.go:53-56` 只对非 reasoning 模型透传 temperature/top_p；
  `reasoning_effort` 映射为 `reasoning.effort`。
- 结论：不修改 LC；如果以后需要设置 effort/top_p，应作为按模型的配置，不能作为 Harness 默认值。

## 6. 其他 Harness 检查（未修改）

| 检查点 | 发现 | 处理 |
|---|---|---|
| 工具参数校验 | 非对象参数从不执行，返回工具错误（`TestInvalidToolArgumentsAreKeptAsStringAndAnsweredWithToolError`） | 正确 |
| `reasoning_content` 回传 | 805/805 逐字节 | 正确 |
| 可用性重试 | 2 次重试、退避上限 5 秒；W13 中每次网关突发约 10 秒内 3 个错误，均导致 run 失败 | PROVIDER-002（P2，已存在） |
| 重试可见性 | 适配器内重试不写入 Core | PROVIDER-006（P3） |
| 文本形式的工具调用被当作最终答案 | 见 §2 | PROVIDER-003（P3） |
| Context / 窗口裁剪 | 862 个 step 中 `dropped` 都是 0；最大 prompt 131,989 / 600K | W13 中没有发生裁剪，失败不能归因于它；长上下文本身对质量的影响没有测量 |
| 空回复、max_steps、取消 | 已有退出类型和测试覆盖 | 无修改 |

## 7. 静态验证（已执行）

在 `cog/harness-fix` 上执行：

- 2026-10-27，`a03bf89`/`ff19b89`（同一棵树，只改了提交说明）：`go build ./...`、`go vet ./...` exit 0；
  `go test -count=1 ./...` exit 0（274 秒）；`go test -race -count=1 ./provider/... ./internal/agent/ ./protocol/` ok；
  `GOOS=windows GOARCH=amd64 go build ./...` ok。
- 2026-10-28，`50aa96d`：`go build ./...`、`go vet ./...` exit 0；`go test -count=1 ./...` exit 0（156 秒）；
  `go test -race -count=1 ./provider/... ./internal/agent/ ./protocol/` ok；`GOOS=windows GOARCH=amd64 go build ./...` ok。
  `gofmt -l` 对 `e562be1..50aa96d` 改动的 Go 文件无输出（仓库里另有 27 个文件在 `e562be1` 和 `4968ea8` 上就已经
  不符合当前 gofmt，本轮没有动它们）。
- §4 和 §9.1 的先失败后通过测试。
- Desktop（`4a6c2f3`）：`tsc --noEmit` exit 0；`vitest run` 76 个文件 411 个测试全部通过；`npm run build` 成功；
  `go build ./...`、`go vet ./...`、`go test -count=1 ./...` 全部 ok。新的 kinds 测试在旧映射上失败（2 failed），
  加上映射后通过。
- 评测脚本：Python 文件 `py_compile` 通过，`sh -n run_controlled.sh` 通过；未设置 `EVAL_AUTHORIZED` 时
  `run_controlled.sh` 以 exit 2 拒绝运行，`dsml_replay.py --execute` 拒绝运行，默认 dry run 只打印选择和费用估算；
  `PREREGISTRATION_TB2_W13.sha256` 中冻结的 W13 文件 `shasum -c` 全部 OK。

这些都是单元测试、编译和静态检查，不是 Benchmark 验证。

## 8. 待验证（需要正式评测）

| 问题 | 决定方式 |
|---|---|
| 两项修复是否改变 LC 成功率 | 方案 §5：`LCpost` vs `LCpre`，同场次对照（NOT RUN） |
| DSML/reasoning-as-content 由路由还是模型引起 | 方案 §7 第 1 阶段：G0 当前路由 vs G1 DeepSeek 官方 API（NOT RUN） |
| top_p / effort 是否能减少该现象 | 方案 §7 第 2 阶段，仅在第 1 阶段满足触发条件时执行（NOT RUN） |
| `deepseek-v4.1-flash` 别名实际对应哪个 checkpoint | 客户端无法观测；G1 使用官方 `deepseek-flash`（Baseline A §2.1） |
| LC 与 Pi/OpenCode/SpecPi 的差距 | 方案 §5 受控对照（NOT RUN） |
| 修复合入 R1a 主线 | 已在本地合入 `cog/r1a`（§9.4、§10）；独立审查与复审见 §10；未推送 |

## 9. 合入前收尾审查（2026-10-28）

范围：只审查 §4 修复的边界和合入，不重新调查架构。没有发起任何模型请求或 Benchmark。

### 9.1 边界条件

| 问题 | 结论 | 依据 |
|---|---|---|
| OpenAI 兼容流的完整性 | 收到第一个非空 `finish_reason` 或 `[DONE]` 才算完整；流内 `error` 事件在任何位置都会让本次尝试失败 | `provider/openai/client.go` `readStream`、`wire.go` `apply`；`TestTruncatedStreamIsAvailability`、`TestStreamEndsAtFinishReasonOrDone` |
| `finish_reason` 之后、`[DONE]` 之前断开 | 作为完整响应接受：按协议，`finish_reason` 表示 choice 已经结束；后面最多还有一个 usage chunk，丢了只影响用量统计 | 同上 |
| Anthropic 流的完整性 | 只有 `message_stop` 才算完整；`error` 事件会让本次尝试失败 | `provider/anthropic/client.go` `readStream`、`wire.go:634`；`TestStreamWithoutMessageStopIsAvailability` |
| 流被切断时的工具调用 | 不会执行任何一个：`readStream` 在调用 `builder.result()` 之前就返回 `errStreamCut`，loop 收不到这些调用 | 同上 |
| `length` 且含 `tool_calls` | **（已被 §10 推翻）** run 继续。参数是完整 JSON 对象的调用会执行（对象的前缀不可能也是合法对象）；参数被截断的调用返回 invalid-argument 错误，不执行。**发现的缺陷：** 如果截断正好发生在最后一个调用的名称之后、第一个参数字节之前，适配器会把空参数补成 `{}`，这个调用就像一个完整的无参调用一样被执行。已修复为：`length` 结束时，最后一个调用没有参数就返回工具错误（`50aa96d`）。同一个无参调用在 `stop` 下照常执行。「完整对象 = 已写完」这个前提只在参数是模型原始输出时成立 | `TestOutputLimitReplyWithToolCallsRunsOnlyFinishedCalls`（3 组），在 `ff19b89` 的 loop 上 `cut_before_the_first_argument` 失败，在 `50aa96d` 上通过 |
| Provider 重试会不会重复执行 Shell / 文件修改 | 不会。重试（`Complete` 内最多 2 次，以及 Core 的 failover）都发生在拿到响应之前；工具只在响应写入 transcript（`AppendAnswered`）之后执行，每个 call ID 在一个 step 里最多执行一次 | `provider/openai/client.go:159-194`、`internal/agent/loop.go`（`send` → `AppendAnswered` → 工具循环） |
| 已成功的工具结果会不会因为重试被重复提交 | 不会。重试只重发模型请求，不重新写工具结果；transcript 中出现重复结果会被 `window.Groups` 判为 `ErrUnpaired`，run 以 `transcript_unpaired` 失败，而不是带着重复结果继续 | `internal/window/window.go:64-98`、`loop.go:206-209` |
| 工具执行途中崩溃 | 下一次 run 不会自动重放这个调用。**（更正，§10）** 不是 `transcript_unpaired`：`Reconcile` 为没有结果的调用补写 `tool_interrupted` 结果，并把已登记的执行标为 `outcome_unknown`，调用不会再次执行。这是 R1a 已有的语义，本轮没有改 | `internal/session/reconcile.go:24-35`、`:98-123`；`internal/agent/execution.go` `postExecution` |
| `interrupted/output_limit` 会不会被改成 `completed` | 不会。只有 loop 在接受最终答案时写 `completed`（`loop.go` 中唯一的 `StatusCompleted` 退出）；stale 恢复只处理 `running` 行；`runtime/session_import.go` 的 `completed` 只用于导入外部会话。用户在 Desktop 里「继续」一个 interrupted 会话会开始新的 run，新 run 自己完成，这是正常转换 | `grep StatusCompleted`、`exit_test.go` |

没有加入任何针对 DeepSeek、DSML 或 Benchmark 的规则：判断只依据协议的结束标记、规范化后的 `length`，以及参数是不是完整的 JSON 对象。

### 9.2 Desktop

- 调用方式：Desktop 固定 Core `v0.1.4`，退出原因只从会话的 `error_json.kind` 读取，通过 `session-status.ts` 的
  `kindKeys` → `app-copy.ts` 显示；未知 kind 显示原文。Desktop 的 Go 侧不解析 kind。
- 修改（Desktop `main` `4a6c2f3`）：`output_limit` → `kindOutputLimit`，zh-CN「模型回复达到输出上限」、en「Model
  reply hit the output limit」；kinds 测试加入 `output_limit`。没有改 UI。
- 历史会话：旧会话里没有这个 kind，显示不变；`interrupted` 的图标、「继续」按钮和结果提示都按状态显示，不看 kind，
  所以新 kind 走 interrupted 的已有路径。
- 依赖：Desktop 仍固定 `v0.1.4`。Core 修复只在本地，没有发布 tag，按发布流程不能升级 pin；这条文案提前到位，pin
  升级到包含 `output_limit` 的 Core 版本时就会生效。

### 9.3 版本管理

工作区 `bench/` 不在任何 Git 仓库里。评测方案、双基线文档、调查报告、结果模板、Harbor 适配器和脚本、预注册文件
和公开数据快照已复制到 Core 的 `benchmarks/tb2/`（Core 已有的 benchmark 产物目录），清单见
`benchmarks/tb2/README.md`；`MANIFEST.sha256` 是提交内容的哈希，`ARTIFACTS.sha256` 是没有提交的大文件（W13 原始
job、二进制、>1 MB 的快照）的哈希。

原始 Benchmark 记录没有被改动：W13 job 目录内没有一个 `lumencortex.db` 的 mtime 晚于 2026-10-27 的运行时间。
本次审查用 SQLite 只读模式读取状态库，SQLite 在其中 38 个目录（W13A 24、W13B 14）生成或刷新了 `lumencortex.db-shm`（WAL 共享内存
索引，不含数据）；之后的读取都改用 `immutable=1`。

### 9.4 合入

`cog/harness-fix`（`da9c5bd`、`ff19b89`、`50aa96d` 和 benchmark 文档提交）以 `--no-ff` 合入本地 `cog/r1a`，原始
提交保持原 SHA，作为合并的第二父提交。`cog/harness-fix` 基于 `cog/r1a-fix@e562be1`，所以这次合并也带入了
CHECKS-002 修复链（独立复审 `FIX_REVIEW.md` Addendum C：`VERDICT=PASS` @ `4e35447`；`e562be1` 只改注释）。
G-R1a=PASS 对应的是 `4968ea8`，不覆盖合并后的提交；本轮修复只经过作者审查，没有非作者审查。合并 SHA 和验证结果
见 `TODO.md` 中的 LOOP-001 / PROVIDER-005。未推送，未打 tag，未合入 `main`。

## 10. 独立审查与整改（2026-10-31）

范围：`cog/r1a@bf6748a` 相对 `4968ea8` 的全部变更（含合并带入的 CHECKS-002 修复链、`1e16a52`）。没有发起任何模型请求或
Benchmark。

### 10.1 审查人

新上下文的 `reviewer` 子代理，不是实现者，没有看到实现会话的对话历史；按要求优先尝试了与实现者不同的模型
（`gpt-6.1-sol`），该模型所在的账户池全部限流（429），改用新上下文的 Claude Opus 完成。Owner 已澄清 Gate 只要求
独立性（非作者、新上下文）和可复现证据，不要求特定模型（`OWNER_DECISIONS_R2_O6_20261026.md`）。审查人的探针都是
临时 `zz_review_*_test.go`，用 `httptest` 模拟协议，结束后已删除。

### 10.2 发现与处理

| ID | 级别 | 发现（审查人复现） | 处理 |
|---|---|---|---|
| R1 | P1 | DeepSeek 文档定义的 `finish_reason` `insufficient_system_resource`、`aborted` 表示生成被中断；任何非空 `finish_reason` 都让流算作完整，被中断的文本以 `completed` 结束，其中的工具调用被执行 | `f4a2c58`：流式和非流式都按 availability 错误处理并重试 |
| R2 | P1 | `length` 时，最后一个调用只要参数能解析成完整 JSON 对象就会执行。DeepSeek DSML 的参数由服务端拼装并自动闭合，Anthropic 非流式返回的是已解析的 `input`，所以「完整对象」不能证明调用已写完 | `ecd6a92`：按位置拒绝最后一个调用 |
| R3 | P2 | `ff19b89` 让 `output_limit` 在完成门禁纠正之前就结束 run：有未完成 work unit 时，基线会纠正并继续，`bf6748a` 直接结束 | `ecd6a92`：只在本来会 `completed` 时以 `output_limit` 结束；空内容的截断回复仍直接 `output_limit`（基线在这里也是直接结束：`empty_response`） |
| R4 | P2 | schema 文件用 `os.ReadFile` 直接读取，没有工作区内、普通文件、大小检查：一个 FIFO 会让所有门禁、`SessionChecks`、`CheckApprovalState` 阻塞 | `4bd4b02`：与 `fileDigest` 共用 `regularFileInside`，最多读 1 MiB |
| R5 | P3 | `$ref` 超过 64 个时，map 顺序决定绑定哪些文件，文件不变也会得到不同的 `SetDigest`；单个文件的引用还能让上限被超过（实测 86 个） | `4bd4b02`：排序遍历，严格 64 个 |
| R6 | P3 | here-doc 正文里的脚本名被当作输入（失败方向是拒绝，不是绕过） | 未修改，记入 `TODO.md` |
| R7 | P3 | `50aa96d` 按 call ID 匹配最后一个调用：空 ID 让拒绝失效，重复 ID 会拒绝错的调用 | `ecd6a92` 改为按位置，随 R2 消除 |
| K1 | P2 风险 | 只有 `[DONE]`、没有 `finish_reason` 的流被接受；本地 sub2api 在切断后不会补 `[DONE]`，其他网关会不会补是 [INFERENCE] | 未修改：OpenAI 规范总在 `[DONE]` 之前发 `finish_reason`，但有的兼容服务不发，收紧会把它们全部变成错误；记入 `TODO.md` |
| K2/K3 | P3 | Windows 路径形式的解释器不绑定输入；`stdbuf -o L` 不绑定（FIX_REVIEW N3 已记录） | 未修改，记入 `TODO.md` |

另有两处我在 §9.1 中的结论被审查推翻或更正（已在原行标注）：`length` 下完整对象可执行（R2），以及工具执行中崩溃的
处理（`Reconcile` 补写 `tool_interrupted`，不是 `transcript_unpaired`）。

### 10.3 `output_limit` 下的工具调用语义

需要修改，已修改（`ecd6a92`）：`length` 结束时，最后一个调用无论参数是否完整都不执行，返回工具错误让模型重发；它之前的
调用已经写完（生成已经进入下一个调用），照常执行。没有改成「拒绝全部调用」：这只在 provider 交错输出并行调用时才有必要，
适配器按 index 组装、按顺序输出，拒绝全部只会让每次撞到上限的 run 都没有进展。`stop` 下不变。

### 10.4 先失败后通过

| 测试 | 修复前生产代码 | 修复后 |
|---|---|---|
| `provider/openai` `TestTruncatedStreamIsAvailability/insufficient_system_resource`、`/aborted` | FAIL（`c45f681` 的 `wire.go`） | ok |
| `provider/openai` `TestInterruptedFinishWithoutStreamingIsAvailability`（2 组） | FAIL | ok |
| `internal/agent` `TestOutputLimitReplyWithToolCallsRunsOnlyFinishedCalls/whole_object_closed_by_the_server` | FAIL：`ran ["ls" "rm -rf /tmp/bu"]` | ok |
| `internal/agent` `TestOutputLimitReplyKeepsTheCompletionGateCorrection` | FAIL：1 次请求即 `output_limit` | ok：2 次请求、2 条纠正、`max_steps` |
| `internal/evaluator` `TestSchemaFilesAreBoundedAndDeterministic` | FAIL：86 个输入 | ok |
| `internal/evaluator` `TestSchemaFIFOIsNotRead`（unix） | FAIL：5 s 后仍阻塞 | ok |

门禁（`cog/harness-fix@8b32898`，生产代码与合并后的 `cog/r1a` 相同）：`gofmt -l` 改动文件无输出；`go build ./...`、
`go vet ./...` exit 0；`go test -count=1 ./...` exit 0；`go test -race -count=1 ./provider/... ./internal/agent/
./internal/evaluator/ ./runtime/ ./protocol/` exit 0；`GOOS=windows GOARCH=amd64 go build ./...` 与 `go vet
./internal/evaluator/` ok。

### 10.5 Desktop（DESKTOP-R1A-001）

5 个失败都不是 Core 回归，在 Core `main@3e1c3cc`（T0-S1）上已失败 1 个、在 `4968ea8` 上 5 个全部失败，`bf6748a` 未改变：

| 测试 | 错误 | 根因 | 分类 |
|---|---|---|---|
| `TestOpenWorkspaceRefusesNewerSchemaWithCoreSentinel` | `open <workspace>/.lumencortex/lumencortex.db: no such file or directory` | T0-S1 把状态库移到 `$XDG_STATE_HOME/lumencortex/workspaces/<key>`；测试写死了旧位置 | 测试预期过时（Core 已批准的接口变更） |
| `TestAcceptRecordsAHumanResultAtTheCurrentFingerprint` | `check definition is not approved` | ADR-0017：检查定义经用户批准才生效；Desktop 没有批准入口 | 接口变更，Desktop 缺适配 |
| `TestAcceptOutsideGitIsRefused` | 期望 `ErrNoFingerprint`，得到 `ErrSpecUnapproved` | 同上：批准检查先于指纹检查 | 同上 |
| `TestCheckResultsPageAndArtifactsReadAsText` | `results=[]` | 同上：未批准的检查不运行，没有结果 | 同上 |
| `TestDelegationGrantProposalStartAndDelivery` | 子任务 `interrupted` / `max_steps` | 同上：子任务的必需检查永远不能满足，一直被纠正到步数用完 | 同上 |

修复在 Desktop 分支 `cog/t7-s0-r1a`（不进 `main`）：

- `fbf8fc1`（与 v0.1.4 兼容）：测试隔离 `XDG_STATE_HOME`（原先未隔离，会读开发者环境的状态目录）；schema 测试改为在
  Core 实际放置的位置找到唯一的状态库，旧位置和外部位置都接受。断言不变。在固定的 v0.1.4 上全部通过。
- `06e368c`（只适用于 R1a）：`Runtime.CheckApprovalState`、`Runtime.ApproveChecks` 直通 Core；检查测试的夹具像用户一样批准
  当前检查集合，4 个测试的断言不变；新增 `TestChecksCountOnlyAfterTheUserApprovesTheShownSet` 固定未批准和过期摘要的拒绝。
  `go.mod` 用 `replace` 指向 `../../wt/core-r1a`（执行计划 T7-S0 允许的开发期做法），没有 Wails/UI 暴露（属于 T7-S1）。

Desktop `main` 仍固定 Core `v0.1.4`，没有改动。
