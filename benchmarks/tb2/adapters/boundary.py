"""Host-only FA-07 schema. Reports are observations, never worker-issued grants."""
from datetime import datetime, timezone

SCHEMA_VERSION = 1
REQUIRED_CHECKS = ("identity", "generation", "cgroup", "credentials", "user", "pid",
                   "mnt", "net", "ipc", "uts", "mounts", "sockets", "networkPolicy",
                   "bubblewrap", "apparmor", "callerIsolation")


def require_boundary(report, binding):
    if not isinstance(report, dict) or report.get("schemaVersion") != SCHEMA_VERSION:
        raise RuntimeError("Eval boundary UNKNOWN: unsupported or absent schema")
    if report.get("context") != binding:
        raise RuntimeError("Eval boundary FAIL: cohort/job/trial/execution/request mismatch")
    try:
        checked = datetime.fromisoformat(report["checkedAt"].replace("Z", "+00:00"))
        age = (datetime.now(timezone.utc) - checked).total_seconds()
    except (KeyError, ValueError, TypeError, AttributeError) as exc:
        raise RuntimeError("Eval boundary UNKNOWN: invalid observation time") from exc
    if not -1 <= age <= 5:
        raise RuntimeError("Eval boundary FAIL: stale observation")
    checks = report.get("checks")
    if not isinstance(checks, dict):
        raise RuntimeError("Eval boundary UNKNOWN: missing observations")
    for name in REQUIRED_CHECKS:
        check = checks.get(name)
        if not isinstance(check, dict) or check.get("status") != "PASS" or not isinstance(check.get("evidence"), str) or not check["evidence"].strip():
            raise RuntimeError(f"Eval boundary refused: {name} is FAIL/UNKNOWN")
    process = report.get("process")
    if not isinstance(process, dict):
        raise RuntimeError("Eval boundary UNKNOWN: missing process identity")
    for name in ("hostPid", "starttime", "generation", "uid"):
        if type(process.get(name)) is not int or process[name] <= 0:
            raise RuntimeError(f"Eval boundary UNKNOWN: invalid {name}")
    if type(process.get("gid")) is not int or process["gid"] < 0 or not process.get("cgroup"):
        raise RuntimeError("Eval boundary UNKNOWN: missing cgroup or GID")
    if report.get("status") != "PASS":
        raise RuntimeError("Eval boundary refused: aggregate is not PASS")


async def assert_host_boundary(environment, binding):
    # Harbor 0.25.0 BaseEnvironment/DockerEnvironment expose no such contract.
    # No container-side `exec`, flag, label or cached JSON can stand in for it.
    if getattr(environment, "boundary_schema_version", None) != SCHEMA_VERSION:
        raise RuntimeError("Eval boundary UNKNOWN: host assertion schema v1 unavailable (Harbor 0.25.0 is unsupported for Eval ON)")
    assertion = getattr(environment, "assert_boundary", None)
    if not callable(assertion):
        raise RuntimeError("Eval boundary UNKNOWN: trusted host collector unavailable")
    report = await assertion(binding)
    require_boundary(report, binding)
    return report
