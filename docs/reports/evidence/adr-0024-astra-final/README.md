# SOL-REV-01 final renderer evidence

Read [the frozen initial audit](initial-audit.md) first. The original report is dated by Git history; the log files record what actually ran. All checks here are local serializer or Go checks, not model-provider, storage or native cross-platform benchmarks.

## Replay

Use a fresh scratch directory **outside the repositories**, the Opus evidence checkout for the frozen source probes, and clean checkouts of the named refs. The driver overlays test files without editing checkouts. For example, with `E` set to this evidence directory:

```bash
D=$(mktemp -d)
W=/Users/tangxingpeng/IdeaProjects/me/LumenCortex/wt
O=$W/core-adr-0024-sol-rev-01-opus-20261122
S=$W/core-adr-0024-sol-01-02-remediation-20261122
P=$W/core-adr-0024-r7r2-opus-final-20261121
python3 "$E/tools/replay.py" "$D" "$O" "$P" old 10f135d1f904ab88c6a086319ca5820ea6b9e09c render
python3 "$E/tools/replay.py" "$D" "$O" "$S" sol 3d85d06e31ae8b424b2ed3d0b9b94be70627e5ac render
python3 "$E/tools/replay.py" "$D" "$O" "$O" candidate 52ac7d7df7d582bc2cb95e36618381cf30f7ab80 render
python3 "$E/tools/replay.py" "$D" "$O" "$O" candidate 52ac7d7df7d582bc2cb95e36618381cf30f7ab80 witness
python3 "$E/tools/check.py" "$D" "$O" old sol candidate
```

`render` independently regenerates all 684 corpus, 1,180 matrix, 33 sweep and eight additional permutation/literal captures for each version. The probe creates real literal paths only inside that run's scratch workspace. `witness` also captures every **compact** label, rather than inferring it from the few rows the payload shows. It uses the archived four-shape directory fixture plus the escaped-joint, multiple-literal, expensive-literal and blocked-row witnesses. Historical witnesses not using the index can also be rendered with the archived `TestReviewRenderCases` probe on the same captured input. For `candidate`, `oracle` replays the archived maximality checker: its Go process exits 0 even for `CHECK` records, so inspect the findings. It flags J5; J3 is marked `ok` despite `ambStrict=1`. Both are corrected on the Astra branch.

`check.py` reads serialized bytes (stripping only the generator's single output-file newline), decodes UTF-8 strictly, checks size/state/omission invariants, compares every checked-in byte/row/failed/omitted table entry, and resolves labels against the full set of normalized paths. Whole-component and looser cross-slash wildcard readings are checked separately. A `NotOne` count is a count of unresolved **or** ambiguous rows, not duplicate strings. A sanitized redaction collision cannot provide an identifiable distinct raw path. The original five and all 40-file permutations, actual wire outputs and input bytes are in the witness archives. `*-wire.sha256` hashes JSON payloads without file framing. The corpus input-set hash covers sorted `(filename, NUL, SHA256(file bytes))`; it includes 684 metadata and 666 receipt files. Cases without receipts are not silently dropped.

For synthetic timing, run `bench` for each renderer sequentially with the same generated `candidate/in`; `scale` covers 100/1,000/50,000 middle-distinguished attempts and start/end IDs at 1,000. The driver records the exact command, actual source blob and exit in the raw log. The three corpus samples use one complete corpus per iteration, `GOMAXPROCS=2`, `getrusage(user+system)`, prebuilt inputs and Go `-benchmem`. Divide cumulative allocations and bytes by 684. Scale uses wall `ns/op`, not process CPU. CPU and latency are host-load sensitive; these are not throughput guarantees.

## Final code

Set `T` to a checkout at the **tested code SHA**, not the documentation-only delivery tip. Reuse `D/candidate` for the exact input bytes. `selected` obtains the renderer's chosen compact labels; it does not determine which prefixes fit. `check.py` independently constructs and sizes the JSON prefix plus the state-specific omissions, then also resolves each actual output label under three readings. The suffix reference probe uses a separate pairwise reference. All outputs are outside the checkouts.

```bash
C=52dfd64f56d9b46dfbe7293c787b0f883a501717
for mode in render witness selected suffix blocked bench scale; do
  python3 "$E/tools/replay.py" "$D" "$O" "$T" astra "$C" "$mode" || exit 1
done
python3 "$E/tools/check.py" "$D" "$O" astra
python3 "$E/tools/delta.py" "$D" "$D/changes.tsv"
python3 "$E/tools/bench-summary.py" "$D" astra
```

- [Complete raw final gate log](logs/astra-final-gates.log): exact commands, code SHA, RED with only `editing.go` overlaid, GREEN with the same tests, full-repository tests, race and exit codes.
- [Independent wire checks](logs/astra-wire-check.log), [per-case deltas](changes.tsv), [all final wire digests](astra-wire.sha256), [selected inputs/outputs/labels](astra-witnesses.tar.gz).
- [Additional validation findings](followup.md), [64/256/1,024 blocked rows before](logs/b1-blocked.log) and [after](logs/astra-blocked.log). The interrupted intermediate gate log is historical, not a pass on the last commit.
- [Historic branch/worktree checks](logs/history-check.log). The raw replay scratch was `/private/tmp/lc-carbon-final.jQjddm`, outside version control; the manifests, representative actual bytes, scripts, metrics and logs here are versioned. `SHA256SUMS` is verified from the repository root; it excludes itself.

The release gate is independent review of the changed renderer, not an automatic merge or approval of the pending architecture specification.
