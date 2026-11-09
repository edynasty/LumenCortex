# Deterministic capability comparison

The platform experiment compares bounded observable affordances, not LLM success rates. No external model requests are made. The reference arm is the same current runtime with the selected capability disabled or unavailable; it is not a historical Core binary and does not claim a controlled old-release benchmark.

## DAP

Four one-defect fixtures (C, Go, Python, JavaScript) compute `input-1` instead of `input+1`. Both arms receive the same source and failing executable. The output-only arm observes `FAIL` and nonzero exit; it does not guess a root cause. The DAP arm stops after initialization, checks `input=41` and `value=40`, and identifies the faulty transformation. A deterministic one-token repair is then compiled/run outside the debugger and must print PASS with exit zero. This is evidence that DAP exposes diagnostic state; source inspection could also solve these fixtures, so no general accuracy advantage is claimed. Live thread lists and closed manager sessions are recorded.

Entry: `LC_CAPABILITY_EXPERIMENT=1 go test ./internal/debugger -run TestCapabilityFourLanguageFaults -v -count=1`, with the installed adapter environment documented by the workspace raw receipt. Failures are preserved, not overwritten. The first Node attempt omitted the required adapter Language and stayed configuring; that experiment configuration error is corrected rather than counted as a program diagnosis.

## Other capability arms

- Inspector: overflow a one-slot live subscriber, then recover matching lifecycle IDs from durable history and reopen the Engine.
- Runtime debugger: run the same deterministic two-write provider normally and with step boundaries; only the latter can observe the exact first-write/second-write-absent boundary. Both final file contents remain equal.
- Hub/Worktree: four concurrent isolated writers plus a dependent integration task; compare against four barrier-synchronized direct shared-file writers and count preserved independent outputs, never claim isolation resolves Git merge conflicts.
- Eval: reuse a kernel versus close/create between dependent cells; compare computed state and explicit artifact round trips.
- Hooks: same provider and workspace policy with/without an explicit denying lifecycle hook; compare actual filesystem effects.
- Verification: same claimed success with and without a required delivery contract; compare claimed completion with independently inspected missing file/delivery gate.

Exactly these bounded local cases form the experiment denominators. Existing regression and Ubuntu cross-language receipts supplement them; they are not extra capability trials or independent model evaluations.
