# Execution platform security

## Linux broker implementation contract

FA-01 is a narrow privileged launcher around the existing Eval Manager. Core retains LC sessions, permission, callbacks and provenance; each authenticated connection owns an existing Manager and cgroup leaves. Root maps a separate non-login worker UID. The RPC accepts no executable, shell-command operation, environment, namespace or mount parameters. Python/JS runtime IDs and resource ceilings are administrator policy; clients may only tighten limits. Cell code remains arbitrary untrusted code, executed only after sandbox admission.

A Unix socket authenticates SO_PEERCRED UID/GID/PID and process starttime against a root-issued, expiring one-use grant bound to cohort/job/trial/execution. Request nonces are unique per connection (concurrent requests may arrive out of order); replay is rejected. Consumed grants cannot reconnect. Disconnect, peer death, expiry and grant revocation dispose the Manager. Broker SIGKILL is covered by the independent cgroup keeper. Worker Shell/Python/JS receive neither the grant nor broker socket, namespace FD or privileged credentials; fd 6 is closed before interpreter exec. Host Shell remains the unprivileged Core user's process and cannot execute root-only bwrap or authenticate as the grant's distinct PID.

Worker launch: root-owned broker creates a small pinned user namespace with only UID/GID 0 and the selected worker identity mapped. Bubblewrap constructs private mounts/namespaces, then a fixed `setpriv` transition clears supplementary groups, all capabilities and the bounding set, sets no-new-privileges, and execs the interpreter. A fixed seccomp filter blocks host-process inspection, namespace creation/join, mounts, kernel keyring/BPF/perf and reboot; ordinary fork/thread creation remains available. The whole tree is in the existing bounded cgroup before bwrap runs. The root worker-data parent and immutable artifacts remain inaccessible to worker identities; only the kernel data tree is owned by its assigned UID.

This mapping was chosen after real Ubuntu bubblewrap 0.9.0 probes: passing nonzero --uid/--gid directly failed after setuid removed the capability needed by subsequent setgid; pre-setting GID then failed network setup. A fixed post-namespace drop passed, showing real host UID 65534, empty supplementary groups, all capabilities zero and NoNewPrivs=1. These exploratory probes did not install a broker or establish production security. Raw receipts: `broker-*probe`, `broker-map-*`, `broker-post-namespace-drop` in the task evidence directory.

## Administration and integration

Build `cmd/lcx-broker` on Linux. Root-owned, non-shared-writable absolute policy paths configure `socket`, `root`, `grants`, `python`, `javascript`, `bubblewrap`, non-root `workerUid`/`workerGid` and `budget`. Broker bwrap must be root-only; all executable and directory ancestors are validated. One active client connection is supported; its Manager permits the configured bounded kernel concurrency. This deliberately is not a general multi-tenant service.

- `lcx-broker serve /absolute/policy.json`
- `lcx-broker grant /absolute/policy.json <trusted-client-pid> <cohort> <job> <trial>`: prints a one-use process-bound grant ID, expires after 30 minutes.
- `lcx-broker revoke /absolute/policy.json <grant-id>`: removes authorization; active connections are checked every 100 ms.
- Core host uses `EvalOptions.Broker`; CLI uses `LCX_EVAL_BROKER_SOCKET` and `LCX_EVAL_BROKER_GRANT`. The latter is unset after loading. No environment-selected fallback to local execution when broker connection fails.
- An exclusive process-held lock prevents duplicate service ownership and permits recovery of a root-owned stale socket after SIGKILL. Grants are not resurrected; a new grant is required.
- Dispose returns the remote cleanup result before closing the connection. An EOF without acknowledgment is an error, not cleanup proof.

## Verified dedicated Ubuntu matrix

Evidence root: workspace `bench/evidence/cognitive-agent-execution-platform-20261108/raw/`.

- `broker-core-both-languages`, `broker-crash-restart-fixed`: actual Core Engine, Python retained state/subprocess, Node and all required host assertions; no model requests.
- `broker-security-matrix-fd-fixed`, `broker-security-matrix-race`: real denied namespace/ptrace/public-bwrap/network/socket/privileged-FD access; foreign session/artifact denial; four kernels and rejected fifth; Python interruption with heap retained; JS timeout/restart; Reset identity change; 640 MiB allocation under 128 MiB hard limit and unaffected bystander; acknowledged Dispose. Race-instrumented broker exited 0 with no race report.
- `broker-wire-security`, `broker-foreign-peer`, `broker-duplicate-service-refusal`: nonce and consumed-grant replay, unknown privileged parameters, shell operation, widened budget, wrong live peer PID and duplicate broker refused.
- `broker-revoke`, `broker-client-kill`, `broker-crash`, `broker-keeper-crash`: explicit revoke, client SIGKILL, broker SIGKILL and keeper SIGKILL. Host observed worker/leaf or scope disappearance after terminal paths; keeper replacement preserved next-cell operation.
- `broker-budget-mount-regression`, `broker-focused-race`: resource-policy and proc self-bind negative regression tests.
- `broker-vm-restored`, `broker-restored-active-profiles`, `broker-final-owned-process-cleanup`: original two profiles and sysctls restored, no owned broker/worker process remained.

Failure receipts remain immutable: namespace FD 6 leaked to the interpreter before the fix; Node could not traverse synthetic `/etc`; root data ancestors were inaccessible after UID drop; proc control self-binds were incorrectly refused; absent ToolCaller was incorrectly serialized as present; SIGKILL left a stale socket before lock-based recovery. These are fixed in this candidate, not erased from the evidence.

## Limits and independent gates

Verified platform is Ubuntu ARM64 with systemd/cgroup v2 and bubblewrap 0.9.0. AppArmor profile was a root-only-bwrap userns exception, not a general syscall sandbox; mounts, seccomp, credentials and cgroups provide the other boundaries. The experiment temporarily removed public-bwrap exceptions and enabled `apparmor_restrict_unprivileged_unconfined`; original VM settings are restored. No production policy or service was installed. macOS/Windows do not offer this broker; no cross-platform security PASS. Host-process attacks against the Core account and administrator account compromise are not covered by the worker isolation proof. Final unified regression, independent Opus review and production installation remain distinct gates.
