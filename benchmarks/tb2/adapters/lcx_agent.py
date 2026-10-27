"""Harbor installed-binary adapter for the LumenCortex Core CLI (`lcx-go agent`).

One subclass per arm; each uploads a prebuilt static linux/amd64 `lcx-go` (bench/build/bin) and runs
one non-interactive agent session in the task's working directory. Nothing is written inside the
task workspace: the global configuration lives under XDG_CONFIG_HOME and Core's state under
XDG_STATE_HOME, both in /tmp/lcx. Model calls go to the host metering proxy (127.0.0.1:18080, reached
as host.docker.internal), which holds the real key; the X-Exp-Tag header names arm and trial. After
the run the state directory and the CLI output are copied into the trial's agent log directory.

    PYTHONPATH=bench/adapters harbor run --agent lcx_agent:LcxA ...
"""

import json
import shlex
from pathlib import Path
from typing import override

from harbor.agents.base import BaseAgent
from harbor.agents.capabilities import AgentCapabilities
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

BIN_DIR = Path(__file__).resolve().parent.parent / "build" / "bin"
ROOT = "/tmp/lcx"
PROXY = "http://host.docker.internal:18080/v1"
MODEL = "deepseek-v4.1-flash"
# Same for every arm: the cognitive arms differ only in the Core binary.
CONTEXT_LIMIT = 600_000
OUTPUT_LIMIT = 32_768
MAX_STEPS = 150


class _Lcx(BaseAgent):
    capabilities = AgentCapabilities()
    ARM = ""
    BINARY = ""

    @staticmethod
    @override
    def name() -> str:
        return "lcx-go"

    @override
    def version(self) -> str | None:
        return self.ARM

    def _tag(self) -> str:
        # logs_dir is <job>/<trial>/agent; the trial directory names the task and the attempt.
        return f"{self.ARM}/{self.logs_dir.parent.name}"

    @override
    async def setup(self, environment: BaseEnvironment) -> None:
        binary = BIN_DIR / self.BINARY
        if not binary.is_file():
            raise FileNotFoundError(f"missing arm binary {binary}")
        await environment.exec(
            command=f"mkdir -p {ROOT}/xdg/lumencortex {ROOT}/state && chmod -R 777 {ROOT}",
            user="root",
        )
        await environment.upload_file(binary, f"{ROOT}/lcx-go")
        await environment.exec(command=f"chmod 755 {ROOT}/lcx-go", user="root")

    async def _workspace(self, environment: BaseEnvironment) -> str:
        res = await environment.exec(command="pwd")
        ws = (res.stdout or "").strip().splitlines()[-1] if res.stdout else ""
        if not ws.startswith("/"):
            raise RuntimeError(f"cannot determine the task working directory: {res}")
        return ws

    def _config(self, workspace: str) -> dict:
        model = {
            "modelID": MODEL,
            "limit": {"context": CONTEXT_LIMIT, "output": OUTPUT_LIMIT},
            "reasoning": {"format": "reasoning-effort", "efforts": ["low", "medium", "high", "max"],
                          "echo": "reasoning_content"},
        }
        return {
            "model": f"local/{MODEL}",
            "trustedWorkspaces": [workspace],
            "providers": {"local": {
                "name": "local", "package": "openai-compatible",
                "settings": {"baseURL": PROXY, "apiKey": "{env:LCX_EXP_KEY}",
                             "timeoutSeconds": 3600, "idleTimeoutSeconds": 600,
                             "headers": {"X-Exp-Tag": self._tag()}},
                "models": {MODEL: model},
            }},
        }

    @override
    async def run(self, instruction: str, environment: BaseEnvironment, context: AgentContext) -> None:
        workspace = await self._workspace(environment)
        local = self.logs_dir
        local.mkdir(parents=True, exist_ok=True)
        (local / "lumencortex.json").write_text(json.dumps(self._config(workspace), indent=1))
        (local / "instruction.md").write_text(instruction)
        await environment.upload_file(local / "lumencortex.json", f"{ROOT}/xdg/lumencortex/lumencortex.json")
        await environment.upload_file(local / "instruction.md", f"{ROOT}/instruction.md")
        await environment.exec(command=f"chmod -R a+rwX {ROOT}", user="root")
        env = {
            "XDG_CONFIG_HOME": f"{ROOT}/xdg",
            "XDG_STATE_HOME": f"{ROOT}/state",
            "XDG_CACHE_HOME": f"{ROOT}/cache",
            "LCX_EXP_KEY": "dummy-exp-key",
            "LCX_POLICY": "full",
            "LCX_MAX_STEPS": str(MAX_STEPS),
        }
        command = (
            f"cd {shlex.quote(workspace)} && "
            f"{ROOT}/lcx-go agent \"$(cat {ROOT}/instruction.md)\" "
            f"> {ROOT}/result.json 2> {ROOT}/stderr.txt; echo \"exit=$?\" > {ROOT}/exit.txt"
        )
        try:
            await environment.exec(command=command, env=env)
        finally:
            await self._collect(environment, context)

    async def _collect(self, environment: BaseEnvironment, context: AgentContext) -> None:
        out = self.logs_dir
        for name in ("result.json", "stderr.txt", "exit.txt"):
            try:
                await environment.download_file(f"{ROOT}/{name}", out / name)
            except Exception as exc:  # the run may have been cut before the file existed
                (out / f"{name}.missing").write_text(repr(exc))
        try:
            await environment.download_dir(f"{ROOT}/state", out / "state")
        except Exception as exc:
            (out / "state.missing").write_text(repr(exc))
        meta = {"arm": self.ARM, "tag": self._tag()}
        result = out / "result.json"
        if result.is_file():
            try:
                parsed = json.loads(result.read_text())
                usage = parsed.get("usage") or {}
                meta["status"] = parsed.get("status")
                meta["sessionId"] = parsed.get("sessionId")
                meta["usage"] = usage
                context.n_input_tokens = usage.get("promptTokens")
                context.n_output_tokens = usage.get("completionTokens")
            except (ValueError, AttributeError) as exc:
                meta["parseError"] = repr(exc)
        exit_file = out / "exit.txt"
        if exit_file.is_file():
            meta["exit"] = exit_file.read_text().strip()
        context.metadata = meta


class LcxA(_Lcx):
    ARM = "A"
    BINARY = "lcx-go-A"


class LcxAplus(_Lcx):
    ARM = "A+"
    BINARY = "lcx-go-Aplus"


class LcxAfix(_Lcx):
    ARM = "A+fix"
    BINARY = "lcx-go-Afix"
