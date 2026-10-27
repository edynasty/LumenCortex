"""LumenCortex arms of the dual-baseline evaluation (bench/EVAL_PLAN_TB2_DUAL_BASELINE.md §3).

`lcx_agent.py` is frozen by PREREGISTRATION_TB2_W13.sha256, so new arms live here and reuse its
`_Lcx` unchanged: same proxy, model, context/output limits, step cap and policy; an arm differs only
in the Core binary it uploads and in the X-Exp-Tag arm prefix.

    PYTHONPATH=bench/adapters harbor run --agent lcx_eval:LcxPost ...
Binaries (static linux/amd64 in bench/build/bin, built from an exact commit, never a dirty tree):
    lcx-go-Aplus  Core 4968ea8, the W13 A+ binary itself (reused, not rebuilt)
    lcx-go-post   Core 50aa96d (cog/harness-fix merged into cog/r1a: da9c5bd cut streams, ff19b89 output_limit, 50aa96d argumentless last call); plan §3.2
"""

from lcx_agent import _Lcx


class LcxPre(_Lcx):
    ARM = "LCpre"
    BINARY = "lcx-go-Aplus"


class LcxPost(_Lcx):
    ARM = "LCpost"
    BINARY = "lcx-go-post"
