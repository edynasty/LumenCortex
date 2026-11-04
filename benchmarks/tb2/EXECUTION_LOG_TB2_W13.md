# Execution log: TB2 W13 (companion to PREREGISTRATION_TB2_W13.md, which stays frozen)

- 2026-10-26/27: oracle check `jobs/oracle-w13`. raman-fitting 1.0. pytorch-model-cli 0.0, but
  the verifier's own `apt-get` received 502 from `deb.debian.org` through the proxy, so `curl`/`uvx`
  were never installed (EXTERNAL, not the task); its oracle is re-run. Five images
  (extract-elf, hf-model-inference, multi-source-data-merger, sqlite-with-gcov, tune-mjcf) did not
  pull within Harbor's 1200 s environment start; they are pulled separately with retries.
- The batch is therefore run as two Harbor jobs with the frozen `run_w13.sh` and identical
  settings: `tb2-w13a` = the 8 ready tasks (`-x` the five), then `tb2-w13b` = the five, each after
  its oracle passes. Splitting changes no trial condition.
- 2026-10-27 00:20–00:34 (local): Docker Hub pulls via the Docker Desktop proxy stalled on layer
  retries for ~45 min. The five images were pulled from the mirror `docker.1ms.run` instead, after
  checking that the mirror's manifest digest equals Docker Hub's for each (`docker buildx imagetools
  inspect`), then tagged with the original names: extract-elf `sha256:6932e4cb…`,
  hf-model-inference `sha256:f96dd7d3…`, multi-source-data-merger `sha256:8b327820…`,
  sqlite-with-gcov `sha256:f986820b…`, tune-mjcf `sha256:56c19773…`. Same bytes, different
  transport.
- pytorch-model-cli oracle re-run (`jobs/oracle-w13-pytorch-retry`): 0.0 again, and again the cause
  is the proxy, now in the solution step: `apt-get` got 502 from `deb.debian.org` via
  `192.168.65.254:7897`, so `gcc: command not found`. A container probe right after (15 fetches of a
  `.deb` via the proxy and 15 direct) returned 200 every time: the 502s come in bursts.
  Consequence for scoring: every non-solved trial is checked for `Bad Gateway` in its agent and
  verifier logs before it is classed `MODEL`.
- Oracles for the five pulled tasks and a third pytorch-model-cli attempt run as `jobs/oracle-w13b`
  (`-n 1`) while `tb2-w13a` runs (one LC trial at a time on a 10-CPU VM; no model calls).
- `jobs/oracle-w13b`: extract-elf, hf-model-inference, multi-source-data-merger, sqlite-with-gcov,
  tune-mjcf all 1.0 (no `Bad Gateway`). pytorch-model-cli 0.0 for the third time, `Bad Gateway`
  in the solution's `apt-get` again.
- Probe (pytorch-model-cli image, `apt-get install --download-only gcc`, ~80 packages): via the
  proxy 1 package 502 (`192.168.65.254:7897`); direct, with no proxy variables, also 1 package 502
  (`199.232.162.132:80`). The losses do not depend on the proxy: this host's path to
  `deb.debian.org` drops about one package in eighty under apt's load, and the oracle's `solve.sh`
  does not retry. Under the pre-registered rule pytorch-model-cli is **`ENV`, excluded from the
  primary comparison**; LC's three attempts on it are still run and reported separately (an agent
  can retry an install, the oracle script does not). Comparison set: 12 tasks.
- `tb2-w13a` finished (24 trials, 3 h 28 min, Harbor mean 0.458). Two kinds of INFRA appear:
  1. **Gateway**: 12 non-200 out of 583 requests (6×502, 6×503), each in a burst of 3 within ~10 s
     (`jobs/metering/proxy.jsonl`). Core retries an availability error twice with ≤5 s backoff and
     then fails the run, so each burst ended a run: prove-plus-comm ×2 and sanitize-git-repo ×1 after
     they had already solved the task (reward 1.0, counted as solved), sanitize-git-repo__sU9xstz and
     vulnerable-secret__75TyDtG (at step 0) before.
  2. **Verifier network**: 5 verifiers ran no test (`testsRun=0`) because their own setup could not
     install `curl`/`uv` (apt 502, `Could not resolve 'deb.debian.org'`, GitHub release download
     failure): configure-git-webserver__Vv5u3Vg, pytorch-model-cli__67LsMqP,
     sanitize-git-repo__sU9xstz, vulnerable-secret__75TyDtG, vulnerable-secret__aZAcGjG. The last
     one's agent had found the correct flag (it appears 9 times in its session) and reported writing
     it, but the verifier scored 0 without testing.
- Rule applied (pre-registered: INFRA re-run once, the re-run counts): an unsolved trial whose
  verifier ran no test, or whose run was ended by gateway non-200, is INFRA. Re-runs:
  configure-git-webserver ×1, sanitize-git-repo ×1, vulnerable-secret ×2, pytorch-model-cli ×1
  (ENV-excluded, reported separately), as `tb2-w13r*` after `tb2-w13b`. A solved trial is never
  re-run. The original trials stay in the record.
