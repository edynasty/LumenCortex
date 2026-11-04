# Terminal-Bench 2.0 evaluation records (TB2)

**Benchmark Status: NOT RUN** for everything planned here (dual baseline, DSML diagnostic). The only real-model
runs on record are the W13 batches of LC arm A+ (Core `4968ea8`), authorized by O-6 and frozen by
`PREREGISTRATION_TB2_W13.md`.

This directory is the versioned copy of the workspace evaluation directory
`/Users/tangxingpeng/IdeaProjects/me/LumenCortex/bench` (outside any Git repository). Paths inside the documents
are written relative to that workspace (`bench/...`); the layout below matches it one to one. The scripts run
from the workspace directory, because they need the raw jobs and binaries that are not committed. Change a file
here first and copy it to the workspace; `shasum -a 256 -c MANIFEST.sha256` run in `bench/` shows whether the
two copies agree.

| File | Role |
|---|---|
| `HARNESS_INVESTIGATION_2026-10-27.md` | Harness investigation of the W13 evidence, Core fixes `da9c5bd`/`ff19b89`/`50aa96d`, pre-merge review (§9) |
| `EVAL_PLAN_TB2_DUAL_BASELINE.md` | Evaluation plan: arms, fixed conditions, gates, cost, DSML diagnostic, required authorization |
| `baselines/BASELINE_A_DEEPSEEK_OFFICIAL.md`, `baselines/raw/deepseek/` | Baseline A: DeepSeek's published numbers and semantics; source snapshots with `MANIFEST.csv` (URL, fetch time, hash) |
| `baselines/BASELINE_B_SPECPI_HARNESS.md`, `baselines/raw/specpi/` | Baseline B: SpecPi harness comparison; source snapshots with `MANIFEST.csv` |
| `public/` | Public per-task data for the 13 tasks (SpecPi, Cline, tbench.ai) and its raw sources |
| `RESULTS_TEMPLATE_TB2_DUAL_BASELINE.md` | Results report to fill after an authorized run |
| `PREREGISTRATION_TB2*.md`, `*.sha256` | Frozen preregistrations and the hashes of the files they freeze |
| `EXECUTION_LOG_TB2_W13.md`, `HARBOR_SETUP.md`, `HARBOR_REMEDIATION_REPORT_2026-10-27.md`, `evidence/` | W13 execution log, Harbor install/oracle validation, Harbor remediation and its evidence |
| `adapters/`, `analysis/`, `run_*.sh`, `collect_w13.py`, `export_w13_evidence.py` | Harbor agent adapters, offline analysis, launch scripts; `run_controlled.sh` and `analysis/dsml_replay.py --execute` refuse to run without `EVAL_AUTHORIZED=1` |
| `MANIFEST.sha256` | SHA-256 of every committed file in this directory except this README and the two manifests |
| `ARTIFACTS.sha256` | SHA-256 of the files that are not committed: the W13 jobs (`jobs/tb2-w13a`, `tb2-w13b`, `tb2-w13r-*`, `metering`; SQLite `-shm` files excluded), the binaries (`build/bin`, `build/bin-host`), the exported dataset (`tb2/`, from `harbor download terminal-bench@2.0`), and the four source snapshots larger than 1 MB |

The raw benchmark records stay in the workspace and are never edited; `ARTIFACTS.sha256` (taken 2026-10-28)
lets a later reader check that they are unchanged.
