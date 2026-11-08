import asyncio
import copy
from datetime import datetime, timezone, timedelta
from pathlib import Path
import tempfile
import unittest

from boundary import REQUIRED_CHECKS, require_boundary
from unified_execution import UnifiedExecutionMixin


class Canonical:
    def __init__(self, logs_dir):
        self.logs_dir = logs_dir
        self.started = False

    async def setup(self, environment):
        self.started = True


class Candidate(UnifiedExecutionMixin, Canonical):
    pass


class BoundaryAdmissionTests(unittest.TestCase):
    def test_unversioned_environment_is_refused_before_setup(self):
        class LegacyEnvironment:
            async def assert_boundary(self):
                raise AssertionError("legacy method must not be used as proof")
        with tempfile.TemporaryDirectory() as root:
            agent = Candidate(Path(root) / "job" / "trial" / "agent", eval_enabled=True, eval_cohort_id="offline")
            with self.assertRaisesRegex(RuntimeError, "schema v1 unavailable"):
                asyncio.run(agent.setup(LegacyEnvironment()))
            self.assertFalse(agent.started)

    def test_incomplete_forged_and_stale_reports_cannot_authorize(self):
        binding = dict(cohortId="c", jobId="j", trialId="t", executionId="e", requestId="r")
        complete = dict(schemaVersion=1, context=binding, checkedAt=datetime.now(timezone.utc).isoformat(),
                        status="PASS", process=dict(hostPid=42, starttime=100, generation=1, uid=1001, gid=1001, cgroup="/owned"),
                        checks={name: dict(status="PASS", evidence="observed") for name in REQUIRED_CHECKS})
        variants = []
        r = copy.deepcopy(complete); del r["checks"]["mounts"]; variants.append(r)
        r = copy.deepcopy(complete); r["checks"]["callerIsolation"]["status"] = "FAIL"; variants.append(r)
        r = copy.deepcopy(complete); r["checks"]["identity"]["status"] = "UNKNOWN"; variants.append(r)
        r = copy.deepcopy(complete); r["context"]["requestId"] = "old"; variants.append(r)
        r = copy.deepcopy(complete); r["checkedAt"] = (datetime.now(timezone.utc)-timedelta(minutes=1)).isoformat(); variants.append(r)
        r = copy.deepcopy(complete); r["process"]["hostPid"] = True; variants.append(r)
        r = copy.deepcopy(complete); r["schemaVersion"] = 2; variants.append(r)
        for report in variants:
            with self.subTest(report=report), self.assertRaises(RuntimeError):
                require_boundary(report, binding)

    def test_eval_off_does_not_require_linux_boundary(self):
        with tempfile.TemporaryDirectory() as root:
            agent = Candidate(Path(root) / "job" / "trial" / "agent")
            asyncio.run(agent.setup(object()))
            self.assertTrue(agent.started)


if __name__ == "__main__":
    unittest.main()
