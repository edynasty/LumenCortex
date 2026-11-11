# Persistent Eval

The platform reuses `evalapi`, `evalruntime.Manager`, the runtime registry, Python/JavaScript workers, notebook host, tool bridge, artifact store and owned-process lifecycle. No new interpreter or session manager exists. Eval remains host-opt-in and per-run opt-in.

## Execution and isolation

A kernel belongs to one LC session and one permission domain. Cells in that kernel retain language state; cross-agent grants are read-only and never authorize execution in a foreign heap. Every request revalidates access, generation, deadline and request identity. Reset destroys the heap and advances generation; crash recovery starts a fresh worker and never claims old state survived. Interrupt is request-scoped: a late request cannot interrupt a newer cell. Python supports cooperative state-preserving interruption; JavaScript interruption/timeout can discard the worker, explicitly reported as state lost.

Linux requires a delegated cgroup v2 subtree. Every worker leaf receives `memory.max`, zero swap, group OOM, `pids.max` and `cpu.max=100000 100000` before gated launch. This is one CPU per entire worker tree, in addition to the existing per-process cumulative `CPUSeconds` rlimit. Missing controllers or failed limit writes reject startup. cgroup.kill and the keeper cover descendants and owner death. macOS uses Seatbelt, process limits and monitored RSS; it is not a Linux hard-memory or CPU-quota claim. Seatbelt lets a worker signal only processes of its own sandbox (itself and its children); the host, other kernels and the user's other processes cannot be signalled.

CPU usage on a result is the execution's delta, not the lifetime counter of a persistent worker. Memory peak remains the current worker generation's kernel high-water mark; it is not an additive per-cell number. `raw/eval-linux-targeted-final.*` proves actual throttling and per-cell accounting under race instrumentation.

Output is bounded while streaming. Artifact bytes and total workspace usage are bounded independently. Filesystem and network policy are host-selected; no cell selects executable, mounts or host environment. FA-01 broker and FA-07 boundary assertion are additional Linux deployment gates, not established merely by kernel or cgroup success.

Linux hosts may select `EvalOptions.Broker` instead of an in-process Manager. This uses the same typed Host contract, permissions, artifact metadata and lifecycle operations through a process-bound Unix connection; it does not fall back on broker failure. Resource requests may only tighten administrator ceilings. Required boundary observations happen before every Cell. See [execution platform security](security/execution-platform-security.md) for the dedicated Ubuntu verification, setup and limitations.

## Artifacts and transfer

Artifacts include ID, kernel SessionID, name, MIME, confined store-relative path, size, SHA256 and creation time. Bytes are obtained only through session-authorized APIs, never through a caller-selected host path. `Engine.TransferEvalArtifact` imports workspace bytes, copies between same-owner kernels, or exports through the existing protected workspace resolver. Every transfer checks identity and source digest and journals source/destination references. PNG, CSV, JSON, Parquet, text and arbitrary bounded data files use this one byte-preserving path; format-specific readers run inside the real language kernel.

Python → Artifact → JavaScript → Artifact → Python retains the original Python heap while each hop shares only explicit bytes. Large data remains in artifacts rather than in model context. Notebook rendering continues through the existing Artifact Viewer.

## Evidence and limits

- `raw/eval-existing-integration.*`: actual macOS Python state, JS transfer, Reset, owner isolation/revocation and Eval OFF; scientific case initially skipped without an explicit interpreter.
- `raw/eval-integration-lifecycle.*`: actual language/artifact/lifecycle scenarios on macOS.
- `raw/eval-linux-resource-first.json`: eight successful Ubuntu resource, OOM, restart, descendant, scoped-interrupt and reset cases; raw delivery is retained in the session transcript.
- `raw/eval-linux-science-first.txt`: actual Ubuntu Python/SciPy/Matplotlib → JSON → JS → JSON → original Python, exported PNG/report and independent semantic/delivery checks, plus identity tests.
- `raw/eval-science-visible-trace.*`: macOS Miniconda SciPy native module fails to load (`__thread_bss` malformed section). This is a local dependency failure, not a passing scientific scenario. The verifier rejected missing output; no successful delivery was recorded. Other installed Python interpreters lack the scientific dependency set. System packages were not changed.

All providers are deterministic local fixtures. No real model requests or formal benchmark Trials are authorized or executed. Full-platform macOS regression, Desktop resource UI and dedicated Ubuntu broker/assertion integration passed with the platform limits in the final implementation report.
