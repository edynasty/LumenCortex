# LumenCortex

[English](README.md) | **简体中文**

**LumenCortex** 是一个带持久认知图、Attention Light、完整 Agent Loop、TUI/CLI、Workflow Contract 与多工具运行时的独立 AI 编程 Agent。

> Graph is Memory. Light is Attention. Decision is Judgment. Think is Deliberation. Agent is Execution.
>
> 图是记忆，Light 是注意力，Decision 是判断，Think 是深度推理，Agent 是执行。

默认入口：

```bash
lumencortex
# 或
lcx
```

<p align="center">
  <img src="docs/assets/architecture/system-architecture.webp" alt="LumenCortex 系统架构" width="100%">
</p>

详细文档见 [docs/README.md](docs/README.md)。

## 当前能力

### 认知运行时

- 持久化 Context Graph
- Evidence / Belief 分离
- Evidence Grade 与 Trust Zone
- Cognitive Git：commit / branch / checkout / merge / conflict / revert / blame / cherry-pick / rebase
- 非破坏式 graph cut / restore / graft
- Attention Light 图传播与 token budget
- Exploit / Explore / Contrarian / Anomaly 多种 Light
- Active Promotion
- Repository 增量 ingest
- SQLite WAL 持久化 Graph / Session / Cognitive Git / Journal / Symbol / FTS5
- FTS5 + Symbol Index 大仓库候选召回
- Source/evidence change transitive invalidation

### 自适应认知控制

Node.js reference runtime 已接入第一版可运行控制面：

- **Decision Layer**：内置 Algorithmic Decision Provider，并支持可选 Jev/Laya-compatible System One provider。
- **Framework Router**：框架拥有最终决策权，决定 Category、是否 Think、Think Effort。
- **Ordered Category Model Chains**：Category 配置有序生成模型链；前一个模型不可用时按顺序尝试下一个。
- **Dynamic Think Effort**：`low / medium / high / max`，根据任务复杂度、证据、重复失败和进展动态计算。
- **Progress Monitor**：归一化失败 signature，识别重复等价失败。
- **Cognitive Trace**：每一步记录 Category / Think / Effort / Retrieval / Decision error / Model candidates。
- **Model Telemetry**：记录每个具体模型的调用次数、失败次数和 EWMA 总延迟；默认不会改变用户配置的 Category 顺序。
- **Persistent Work Units**：支持依赖、required evidence、verification gate 和最终回答门禁；Work Unit 禁止选择模型/provider/Category。
- **Graph Governor baseline**：已实现全局 Analyzer、独立配置的语义 Curator、确定性 Validator、safe tier/archive，以及显式 branch/promotion/canonicalization 和可回滚 Cortex Epoch。
- **Storage tiers**：SQLite schema v2 持久化 hot/warm/cold、访问时间/次数、归档时间和 compaction 状态；safe compaction 只清理派生搜索缓存，不删除图认知。

Jev/Laya **只属于 Decision Layer**，不执行 coding Work Unit，也不会出现在 Category 生成模型链里。

即使没有 Jev/Laya，LumenCortex 仍然可以使用 Algorithmic Decision Path 和当前 CLI 指定的生成模型运行。

### Workflow Contract

Workflow 与 Cognitive Graph 分离：

```text
Facts -> Action -> Route -> Outcome -> Gate
```

支持 Action 工具边界、工具结果 Facts、Final Answer Gate、Human Gate 和持久化恢复。

### 独立 Agent

- 多轮 Agent Loop，每轮移动 Attention Light
- OpenAI-compatible provider adapter
- OpenRouter / Groq / DeepSeek official preset
- 本地 Ollama / vLLM / 其他 OpenAI-compatible endpoint
- Workspace read/search/write/apply_patch/shell 工具
- LSP definition/references/symbols/hover/diagnostics
- MCP stdio + HTTP
- Focused Subagent
- Parallel Sessions
- Persistent resumable Session
- Bounded Working-Set Pager
- Permission policy
- TUI + Chat CLI

## 安装

```bash
npm install -g github:edynasty/LumenCortex
```

初始化：

```bash
lcx init
lcx ingest .
lcx commit "baseline"
```

## 快速开始

### OpenRouter

```bash
export OPENROUTER_API_KEY=sk-or-...
lcx agent "inspect this project, find the bug, fix it and run the relevant tests" \
  --provider openrouter \
  --model openrouter/free \
  --yes
```

### Groq

```bash
export GROQ_API_KEY=gsk_...
lcx agent "fix the failing tests" --provider groq --model openai/gpt-oss-120b --yes
```

### DeepSeek Official

```bash
export DEEPSEEK_API_KEY=...
lcx agent "fix the failure and verify it" --provider deepseek --model deepseek-flash --yes
```

### 本地模型 / vLLM

```bash
export LUMENCORTEX_BASE_URL=http://127.0.0.1:8000/v1
export LUMENCORTEX_MODEL=Qwen/Qwen2.5-32B-Instruct-AWQ
export LUMENCORTEX_API_KEY=dummy
lcx agent "run the tests and repair failures" --provider generic --yes
```

## Cognitive Routing 配置

默认 CLI Agent 会启用认知控制。如果没有配置文件：

- Decision Layer 使用 Algorithmic Decision Provider
- Category 最终使用当前 `--provider / --model`
- 不影响原来的单模型使用方式

默认配置路径：

```text
.lumencortex/cognition.json
```

也可以显式指定：

```bash
lcx agent "..." --cognition path/to/cognition.json --yes
```

完整示例见 [examples/cognition.json](examples/cognition.json)。

简化配置：

```json
{
  "decision": {
    "providers": [
      { "type": "laya", "baseURL": "http://127.0.0.1:8000" },
      { "type": "jev", "model": "jev-latest" }
    ]
  },
  "categories": {
    "general": {
      "default": true,
      "models": [
        { "provider": "openrouter", "model": "openrouter/free" }
      ]
    },
    "deep": {
      "models": [
        { "provider": "openrouter", "model": "your-strong-reasoning-model" },
        { "provider": "groq", "model": "openai/gpt-oss-120b" }
      ]
    },
    "visual-engineering": {
      "models": [
        { "provider": "openrouter", "model": "your-visual-model" },
        { "provider": "openrouter", "model": "openrouter/free" }
      ]
    }
  }
}
```

核心边界：

```text
Jev / Laya
    -> Decision Layer
    -> 只负责判断

Framework Router
    -> 最终决定 Category
    -> 最终决定是否 Think
    -> 动态决定 Think Effort

Category Model Chain
    -> 只放生成 / 推理 / 执行模型
    -> 按用户顺序选择
```

查看内置默认配置：

```bash
lcx cognition defaults
lcx governor analyze
lcx governor storage
lcx governor compact --retention-days 30
lcx governor compact --retention-days 30 --yes
lcx governor plan
lcx governor apply plan.json --dry-run
lcx governor apply plan.json --yes
lcx governor apply plan.json --semantic --epoch --yes
```

关闭控制面：

```bash
lcx agent "..." --no-cognition
```

## 常用 Agent CLI

```bash
lcx agent "add pagination to the user API and test it" --yes
lcx chat --provider groq --model openai/gpt-oss-120b --yes
lcx sessions
lcx agent "continue" --session session_xxx --yes
```

常用参数：

```text
--provider openrouter|groq|deepseek|generic
--model MODEL
--base-url URL
--max-steps 24
--max-tokens N
--budget 24000
--recent-rounds 6
--working-chars 120000
--timeout-ms 120000
--tools read_file,code_search,apply_patch,shell
--workflow path/to/workflow.json
--work-units examples/work-units.json
--max-tool-calls-per-step N
--cognition path/to/cognition.json
--no-cognition
--no-auto-promote
--no-ingest
--policy read-only|workspace|full
--session ID
--cognitive-commit
--json
--yes
```

## 权限策略

- `read-only`：只读
- `workspace`：workspace 读写，不允许 host shell 与外部 MCP 写操作
- `full`：workspace + host shell + 外部写工具

未知 policy fail closed。没有 `--yes` 时，write / shell 默认需要交互授权。

## 大仓库代码智能

```bash
lcx index build
lcx search "reserveInventory"
lcx lsp references src/main/java/.../InventoryService.java 42 18
```

SQLite FTS5 + Symbol Index 负责候选召回，Attention Light 负责图传播、关系权重和 token budget。

## MCP

```bash
lcx mcp status
lcx mcp tools my-server
lcx mcp call my-server tool-name '{"arg":"value"}'
```

## Workflow

```bash
lcx workflow validate examples/workflows/verified-code-fix.json
lcx agent "fix the failing tests" --workflow examples/workflows/verified-code-fix.json --yes
```

Human Gate：

```bash
lcx workflow status session_xxx
lcx workflow approve session_xxx release-approval
lcx agent --session session_xxx --yes
```

## Cognitive Graph / Cognitive Git

```text
lcx light <goal> [--budget N] [--multi]
lcx promote <title> <nodeId> [nodeId...]
lcx verify
lcx status
lcx commit <message>
lcx log
lcx branch [name]
lcx checkout <branch>
lcx merge <branch>
lcx revert <commit>
lcx blame <node-or-edge-id> [limit]
lcx cherry-pick <commit>
lcx rebase <branch>
lcx edge cut <edgeId> [reason]
lcx edge restore <edgeId>
lcx edge graft <from> <type> <to> [weight] [reason]
lcx governor analyze
lcx governor storage
lcx governor compact [--retention-days 30] [--yes]
lcx governor plan
lcx governor apply <plan.json> --dry-run|--yes [--semantic] [--epoch]
```

## 持久化

```text
.lumencortex/
├── lumencortex.db
├── lumencortex.db-wal
├── lumencortex.db-shm
├── cognition.json    # 可选
├── lsp.json          # 可选
└── mcp.json          # 可选
```

完整 Session 历史可以持续增长，但模型每轮只得到有限 Working Set。

## 开发与测试

```bash
npm test
npm run benchmark
npm run benchmark:search
npm run smoke:free
```

Node.js 要求 `>= 22.13`，核心运行时保持零 runtime npm dependencies。

## Go Runtime 迁移

Node.js 目前仍是生产 `lcx` 主路径；Go runtime 正在按 bounded-memory parity gate 增量迁移。

详见 [Go runtime migration](docs/go-runtime-migration.md)。

## 当前实现边界

已经实现并有自动化测试覆盖：

- Context Graph / Evidence / Belief
- Attention Light / Active Promotion
- Cognitive Git
- Agent Loop / Workflow Contract
- SQLite WAL / FTS5 / Symbol retrieval
- LSP / MCP / Subagents / Parallel Sessions / TUI
- Provider abstraction / Tools / Resumable Sessions
- Decision Layer baseline
- Algorithmic Decision Provider
- Jev/Laya-compatible System One HTTP adapter
- Ordered Category model chains
- Framework-owned Think decision
- Dynamic Think effort
- Progress Monitor / failure signatures
- Per-step cognitive trace
- Model latency telemetry
- Graph Governor Analyzer / Plan Validator / safe tier+archive executor

仍然不应宣称已经完成：

- Embedding Retrieval：Node 已实现可选 SQLite 持久化向量 + exact cosine + RRF Hybrid；ANN/HNSW/IVF 仍未实现
- Real Git Worktree transaction binding
- Governor 自动调度与更高层 split/merge policy
- 独立物理 hot/warm/cold node store 或破坏性 cognitive GC
- Jev/Laya live validation、校准与 routing benchmark
- Go Graph Governor mutation/executor parity；Go 控制面 baseline 已覆盖 Decision Layer、Category chain、Think effort、circuit breaker、Work Unit、共享 cognition profile 与只读 Governor Analyze/Plan/Validate
- reusable Skills
- vision/browser tooling
- 新认知控制面对 Go runtime 的完整 parity

## 文档

- [System overview](docs/system-overview.md)
- [Architecture diagrams](docs/architecture-diagrams.md)
- [Architecture](docs/architecture.md)
- [Cognitive control plane](docs/cognitive-control-plane.md)
- [Attention Light algorithm](docs/attention-light-algorithm.md)
- [Agent runtime](docs/agent-runtime.md)
- [Workflow Contract](docs/workflow-contract.md)
- [Security and trust](docs/security-and-trust.md)
- [Validation](docs/validation.md)
- [Standalone readiness](docs/standalone-readiness.md)

## License

MIT


### Go 认知控制预览

Go runtime 现在可以直接读取与 Node reference runtime 相同的 `.lumencortex/cognition.json`。当设置 `LCX_COGNITION=true`、`LCX_COGNITION_PROFILE`，或 workspace 已存在 cognition profile 时，会启用 Algorithm Router、可选 Jev/Laya-compatible Decision Layer、Category 模型链、provider circuit breaker、动态 Think effort 和 Persistent Work Units。

```bash
LCX_MODEL=your-fallback-model \
LCX_BASE_URL=http://127.0.0.1:11434/v1 \
LCX_COGNITION=true \
lcx-go agent "debug the transaction path"

lcx-go governor analyze
lcx-go governor plan
lcx-go governor validate plan.json
```

Go Governor 当前刻意保持只读。持久化 Graph Governor mutation/apply 与 Cortex Epoch 执行仍由 Node reference runtime 负责，避免出现两套 Cognitive Graph 写事务 authority。


### 可选 Embedding / Hybrid Retrieval

Node runtime 已支持显式开启的 OpenAI-compatible embedding 检索：向量按内容 hash 增量持久化到 SQLite，使用 exact cosine 排序，再通过 RRF 与 Symbol/FTS 结果融合，最后仍进入 Attention 的 reliability、graph propagation 与 token budget 约束。

```bash
lcx index embeddings
lcx search "warehouse contention" --semantic
lcx search "warehouse contention" --hybrid
lcx light "why is warehouse acceptance inconsistent?" --mode hybrid --json
```

该实现是正确性优先的 exact-cosine baseline，不是 ANN。HNSW/IVF 等近似最近邻索引仍属于后续性能优化。


### LSP Rename / Code Action

LSP 已不仅用于跳转和诊断，也支持基于真实 language server 的 rename 与 edit-backed code action。返回的 WorkspaceEdit 会先进行全量路径/range/重叠校验，再按多文件原子方式应用；写入失败会回滚。

```bash
# 默认只预览
lcx lsp rename src/file.ts 12 8 NewName

# 显式确认后应用
lcx lsp rename src/file.ts 12 8 NewName --yes

lcx lsp actions src/file.ts 20 1 20 80
lcx lsp apply-action src/file.ts 20 1 20 80 0 --yes
```

LSP WorkspaceEdit 中针对文件的 create/rename/delete 已支持，并带有 overwrite/ignore 语义、工作区边界与 symlink 防护、全量预验证和失败回滚。command-backed code action 也已支持：只有用户/Agent 显式选择并执行该 action 时，language server 反向发起的 `workspace/applyEdit` 才会被接受；普通会话中的 unsolicited edit 会被拒绝。


### Go Session Worktree / Skills

Go runtime 已支持显式的 Session Git Worktree 隔离：Agent 工具、Shell、LSP 与 Repository 操作会切到该 Session 的 managed worktree；handoff/apply 前会检查目标工作区脏状态和重叠文件冲突。

```bash
lcx-go worktree attach <session-id> [base]
lcx-go worktree status <session-id>
lcx-go worktree plan <session-id>
lcx-go worktree apply <session-id> --yes
lcx-go worktree conflicts
lcx-go worktree remove <session-id> [--force]
```

Go runtime 同时已有 global/project 分层 Skills，并会把启用的 Skill 注入 Agent system prompt：

```bash
lcx-go skills list effective
lcx-go skills save project my-skill ./SKILL.md
lcx-go skills disable project my-skill
lcx-go skills enable project my-skill
lcx-go skills delete project my-skill
```

这不等同于“Cognitive Git branch 与真实 Git worktree 一一事务绑定”，也不代表 Node runtime 已有 Skills parity；这两项仍保持独立边界。


### Governor Scheduler

Node runtime 现在支持显式开启的全局 Governor 调度器。它在 Agent 成功完成并记录任务 cognition 后，根据 graph revision、cooldown、archive/canonicalization/branch/promotion backlog、tier drift 和 Cortex Epoch 信号决定是否生成新的全局维护计划。

调度器**只自动规划，不静默改图**。计划绑定生成时的 graph revision；图发生变化后旧计划会以 `GOVERNOR_PLAN_STALE` 拒绝应用。

```bash
lcx governor scheduler status
lcx governor scheduler run --force
lcx governor scheduler apply --dry-run
lcx governor scheduler apply --yes
lcx governor scheduler apply --semantic --epoch --yes
```

`governor.scheduler.useCurator` 默认关闭，因此不会自动产生 Governor 模型成本。只有显式开启后才会调用独立配置的 Governor Curator 模型。

### Node Skills

Node reference runtime 与 Go runtime 现在都支持 global/project 分层 Skills。Node 主 Agent、Subagent 和标准 CLI/TUI harness 使用同一个 effective Skill registry，并把启用的 Skill 注入模型 system working set。

```bash
lcx skills list effective
lcx skills show project my-skill
lcx skills save project my-skill ./SKILL.md
lcx skills disable project my-skill
lcx skills enable project my-skill
lcx skills delete project my-skill
```
