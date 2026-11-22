# ADR-0024 OV-01..OV-06 remediation evidence

Branch `fix/adr-0024-opus-ov-remediation-20261122`, based on Astra `a5a1ec6be1081bad4e706944d655a9a9d97f4f70`. The tested code is commit `7e0610fa86bdf0c8001187b4d4985b601603d104` (`editing.go` blob `d470f49bef3fcafbd070a006b5902e4c5dc40fb7`), on top of `11d3222e2a134e705ae9a173e156553ec15be11e`. Production scope is `internal/toolset/editing.go`. Every OV finding is **`FIXED_BY_IMPLEMENTER / SELF_VERIFIED / PENDING_INDEPENDENT_REVIEW`**.

## What changed

| Finding | Change |
|---|---|
| OV-01 | Every overflow size check uses the exact serialized envelope, including `filesOmitted`, `filesOmittedByState` and `pathNotation`. `omissionBytes` is checked against `json.Marshal` in a unit test |
| OV-02 | Owner-suffix maxima are indexed once per owners map, using the nearest differently-owned neighbour in reverse-byte order. Root-filtered owner maps use a per-basename root index. Labels are unchanged; unit tests compare both against brute force |
| OV-03 | A counted gap `root/…{n}/tail` is not emitted when another attempted path ends its pre-tail component with `{n}`. `pathNotation` also defines the precedence of the counted form |
| OV-04 | A payload gains `pathNotation` when it shows notation, listing exactly the forms present: shortened labels, counted gaps, literal tails, `./` exact paths, redacted literal names. The text is counted in the byte budget |
| OV-05 | `truncated` means removed content. The reversible `./` qualifier and redaction no longer set it |
| OV-06 | Overflow rows are selected per priority class (failed, verified, other) by an exact DP over publish states. It picks the largest count that fits, then the lexicographically smallest set by sanitized path, and renders in priority/original order. At most 256 renderable candidates per class are considered. Lower classes use only leftover budget, up to the rich view's 32-row limit. `selectRows` is checked against exhaustive enumeration |

**Supported optimality.** The selection is exact for the row count, given each row's compact label and the legend chosen by the bounded form iteration. It is not a global optimum over every possible label encoding, nor over candidates beyond the 256-row window.

## Results (`logs/`)

- **Gates** (`gates.log`): `gofmt`, `go build ./...` and `go vet ./...` pass. Toolset tests pass normally and under `-race`. `^TestOV` has 31 PASS. The SOL/R7/Astra targeted run has 128 PASS. The Carbon probe has 19 PASS lines. The R7/R6/Resume race run has 149 PASS. Windows and Linux were checked by vet only. `go test -timeout=45m -count=1 ./...` exits 0 in 773 s: 43 packages ok, 6 without tests. Native Windows, Linux and FreeBSD are not verified.
- **RED** with the base `editing.go`: `red-stage-a-counted-gap.log` exits 1. In `red-stage-b.log`, 10 of the 12 behavioural tests fail. The units file needs new helpers and is excluded.
- **Independent oracle** (`tools/zz_ov_probe_test.go.txt`): 1,545 suite renders and 684 corpus payloads, holding 52,891 labels. It found **0 problems**: exact identity under four typed readings, state/omission accounting, legend text equal to the emitted forms, `truncated` set if and only if content was removed, the 4,096-byte bound, and every overflow failed count inside independent bounds. The naive in-component reading finds no wrong-target label; Z4 passes.
- **Differential against Astra** (`corpus-compare.txt`, `suites-compare.txt`):
  - Corpus input set: `599d1d87074bf79b00df3976efa3b12addc75609b1acb832e404cd74c2399897`.
  - Corpus (684): 229 identical, 104 legend only, 207 with extra lower-class rows, 38 with fewer failed rows (legend bytes), 51 with fewer lower-class rows, 55 other changes at equal counts.
  - Matrix (1,180): 1,130 identical. In the other 50 there is never a wrong identity.
- **Notable cases:**
  - Five long-filename cases: 28, 28, 28, 28 and 3 rows. The legend costs about 164 B, so one row fewer than Astra.
  - 40-file batch: 30 rows at 4,095 B in all 60 orders, so the selected set no longer depends on order. With the legend, the 32 cheapest rows need 4,234 B.
  - I8: 34 failed rows (Astra 20).
  - The impossible first row now shows the later verified row.
  - The metadata band never shows 0 rows.
- **Performance** (`bench-*.log`, median of 3, `GOMAXPROCS=2`, same machine and inputs):
  - Adversarial oversize: 7,846 → 266 ms at 1,024 rows and 1,843 → 128 ms at 512.
  - Realistic workloads stay linear. `longname_middle` at 1,024 rises from 37 to 103 ms, because labels are computed for up to 256 candidates. Other workloads change by between −18% and +42%.

## Replay

Overlay `tools/zz_ov_probe_test.go.txt`, plus the frozen `zz_review_adv_cases` and `zz_review_adv_common` probes from `docs/reports/evidence/adr-0024-sol-rev-01/inputs/probes-v2/`, as files in `internal/toolset`, then run with `OV_OUT=<dir>`:

```bash
go test -overlay overlay.json -run '^(TestOVSuites|TestOV40Costs)$' ./internal/toolset
```

For the corpus, run the frozen `TestReviewCorpusDump`, then `TestOVCorpus` with `OV_CORPUS_IN`, `OV_CORPUS_OUT` and `OV_CORPUS_REPORT`. `tools/run-gates.sh` and `tools/run-bench.sh` contain the exact gate and benchmark commands. `remediation-wire.sha256` lists all output digests, and `SHA256SUMS` covers this directory.
