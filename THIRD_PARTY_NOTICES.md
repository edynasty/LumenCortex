# Third-party notices

LumenCortex Core contains text derived from third-party open-source work. This file records it.

## oh-my-pi (`@oh-my-pi/pi-coding-agent`)

- Project: oh-my-pi (the `pi` coding agent)
- License: MIT
- Version the derived text was taken from: 18.12.6
- Copyright lines as published:
  - Copyright (c) 2025 Mario Zechner
  - Copyright (c) 2025-2026 Can Bölük
  - Copyright (c) 2026 Stencil Labs, Inc.

Derived files in this repository (rewritten for LumenCortex's tools, authority model and markdown
output; no license text is embedded in the prompts themselves):

- `config/presets/reviewer.md` — adapted from `src/prompts/agents/reviewer.md`.
- `config/presets/security-reviewer.md` — adapted from `src/prompts/agents/security-reviewer.md`.
- `config/presets/sonic.md` — adapted from `src/prompts/agents/task.md` (the prompt oh-my-pi's
  `sonic` agent reuses).
- `config/presets/thread-historian.md` — adapted from `src/prompts/agents/seance.md`.
- `runtime/subagents.go` (`explorePrompt`) — adapted from `src/prompts/agents/scout.md`.
- `runtime/tasks.go` (`workerPrompt`) — adapted from `src/prompts/agents/task.md`.

The full license follows.

```text
MIT License

Copyright (c) 2025 Mario Zechner
Copyright (c) 2025-2026 Can Bölük
Copyright (c) 2026 Stencil Labs, Inc.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
