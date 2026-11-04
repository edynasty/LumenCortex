"""SpecPi 0.33.0 on Harbor's built-in Pi agent (BASELINE_B_SPECPI_HARNESS.md §5.3).

Harbor ships no SpecPi agent and SpecPi ships no Harbor adapter. SpecPi is an installer layered on
Pi, so this subclass runs Harbor's Pi install (pinned with `--ak version=0.87.1`), then installs the
SpecPi package and runs its installer into the same agent directory Harbor's Pi run uses
(PI_CODING_AGENT_DIR=/tmp/harbor-pi-agent). Everything else — models.json, the `pi --print --mode
json` invocation, token accounting — is Harbor's Pi code, unchanged.

The install shape follows SpecPi's own tier suite (`--yes --skip-browser-install`, SPECPI_PI pointing
at Pi) and sets the permission system's `yoloMode`, because an unattended run cannot answer the
dialog it otherwise fails closed to.

Status: NOT RUN. Two installer behaviours are unverified for 0.33.0 (§5.3): that `specpi install`
accepts `--skip-browser-install`, and that it writes into PI_CODING_AGENT_DIR. Verify both with one
install-only trial (EVAL_PLAN_TB2_DUAL_BASELINE.md §5, gate G2) before any counted run.

    PYTHONPATH=bench/adapters harbor run --agent specpi_agent:SpecPi033 --ak version=0.87.1 ...
"""

from typing import override

from harbor.agents.installed.pi import Pi
from harbor.environments.base import BaseEnvironment

SPECPI_VERSION = "0.33.0"
# Must equal harbor.agents.installed.pi._REMOTE_PI_CONFIG_DIR, where Harbor's Pi run looks.
AGENT_DIR = "/tmp/harbor-pi-agent"


class SpecPi033(Pi):
    @staticmethod
    @override
    def name() -> str:
        return "specpi"

    @override
    def version(self) -> str | None:
        return SPECPI_VERSION

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        await super().install(environment)
        # Harbor installs nvm and Node under the agent user's $HOME (node_install.py), so the SpecPi
        # package goes into that same Node, as the agent user.
        await self.exec_as_agent(
            environment,
            command=f'. "$HOME/.nvm/nvm.sh" && npm install -g --ignore-scripts specpi@{SPECPI_VERSION}',
        )
        await self.exec_as_agent(
            environment,
            command=(
                f'. "$HOME/.nvm/nvm.sh" && mkdir -p {AGENT_DIR} && '
                f"PI_CODING_AGENT_DIR={AGENT_DIR} SPECPI_PI=$(command -v pi) "
                "specpi install --yes --skip-browser-install"
            ),
        )
        await self.exec_as_agent(
            environment,
            command=(
                f"mkdir -p {AGENT_DIR}/extensions/pi-permission-system && "
                f"printf '%s' '{{\"yoloMode\":true}}' > {AGENT_DIR}/extensions/pi-permission-system/config.json"
            ),
        )
