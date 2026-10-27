# Terminal-Bench Open-Weights Side-by-Side Runs

This directory contains trace archives and summaries for full Terminal-Bench
89-task runs comparing Cline, OpenCode, and Pi-Code across open-weight models.

## Groups

- `cline-non`: Cline with reasoning/thinking off. Kimi is omitted because its
  OpenRouter endpoint requires reasoning.
- `cline-medium`: Cline with medium reasoning/thinking.
- `opencode-medium`: OpenCode with medium reasoning/thinking.
- `pi-medium`: Pi-Code with medium reasoning/thinking.

## Run Configuration

These are Cline team runs on Terminal-Bench 2.0, using the full 89-task set and
pass@1 scoring. Runs used OpenRouter model routes, Modal execution,
`--timeout-multiplier 2.0`, and concurrency `n=20`. The runs were launched on
June 24-25, 2026.

Harness versions:

- Cline CLI `3.0.29`. The first Cline matrix tarball was
  `cline-cli-linux-x64-3.0.29-main-218db385-20260623T235704Z.tgz`. Some later
  Cline reruns used a newer `3.0.29` tarball from commit `14a28b055`; see
  `provenance/` for per-archive source metadata.
- OpenCode `1.17.9`.
- Pi-Code `@mariozechner/pi-coding-agent` `0.73.1`.

OpenCode and Pi-Code were installed from their then-current npm `latest`
packages at run time. These are not official benchmark numbers from OpenCode or
Pi-Code.

## Files

- `summary.md`: readable score table.
- `summary.csv`: machine-readable score table.
- `summary.json`: score table with archive hashes and sizes.
- `run-configs.md`: readable harness configuration table.
- `run-configs.csv`: machine-readable harness configuration table.
- `runs/`: non-LFS per-run manifests, representative Harbor configs, and
  trial-level result indexes.
- `archives/`: Git LFS ZIP archives, one per group/model run.
- `provenance/`: original source-package metadata and archive manifests.
- `scripts/verify-archives.py`: verifies archive SHA-256 and file sizes.

## Notes

The traces are intended for transparency and inspection, not as a minimal
reproduction package. They preserve the benchmark run artifacts that were
available in the sanitized export.

`summary.*` reports the pass score and an `unscored` count. `unscored` means the
included trial `result.json` did not expose a verifier reward of `0.0` or `1.0`;
inspect that trial directly before labeling it as an infra error.

OpenCode token accounting requires care: visible output and reasoning tokens
can be reported separately by the harness. Use normalized reports when comparing
completion-side token mass.
