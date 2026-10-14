# LumenCortex Core Agent Instructions

These instructions apply to all work in the LumenCortex Core repository.

## Repository role

LumenCortex Core is the authoritative coding-agent harness/runtime.

Core owns:
- execution lifecycle and state;
- tool orchestration;
- persistence and migrations;
- recovery and concurrency semantics;
- context/window construction;
- provider abstraction and model execution contracts;
- evaluation and runtime evidence;
- repository/worktree services;
- public contracts consumed by Desktop and CLI.

Desktop-specific presentation and Wails UI behavior do not belong in Core unless a public Core contract requires them.

## Architecture authority

For the current architecture migration, the approved target design is:

`../LumenCortex-architecture-audit/design/README.md`

Treat that document as the architectural contract when it is available in the workspace.

Do not reopen settled architecture decisions during implementation unless current code, tests, or runtime evidence shows a concrete contradiction or blocker. If that occurs, isolate the contradiction and resolve only the affected decision.

Migration PR scope is strict:
- implement only the active migration item;
- do not start later migration items opportunistically;
- pair Core/Desktop changes only where the approved migration plan requires it;
- remove replaced behavior in the same PR unless the design explicitly says otherwise;
- do not keep parallel old/new implementations behind speculative flags.

## Harness-first architecture priority

LumenCortex is a coding-agent harness/runtime first, not a model-specific integration project.

Architecture and implementation work MUST stay centered on generic harness mechanics:
- execution state;
- tool orchestration;
- context construction;
- persistence;
- checkpoints;
- crash/cancel recovery;
- concurrency and idempotency;
- evaluation and observability;
- provider abstraction;
- Core/Desktop boundaries.

Provider-specific behavior may be investigated only when it reveals a concrete compatibility requirement for the generic harness. In that case:
- reduce it to the smallest provider-adapter capability or protocol contract;
- keep provider-specific details behind the adapter boundary;
- use the provider as a conformance fixture/example, not as the architectural center;
- do not reshape Task State, recovery, orchestration, routing, or persistence around one model's quirks;
- stop provider-specific research once the generic contract and required regression/conformance test are clear.

## Implementation discipline

Agents MUST:
- inspect relevant code and tests before editing;
- preserve established architecture unless the active task explicitly changes it;
- make the smallest coherent change that fully solves the task;
- prefer root-cause fixes over downstream masking;
- preserve unrelated user changes;
- avoid duplicate implementations and abandoned scaffolding;
- add or update regression tests for behavior changes;
- verify failure behavior, not only happy paths;
- report exact validation evidence and any unverified claims.

Agents MUST NOT:
- expand the task into adjacent migration items;
- introduce speculative abstractions for future work;
- weaken tests merely to make a change pass;
- silently change public contracts;
- persist secrets for convenience;
- add model/provider special cases to generic runtime state without an explicit adapter contract;
- claim runtime validation that was not actually executed.

## Artifact-led iterative work

Long implementation, architecture, audit, and migration work SHOULD be artifact-led and iterative rather than split into rigid research and implementation phases.

Use this loop:

```text
Question → Evidence → Decision → Write/Implement → Verify → Next question
```

Reading, designing, implementation, and targeted verification may interleave.

Rules:
- every non-trivial investigation SHOULD be tied to a concrete implementation decision, failing test, design question, or active artifact;
- once enough evidence exists, update code/tests/artifacts before starting another broad investigation;
- do not accumulate a large batch of research before producing durable progress;
- re-read sources only when earlier evidence is insufficient, stale, contradicted, or needed for an exact reference;
- after compaction/resume, continue from the current artifact, explicit decisions, tests, and open questions instead of restarting broad discovery;
- repeated reading is a loop when it does not materially change code, tests, an explicit decision, a blocker, or the active artifact;
- when uncertainty remains but does not block implementation, record a narrow VERIFY/TODO instead of postponing the entire deliverable.

Progress means durable code, tests, artifacts, or explicit decisions. Token use, repeated verification, and repeated compaction do not count as progress.

## Reasoning and recovery boundary

Reasoning is ephemeral. Observable state is persistent.

Hidden model reasoning MUST NOT become durable runtime or recovery state. This includes:
- thinking;
- redacted_thinking;
- chain-of-thought;
- scratchpads;
- internal notes;
- equivalent provider-private reasoning fields.

Agents and runtime implementations MUST NOT inspect, reconstruct, summarize, replay, migrate, or persist hidden reasoning for cross-session recovery.

Cross-session continuation MUST use observable evidence only:
- explicit Task State and TODOs;
- visible messages;
- tool calls and results;
- source files and artifacts;
- Git state;
- tests;
- checkpoints;
- explicit decisions and evidence.

If a provider protocol requires opaque reasoning/echo data during an active tool-use flow, keep it provider-private and opaque. It MUST NOT become task memory, generic runtime state, or a recovery source.

Changes to persistence, compaction, handoff, context construction, or recovery MUST include regression coverage proving hidden reasoning is not serialized into durable recovery state or reloaded as recovery context.

## Runtime correctness

For lifecycle, persistence, recovery, and concurrency changes:
- define terminal-state behavior for every exit path;
- preserve cancellation semantics;
- make crash/restart behavior explicit;
- do not leave partially finalized runs silently recoverable as success;
- protect shared state with the approved ownership/locking model;
- test observable invariants across restart, cancellation, failure, and retry where relevant.

Do not infer correctness from implementation shape alone; use executable regression tests.

## Durable reports and handoffs

For substantial architecture/design reviews, implementation milestones, security audits, migration acceptance, and cross-repository handoffs, agents MUST write and maintain a readable Markdown report in the project files. A chat-only completion report is insufficient.

- Use the existing canonical document when one exists. Otherwise use a descriptive path such as `docs/reports/<task>-<date>.md` (repository-specific work) or `../LumenCortex-architecture-audit/design/handoffs/<task>-<date>.md` (cross-repository design). Do not create redundant competing sources of truth.
- Start the document when the task begins and update it as verified evidence, decisions, and progress become available; do not wait until the final chat response.
- Record scope and exact baseline/commits; status (planned/implemented/validated/blocked); findings with severity and evidence; decisions (distinguish proposed from owner-approved); changes and affected files; tests actually run and results; unverified/platform limits; unresolved risks; next actions and handoff dependencies. Omit sections that are genuinely inapplicable.
- Link to source code, tests, authoritative design and verification artifacts rather than copying large sections. Never put credentials, hidden model reasoning, raw private agent traces, or secret-bearing log output in reports.
- Keep the report in sync with corrections. Final chat output SHOULD be a concise summary with the exact report path, commit state, and open blockers, not a substitute for the document.
- When the report is inside a Git repository, include it in the task's commit without staging unrelated changes. If its directory is not version-controlled, disclose that limitation explicitly and retain the exact local path.
- A brief chat summary is sufficient for trivial questions and tiny edits unless the user explicitly requests a document.

## Required validation

At minimum, for affected Core Go code run the relevant focused tests during development, then:

```bash
go test -count=1 ./...
go build ./...
go vet ./...
```

If the active migration item names stricter or additional checks in the approved design, those checks are mandatory.

When a test cannot be run in the current environment, report that limitation explicitly rather than treating it as passed.

## Definition of done

A change is complete only when:
- the active task scope is fully implemented;
- relevant regression tests exist and pass;
- affected build/vet checks pass;
- public behavior matches the approved design;
- unrelated changes are preserved;
- no later migration item was pulled in accidentally;
- remaining risks and deliberately deferred work are stated precisely.
