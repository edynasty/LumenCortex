# Host boundary assertion (FA-07)

`evalapi.BoundaryReport` schema 1 binds cohort, job, trial, execution and request. `evalruntime.Manager.AssertBoundary` selects only its own interpreter by executable inode, managed cgroup and process group, then brackets `/proc` observation with pidfd/starttime checks. It records generation, UID/GID, namespace identities, cgroup resource settings, mount destinations/source identities/access modes, socket exposure, network policy, bwrap identity and AppArmor label. It neither executes a sandbox command to obtain proof nor accepts a worker report.

A report is an observation, not an authority token. Every enabled cell admission collects again. `BoundaryContext` in host `EvalOptions` enables this gate; model tool arguments cannot supply it. CLI hosts use `LCX_EVAL_REQUIRE_BOUNDARY=1` plus cohort/job/trial/execution IDs. Absent observations are UNKNOWN; negative observations are FAIL; either refuses before Cell side effects. A stale or mismatched report cannot pass schema validation. Reset produces a new process/generation; reports from earlier generations are not reused.

`AssertEvalBoundary` is an owner/session-authorized host API. No arbitrary PID query or DAP target is accepted. Non-Linux platforms return UNKNOWN rather than claiming equivalent controls. Explicit local development without this production gate preserves the existing contained Eval path; that path is not independent-UID isolation.

## Harbor 0.25.0

The installed `BaseEnvironment` and `DockerEnvironment` have no versioned host assertion. `UnifiedExecutionMixin` requires `boundary_schema_version=1`, a trusted `assert_boundary(binding)` method and a fully validated report before setup and before each Eval-enabled executable invocation. It also enables Core's fresh Cell gate. An old method returning None, arbitrary truthy value, container-side `exec` result or config flag is not sufficient. Eval OFF leaves canonical setup/run unchanged. No compatible Harbor host provider is claimed or installed by this change; Eval ON refuses on actual Harbor 0.25.0.

## Exercised evidence

Workspace `bench/evidence/cognitive-agent-execution-platform-20261108/raw/`:

- `boundary-linux-first.*`: real Ubuntu Python interpreter identity, cgroup, six namespace separations, mounts/socket inspection; reset changes process generation; foreign-owner query denied; same-UID Cell refused before a file write. Race enabled.
- `boundary-local-contract.*`: malformed/incomplete/stale/context-mismatched reports rejected; existing macOS identity/revocation scenario passes. CLI package compiled but selected no tests.
- `boundary-adapter-contract.*`: three rejection/opt-out contract cases.
- `boundary-harbor0250-real.*`: actual installed DockerEnvironment rejects schema v1 absence; no containers, Trials or model requests.
- `boundary-real-manager-smoke-corrected.*`: standalone real Manager runs Python `print(42)` and writes its private file with the development gate off; the same side effect is refused when the boundary is required. Both managers dispose successfully. Earlier smoke failed because its driver had not created the required workspace; the failed receipt remains preserved.

The current VM's report is **FAIL**, not deployment PASS: worker and host both have UID 1000, public bwrap is used, and `apparmor_restrict_unprivileged_unconfined=0`. Its non-empty AppArmor label only establishes observed profile attachment (`lc-bwrap-userns (unconfined)`); it does not establish caller isolation or an enforcing confinement profile. FA-01 must supply genuine identity separation and a verified deployment before an overall PASS is possible. No AppArmor or host configuration was changed for FA-07.
