# Agent Hub

Hub extends the existing `RunSupervisor`, Task State work-unit DAG and repository worktree owner. It does not run a background scheduler or merge worktrees.

## Host APIs

- `Engine.ConfigureHub(ctx, root, []HubTaskSpec)` seeds an idle root's graph exactly once. Each spec provides ID, goal, dependencies and optional task-eligible agent. Core validates the DAG and creates execution proposals. Caller-supplied completion or verification is not accepted.
- `RunSupervisor.Hub(ctx, root)` derives task states and bounded worker views from durable sessions, work units, verification and source fences.
- `RunSupervisor.StartProposedTask` admits only satisfied dependencies and uses existing resource/concurrency limits. Writing workers use distinct worktrees. Direct child restart is subject to the same dependency admission.
- `Engine.SendWorkerMessage(ctx, root, sender, recipient, id, body)` admits only root/direct-worker membership. IDs are idempotent for identical content; conflicting reuse is rejected.
- `Engine.WorkerMessages(ctx, root, reader, after, limit, receive)` returns cursor pages. Root host inspection can view the timeline without acknowledging an inbox; agent reads are recipient-only. An acknowledgement means read, not execution or verification.

Agent `worker_send_message` and `worker_inbox` bind sender/root to the current run and cannot choose authority. Inbox results carry source provenance. Messages never directly modify recipient Task State or grant permissions.

## Storage and privacy

Migration 0012 stores inboxes in the existing session database. Bodies are UTF-8, secret-masked, at most 8192 bytes; IDs at most 128 bytes; at most 1000 messages per root. Retention exhaustion rejects new messages rather than silently losing unreceived data. Queries return at most 100 messages. Fenced sources return a withdrawn envelope without body; privacy purge removes all rows involving a deleted root/sender/recipient. Desktop holds only the latest 200 rows, revalidates that window on every poll and clears message content when reads fail.

## Control and results

Linked execution workers expose the existing safe-boundary Agent debugger. Pause waits for an active operation to finish and affects only that worker. Cancel uses `CancelSubtree`; it does not cancel sibling runs. Completion is derived from actual child execution, required checks and work-unit outcomes. Events include assignment, blocked/completed/result-ready and message sent/received metadata; message bodies are not event payloads.

Desktop's Inspector embeds Hub, task dependency rows and message timeline. Selecting a worker binds Inspector/debug controls; opening its thread exposes the existing transcript and artifact surfaces. Core remains authoritative for admission, resource budgets and verification. Worktree integration continues through the existing verified handoff API; no automatic main merge exists.

## Exercised evidence

Workspace evidence `bench/evidence/cognitive-agent-execution-platform-20261108/raw/`: `hub-regression.json` (Core/session race, four concurrent workers, dependency gating, idempotence, membership, restart/fence/privacy); `hub-real-ui.json` (real Desktop backend/Core over a temporary localhost transport, four isolated writers, paused/continued worker, message and fifth dependent worker); `hub-message-revalidation.json` (UI withdrawn/error refresh). These use deterministic local providers and zero external model requests. Native Wails build/self-check and full-platform macOS regression passed in the final implementation report; native interactive acceptance remains separate.
