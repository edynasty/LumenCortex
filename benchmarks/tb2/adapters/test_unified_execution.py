import asyncio
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

from unified_execution import UnifiedExecutionMixin


class Environment:
    def __init__(self):
        self.calls = []
        self.checked = 0

    async def assert_boundary(self):
        self.checked += 1

    async def exec(self, *args, **kwargs):
        self.calls.append((args, kwargs))
        return SimpleNamespace(stdout="")


class Canonical:
    def __init__(self, logs_dir):
        self.logs_dir = logs_dir

    async def setup(self, environment):
        await environment.assert_boundary()

    async def run(self, instruction, environment, context):
        await environment.exec(command="frozen-core agent", env={"LCX_POLICY": "full", "LCX_EXP_KEY": "synthetic-only"})
        context.metadata = {"usage_ledger": {"cache_hit_tokens": None, "actual_cost": None}, "reward": None}


class Candidate(UnifiedExecutionMixin, Canonical):
    pass


class ExecutionFlagTests(unittest.TestCase):
    def test_disabled_preserves_canonical_environment_and_unknown_accounting(self):
        with tempfile.TemporaryDirectory() as root:
            env, context = Environment(), SimpleNamespace(metadata=None)
            agent = Candidate(Path(root) / "job" / "trial" / "agent")
            asyncio.run(agent.run("synthetic", env, context))
            self.assertEqual(env.calls[0][1]["env"], {"LCX_POLICY": "full", "LCX_EXP_KEY": "synthetic-only"})
            self.assertNotIn("eval", context.metadata)
            self.assertIsNone(context.metadata["usage_ledger"]["cache_hit_tokens"])

    def test_enabled_binds_trial_retains_ledger_and_records_bounded_events(self):
        with tempfile.TemporaryDirectory() as root:
            logs = Path(root) / "job-1" / "trial-1" / "agent"
            logs.mkdir(parents=True)
            (logs / "stderr.txt").write_text("x" * (300 * 1024) + "\n" + json.dumps({"type":"eval.ExecutionCompleted", "sessionId":"s", "data":{"requestId":"r"}}) + "\n")
            env, context = Environment(), SimpleNamespace(metadata=None)
            agent = Candidate(logs, eval_enabled=True, eval_python="/usr/bin/python3")
            asyncio.run(agent.setup(env))
            asyncio.run(agent.run("synthetic", env, context))
            self.assertEqual(env.checked, 2)
            launch = env.calls[0][1]["env"]
            self.assertEqual(launch["LCX_EVAL_TRIAL_ID"], "trial-1")
            self.assertEqual(launch["LCX_EVAL_RUN_ID"], "job-1")
            self.assertEqual(launch["LCX_EVAL_ENABLED"], "1")
            self.assertIsNone(context.metadata["usage_ledger"]["actual_cost"])
            self.assertIsNone(context.metadata["reward"])
            self.assertEqual(context.metadata["eval"]["coverage"], "BOUNDED")
            row = json.loads((logs / "EVAL_EVENTS.jsonl").read_text())
            self.assertEqual(row["event"]["data"]["requestId"], "r")
            self.assertNotIn("synthetic-only", (logs / "EVAL_EVENTS.jsonl").read_text())

    def test_rejects_unsafe_flag_and_executable(self):
        with tempfile.TemporaryDirectory() as root:
            with self.assertRaises(TypeError):
                Candidate(Path(root), eval_enabled="false")
            with self.assertRaises(ValueError):
                asyncio.run(Candidate(Path(root), eval_enabled=True, eval_python="python3").setup(Environment()))
            with self.assertRaises(ValueError):
                asyncio.run(Candidate(Path(root), eval_enabled=True, eval_javascript="node").setup(Environment()))

    def test_blank_interpreter_is_unset_not_an_invalid_path(self):
        # Core trims LCX_EVAL_PYTHON/LCX_EVAL_JAVASCRIPT and discovers python3/node on PATH, and run()
        # already treats a falsy value as unset: a blank optional interpreter must not fail setup.
        with tempfile.TemporaryDirectory() as root:
            logs = Path(root) / "job-2" / "trial-2" / "agent"
            env, context = Environment(), SimpleNamespace(metadata=None)
            agent = Candidate(logs, eval_enabled=True, eval_python="", eval_javascript="")
            asyncio.run(agent.setup(env))
            asyncio.run(agent.run("synthetic", env, context))
            launch = env.calls[0][1]["env"]
            self.assertNotIn("LCX_EVAL_PYTHON", launch)
            self.assertNotIn("LCX_EVAL_JAVASCRIPT", launch)
            self.assertEqual(launch["LCX_EVAL_ENABLED"], "1")
            # A whitespace-only value is unset for Core too: setup must not refuse it.
            asyncio.run(Candidate(Path(root), eval_enabled=True, eval_python="  ").setup(Environment()))


if __name__ == "__main__":
    unittest.main()
