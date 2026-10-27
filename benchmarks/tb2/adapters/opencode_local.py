"""Harbor's OpenCode agent with an offline install and its file watcher off.

Identical to the built-in `opencode` agent except:
- `install()`: on a glibc image it uploads the official opencode-linux-x64 binary
  (bench/build/bin/opencode-linux-x64, npm opencode-linux-x64@1.19.4) to /usr/local/bin instead of
  apt-get nodejs + npm i -g opencode-ai, which failed on this host's network. The LumenCortex arms
  likewise upload a prebuilt binary, so no arm depends on the network at setup. A musl image, or one
  without stdbuf (run() pipes through it), falls back to the built-in install.
- harness switches, set for every exec of the trial: the file watcher, autoupdate, the models.dev
  fetch and LSP downloads are off. Under amd64 emulation 1.19.4 hung at "project copy refresh" or
  after "exiting loop" with the watcher on (smoke-opencode-fixgit-3: 30 min AgentTimeout after one
  title request; debug run: task done in 13 s, then no exit until KILL); with these off the same run
  exits 0 in 22 s. None of them changes the agent's tools, prompt or model.

    PYTHONPATH=bench/adapters harbor run --agent opencode_local:OpenCodeLocal -m oi/... ...
"""

from pathlib import Path
from typing import override

from harbor.agents.installed.opencode import OpenCode
from harbor.environments.base import BaseEnvironment

BINARY = Path(__file__).resolve().parent.parent / "build" / "bin" / "opencode-linux-x64"
VERSION = "1.19.4"
HARNESS_ENV = {
    "OPENCODE_EXPERIMENTAL_DISABLE_FILEWATCHER": "1",
    "OPENCODE_DISABLE_AUTOUPDATE": "1",
    "OPENCODE_DISABLE_MODELS_FETCH": "1",
    "OPENCODE_DISABLE_LSP_DOWNLOAD": "1",
}


class OpenCodeLocal(OpenCode):
    def __init__(self, *args, extra_env: dict[str, str] | None = None, **kwargs):
        super().__init__(*args, extra_env={**HARNESS_ENV, **(extra_env or {})}, **kwargs)

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        probe = await environment.exec(
            command="[ -f /etc/alpine-release ] && echo musl; ldd --version 2>&1 | grep -qi musl && echo musl; "
            "command -v stdbuf >/dev/null || echo nostdbuf; uname -m",
            user="root",
        )
        out = probe.stdout or ""
        if "musl" in out or "nostdbuf" in out or "x86_64" not in out:
            self.logger.warning("opencode_local: falling back to the built-in install (%s)", out.strip())
            await super().install(environment)
            return
        await environment.upload_file(BINARY, "/usr/local/bin/opencode")
        await environment.exec(command="chmod 755 /usr/local/bin/opencode", user="root")
        await self.exec_as_agent(environment, command="opencode --version")

    @override
    def version(self) -> str | None:
        return VERSION
