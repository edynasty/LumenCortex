# ADR-0024 OV-04 follow-up: compressed path-notation legend

Branch `fix/adr-0024-opus-ov-remediation-20261122`, on top of `a3f19d9d98af8d0ffc59e9da3c202fb3f0e42299`. The tested `internal/toolset/editing.go` blob is `864247b3a7c73f6d85a2a5666665db343bb276fa`. Status: **`FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW`**.

## Owner decision

The Owner first asked to move the legend into the editing tool descriptions. I measured that and the Owner rejected it in favour of **option C: keep the legend in results and compress it**. Runtime scope stays unchanged.

The rejected variant is not on the branch. Its patch was kept outside the repository (`/private/tmp/lc-ov-desc/rejected-descriptions.patch`, SHA256 `a46b9b85f49137203c9a0477e73ff3da6d3a61999ac7fe1a2d5b2337f39dc163`). What it measured:

- **Contract on each of the six tools:** 526 B per tool, about 206 estimated tokens. `runtime` `TestAgentLoopEditsLinesAndRejectsAStaleEdit` (8K context, 1K output, full policy) failed with "static minimal prefix exceeds every candidate". That fixture admits about 150 B extra per path-label tool and fails at 164 B.
- **Contract once per request:** about 660 B on every request. The schema-cost surfaces in `runtime/capabilities.go` would also have needed changes (`TestToolCatalogsMatchAFullPolicyRun`: 20,989 B measured vs 18,965 B sent).
- **Payload gain** (`logs/compare-a3f19d9-vs-rejected-descriptions.txt`): +225 failed rows over 2,229 renders.

## Change

Only the five legend constants in `editing.go` changed. Selection, identity, byte accounting and the forms logic are untouched. The legend still lists only the forms a payload uses and is still counted in the 4,096-byte budget.

| Form | Before (B) | After (B) | New text |
|---|---:|---:|---|
| short | 148 | 91 | `…/ = 1+ omitted dirs; … in a name = omitted chars. Display-only labels, not file paths.` |
| count | 33 | 25 | `…{N}/ = exactly N dirs.` |
| tail | 47 | 39 | `…/./x = omitted dirs, then exactly x.` |
| exact | 58 | 34 | `./x = exactly path x, … literal.` |
| redacted | 80 | 55 | `<outside-workspace>/x = outside file named x.` |

Sizes are JSON-encoded; `<` and `>` serialize as `\u003c` and `\u003e`. The whole `"pathNotation"` member shrinks:

- short only: 166 → 109 B;
- short + count: 200 → 135 B;
- exact only: 76 → 52 B.

## Results (`logs/`)

- **Gates** (`gates.log`, recipe `tools/run-gates.sh`), all exit 0:
  - `gofmt`, `go build ./...` and `go vet ./...`;
  - toolset tests, normal and `-race`;
  - `^TestOV`: 31 PASS;
  - SOL/R7/Astra targeted run: 128 PASS;
  - Carbon probe: 19 PASS;
  - R7/R6/Resume race run: 149 PASS;
  - Windows and Linux vet (compile only);
  - `go test -timeout=45m -count=1 ./...`: 800 s, 43 packages ok, 6 without tests.
- **Oracle** (`tools/zz_ov_probe_v3_test.go.txt`, the archived probe with the new legend text): 2,229 renders and 0 problems. Every payload is at most 4,096 B.
  - Inputs are identical to the archived corpus input set.
  - Before comparing, I re-rendered the a3f19d9 baseline and confirmed it matches the archived `remediation-wire.sha256` and oracle reports.
- **Against a3f19d9** (`compare-a3f19d9-vs-compressed.txt`, `tools/compare_notation.py`):
  - 1,483 renders are identical. Across all renders, failed rows rise from 25,758 to 25,932 (+174, in 174 payloads).
  - **0 payloads lose a failed row or any row.**

| Suite | Failed rows before → after | Note |
|---|---:|---|
| 684 corpus | 7,291 → 7,330 | 39 payloads +1; legend bytes −25,655 over 453 payloads |
| 1,180 matrix | 14,515 → 14,532 | 17 payloads +1 |
| 33 filename sweep | 911 → 941 | 30 cases +1 |
| Five long-filename cases | 28/28/28/28/3 → 29/29/29/29/3 | 4,081 → 4,096 B |
| 40-file batch, 60 orders | 30 → 31 in every order | ≤ 4,096 B |
| Archived deep-32 | 30 → 31 | 4,092 B, one `partially_modified` omitted |

`legend-wire.sha256` lists the digests of all 2,229 outputs, each with its trailing newline removed. `SHA256SUMS` covers this directory.
