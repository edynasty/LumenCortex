# Data model

## Graph state

```json
{
  "version": 1,
  "nodes": {},
  "edges": {},
  "metadata": {}
}
```

## Node

Common fields:

```json
{
  "id": "node_...",
  "kind": "belief",
  "title": "Submission does not deduct inventory",
  "body": "...",
  "tags": [],
  "status": "active",
  "trustZone": "model_inferred",
  "grade": "static",
  "createdAt": "...",
  "updatedAt": "...",
  "version": 1,
  "metadata": {}
}
```

Optional fields:

- `evidenceIds[]`
- `childIds[]`
- `unresolved[]`
- `source`
- `observedAt`
- `ttlMs`
- `contentHash`
- `sourceVersion`
- `validity`

## Edge

```json
{
  "id": "edge_...",
  "from": "node_a",
  "to": "node_b",
  "type": "depends_on",
  "weight": 0.9,
  "createdAt": "...",
  "metadata": {}
}
```

Supported v0.1 relation types:

```text
depends_on
derived_from
relates_to
supersedes
abstracts
invalidates
contradicts
verifies
causes
calls
affects
```

## Evidence grades

The grade is ordinal provenance, not an LLM probability:

| Grade | Meaning |
|---|---|
| hypothesis | unverified reasoning |
| static | supported by source/static evidence |
| tested | supported by a test |
| runtime | observed in actual runtime state |
| reproduced | independently reproduced |

## Belief integrity

A `belief` or `negative` node with grade above `hypothesis` must cite evidence.

An `evidence` node cannot use the `model_inferred` trust zone.

Evidence expiration or content changes mark dependent cognition `stale`.

## Cognitive commit

```json
{
  "id": "...",
  "message": "...",
  "parents": ["..."],
  "createdAt": "...",
  "graphHash": "...",
  "diff": { "operations": [] },
  "metadata": {},
  "snapshot": {}
}
```

Diff operations carry before/after objects, which makes strict precondition checking and inversion possible.
