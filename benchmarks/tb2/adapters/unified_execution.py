"""Opt-in execution adapter mixin, composed with the approved canonical TB2 adapter.

This module never replaces the selected adapter, sandbox, ledger, model route or
Verifier. The cohort loader must include this module's hash in its admission
sources and compose ``UnifiedExecutionMixin, LcxBenchmark`` in that order.
No formal job is created by importing this module.
"""
import json
import re
from pathlib import Path


class UnifiedExecutionMixin:
    def __init__(self, *args, eval_enabled=False, eval_python=None,
                 eval_javascript=None, **kwargs):
        if not isinstance(eval_enabled, bool):
            raise TypeError("eval_enabled must be boolean")
        self.eval_enabled = eval_enabled
        self.eval_python = eval_python
        self.eval_javascript = eval_javascript
        super().__init__(*args, **kwargs)

    async def setup(self, environment):
        # Always let the canonical adapter perform its real boundary and identity
        # checks. A flag is not a sandbox attestation.
        await super().setup(environment)
        if not self.eval_enabled:
            return
        if not callable(getattr(environment, "assert_boundary", None)):
            raise RuntimeError("Eval requires the canonical verified sandbox environment")
        await environment.assert_boundary()
        for executable in (self.eval_python, self.eval_javascript):
            # None (the constructor default) and a blank value both mean "no explicit
            # interpreter": Core trims LCX_EVAL_PYTHON/LCX_EVAL_JAVASCRIPT and falls back to
            # discovering python3/node on PATH, and run() already treats a falsy value as unset.
            # Only an explicitly supplied path must be an absolute sandbox path.
            if executable is None or not executable.strip():
                continue
            if not executable.startswith("/") or "\0" in executable:
                raise ValueError("runtime executable must be an explicit absolute sandbox path")

    async def run(self, instruction, environment, context):
        if not self.eval_enabled:
            return await super().run(instruction, environment, context)
        trial_id = self.logs_dir.parent.name
        run_id = self.logs_dir.parents[1].name
        if not all(re.fullmatch(r"[A-Za-z0-9_.-]{1,180}", value) for value in (trial_id, run_id)):
            raise ValueError("invalid execution run/trial identifier")
        owner = self

        class ExecutionEnvironment:
            def __getattr__(self, name):
                return getattr(environment, name)

            async def exec(self, *args, **kwargs):
                env = kwargs.get("env")
                if env and env.get("LCX_POLICY"):
                    env = dict(env)
                    env.update(LCX_EVAL_ENABLED="1", LCX_EVENTS="1",
                               LCX_EVAL_TRIAL_ID=trial_id, LCX_EVAL_RUN_ID=run_id)
                    if owner.eval_python:
                        env["LCX_EVAL_PYTHON"] = owner.eval_python
                    if owner.eval_javascript:
                        env["LCX_EVAL_JAVASCRIPT"] = owner.eval_javascript
                    kwargs["env"] = env
                return await environment.exec(*args, **kwargs)

        # Same canonical run/collector: original model request ledger, cache
        # unknowns, actual cost and Reward are untouched.
        try:
            return await super().run(instruction, ExecutionEnvironment(), context)
        finally:
            self.logs_dir.mkdir(parents=True, exist_ok=True)
            count, omitted = 0, False
            source = self.logs_dir / "stderr.txt"
            target = self.logs_dir / "EVAL_EVENTS.jsonl"
            with target.open("w") as output:
                if source.is_file():
                    with source.open() as stream:
                        while True:
                            line = stream.readline(256 * 1024 + 1)
                            if not line:
                                break
                            if len(line) > 256 * 1024:
                                omitted = True
                                while line and not line.endswith("\n"):
                                    line = stream.readline(256 * 1024 + 1)
                                continue
                            try:
                                event = json.loads(line)
                            except (ValueError, TypeError):
                                continue
                            if isinstance(event, dict) and str(event.get("type", "")).startswith("eval."):
                                if count >= 10000:
                                    omitted = True
                                    break
                                output.write(json.dumps({"run_id": run_id, "trial_id": trial_id,
                                                         "event": event}, ensure_ascii=False) + "\n")
                                count += 1
            meta = dict(context.metadata or {})
            meta["eval"] = {"enabled": True, "run_id": run_id, "trial_id": trial_id,
                            "events": target.name, "event_count": count,
                            "coverage": "BOUNDED" if omitted else "CAPTURED",
                            "semantic_validation_is_official_reward": False,
                            "model_usage": "canonical request ledger; no independent estimate"}
            context.metadata = meta
