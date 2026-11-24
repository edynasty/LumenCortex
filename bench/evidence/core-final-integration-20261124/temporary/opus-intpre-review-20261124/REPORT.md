# INT-PRE-REG-01 independent review (Claude Opus 5.5, read-only)

Target: fix/runtime-int-pre-reg-01-carbon-remediation-20261124 @ 2852b56023088c3c33b2678af0c8dd7debff06e4 (local = origin). Base ef10ca7c8e8031e75fdf27f449924b8254f0e2bb = origin/main = local main. Worktree clean. Owner AGENTS.md SHA256 84bb06cd73eb37997966e2d2e3cb8e8b181b27f97ff367d209f327bee818d1a0 unchanged.

Verdict: PASS_WITH_LIMITATIONS -> INT_PRE_REG_01_INDEPENDENTLY_VERIFIED (focused scope; not final integration acceptance).

Diff: 13 files; only Go file runtime/context_space_t3_test.go (+421/-25 in 316e99b); 12 evidence files in 2852b56. Zero production, SQL, schema changes. Only removed assertion is old T6-3 OutcomeUnknown expectation; T6-1/T6-2/T6-4 assertions retained and strengthened.

Semantics: execution.go:822-843 classifies a non-dirty uncommitted file-tool receipt with complete coverage as OutcomeFailed; DirtyFailure or uncommitted known effects (receiptHasDirtyOrUncommittedPublish:171-184) force OutcomeUnknown. sealFilesMutation:991-1002 and classifyExecutionSummary transcript.go:747-763 return unknown for every non-completed file-tool outcome; ADR-0024 lines 69-70 define this conservative no-attestation rule. Same combination already existed on main for T5 and T6-4a. Thus OutcomeFailed + MutationUnknown + FileEdgeCount=0 is valid: outcome=definite failure, MutationUnknown=no authoritative mutation attestation (not proof of mutation), edges=no observed derived content. Downstream effect is conservative: repair stage label unknown_mutated/postGateUnknown=1, no floor advance (transcript.go:1105 requires post-block Proven).

Independent runs (isolated git archive, GOFLAGS empty, GOWORK=off, CGO=1, go1.26.5 darwin/arm64): gofmt exit0 empty; focused T6 exit0; T3B exit0 9 subtests PASS; T3B race exit0. Full Core not run.

Independent mutants (real-path overlays proven applied by canary): o1 clean abort->uncertain killed L673; o2 refused cleanup->clean killed L770; o3 post-swap close->clean killed L804; o4 target corruption before abort killed L645; o5 re-block acknowledges unknown progress killed L700; o6 failed file tool->None killed L676. Non-vacuity probe: after the follow-up repair, LatestRev=RevOf(repaired) and Diff(orig->repaired) succeeds while Diff(orig->next) remains CodeUnknownRev; therefore T6-3a LatestRev/Diff assertions are discriminating.

Evidence: 11/11 SHA256SUMS OK; referenced 04-test-all.stdout SHA eda4550c... and integration report SHA 00eec92b... match.

Findings: OPUS-INTPRE-EV-01 P3 evidence/doc: commissioned report path docs/reports/runtime-int-pre-reg-01-carbon-2026-11-24.md absent from 2852b56; equivalent content in bench/evidence/.../README.md. Non-blocking; owner Opus; remediation not authorized in this review. Observation (not finding): Carbon m3/m4 die at T6-1, not new T6-3 cases; independent o2/o3/o5 close that gap. T6-3d uncertain cold-resume does not assert disk/displaced bytes; non-blocking because T6-3c covers the same path warm.

Limitations: full Core suite and both merged Race gates still unrun on candidate; inherited L2/L3/late hard-link/native ext4/Windows/FreeBSD/NFS-FUSE limits unchanged.
