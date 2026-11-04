"""Throwaway probe agent: validates Harbor's --agent import-path contract.

Not a benchmark agent. Used only to prove that a module outside the Harbor
package can be imported by `harbor run --agent <module>:<Class>` via PYTHONPATH,
and that AgentContext fields reach result.json.
"""

from pathlib import Path
from typing import override

from harbor.agents.base import BaseAgent
from harbor.agents.capabilities import AgentCapabilities
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext


class ProbeAgent(BaseAgent):
    capabilities = AgentCapabilities()

    @staticmethod
    @override
    def name() -> str:
        return "probe"

    @override
    def version(self) -> str | None:
        return "0.0.1"

    @override
    async def setup(self, environment: BaseEnvironment) -> None:
        await environment.exec(command="echo probe-setup-ok", user="root")

    @override
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        result = await environment.exec(command="uname -m && pwd")
        (self.logs_dir / "probe.txt").write_text(
            f"instruction_len={len(instruction)}\n"
            f"stdout={result.stdout}\n"
            f"model={self.model_name}\n"
        )
        context.n_input_tokens = 11
        context.n_output_tokens = 7
        context.cost_usd = 0.0
        context.metadata = {"probe": "ok", "arch": (result.stdout or "").strip()}
